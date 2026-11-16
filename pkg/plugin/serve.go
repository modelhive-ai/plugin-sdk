package plugin

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
)

const (
	HTTPProcessPath     = "/v1/plugin/process"
	HTTPOutputCheckPath = "/v1/plugin/output/check"
	HTTPObservePath     = "/v1/plugin/observe"
	HTTPAPIPath         = "/v1/plugin/api/"
	HTTPAdminHeader     = "X-Sundelion-Plugin-Admin"
	HTTPTokenHeader     = "X-Sundelion-Plugin-Token"

	httpSignatureHeader = "X-Sundelion-Signature"
	httpTimestampHeader = "X-Sundelion-Timestamp"
	httpMaxBodyBytes    = int64(1 << 20)
	httpSignatureSkew   = 5 * time.Minute
)

// NewHTTPHandler exposes one configured runtime Handler over the public JSON
// protocol. secret authenticates every call with HMAC-SHA256. It is primarily
// useful to hosts embedding a plugin; standalone binaries normally call Serve.
func NewHTTPHandler(d Definition, in Instance, secret string) (http.Handler, error) {
	if d.New == nil {
		return nil, errors.New("plugin: definition has no runtime factory")
	}
	if secret == "" {
		return nil, errors.New("plugin: HTTP HMAC secret is required")
	}
	handler, err := d.New(context.Background(), in)
	if err != nil {
		return nil, fmt.Errorf("plugin: construct runtime handler: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc(HTTPAPIPath, func(w http.ResponseWriter, r *http.Request) {
		api, ok := handler.(APIHandler)
		if !ok {
			writeHTTPError(w, http.StatusNotImplemented, "admin API not implemented")
			return
		}
		ctx := r.Context()
		if header := r.Header.Get(HTTPTokenHeader); header != "" {
			var token APIToken
			if !decodeIdentity(header, &token) {
				writeHTTPError(w, http.StatusUnauthorized, "invalid token identity")
				return
			}
			ctx = WithToken(ctx, &token)
		} else {
			var admin Admin
			if !decodeIdentity(r.Header.Get(HTTPAdminHeader), &admin) {
				writeHTTPError(w, http.StatusUnauthorized, "invalid admin identity")
				return
			}
			ctx = WithAdmin(ctx, &admin)
		}
		forward := r.Clone(ctx)
		urlCopy := *r.URL
		forward.URL = &urlCopy
		forward.URL.Path = "/" + strings.TrimPrefix(r.URL.Path, HTTPAPIPath)
		forward.URL.RawPath = ""
		api.ServeHTTP(w, forward)
	})
	mux.HandleFunc(HTTPProcessPath, func(w http.ResponseWriter, r *http.Request) {
		processor, ok := handler.(Processor)
		if !ok {
			writeHTTPError(w, http.StatusNotImplemented, "processor not implemented")
			return
		}
		var request Request
		if !decodeHTTPJSON(w, r, &request) {
			return
		}
		ops, err := processor.Process(r.Context(), &request)
		if err != nil {
			writeHTTPError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeHTTPJSON(w, http.StatusOK, ops)
	})
	mux.HandleFunc(HTTPOutputCheckPath, func(w http.ResponseWriter, r *http.Request) {
		guard, ok := handler.(OutputGuard)
		if !ok {
			writeHTTPError(w, http.StatusNotImplemented, "output guard not implemented")
			return
		}
		var window OutputWindow
		if !decodeHTTPJSON(w, r, &window) {
			return
		}
		if window.SessionID == "" {
			writeHTTPError(w, http.StatusBadRequest, "session_id is required")
			return
		}
		verdict, err := guard.CheckOutput(r.Context(), &window)
		if err != nil {
			writeHTTPError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeHTTPJSON(w, http.StatusOK, verdict)
	})
	mux.HandleFunc(HTTPObservePath, func(w http.ResponseWriter, r *http.Request) {
		observer, ok := handler.(Observer)
		if !ok {
			writeHTTPError(w, http.StatusNotImplemented, "observer not implemented")
			return
		}
		var exchange Exchange
		if !decodeHTTPJSON(w, r, &exchange) {
			return
		}
		if err := observer.Observe(r.Context(), &exchange); err != nil {
			writeHTTPError(w, http.StatusBadGateway, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeHTTPError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		writeHTTPJSON(w, http.StatusOK, map[string]string{"status": "ok", "plugin": d.Ref})
	})
	return authenticateHTTP([]byte(secret), recoverHTTP(mux)), nil
}

// recoverHTTP turns a plugin panic into a 500 so one bad request cannot drop the connection.
func recoverHTTP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recover() != nil {
				writeHTTPError(w, http.StatusInternalServerError, "plugin panicked")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// Serve starts a standalone plugin server. It reads its instance and transport
// settings from the environment injected by Modelhive:
//
//   - SUNDELION_PLUGIN_LISTEN: unix:///absolute/plugin.sock or https://host:port
//   - SUNDELION_PLUGIN_HMAC_SECRET: per-instance request signing secret
//   - SUNDELION_PLUGIN_INSTANCE: JSON-encoded Instance
//   - SUNDELION_PLUGIN_SECRETS: optional JSON object used by Instance.Secrets
//   - SUNDELION_PLUGIN_TLS_CERT_FILE / _KEY_FILE: server certificate for HTTPS
//   - SUNDELION_PLUGIN_TLS_CLIENT_CA_FILE: optional CA requiring client mTLS
//   - SUNDELION_PLUGIN_GATEWAY_URL: optional gateway base URL; when set,
//     Instance.Resources calls back to /api/v1/plugin-resources/ there,
//     authenticated with ResourceToken(SUNDELION_PLUGIN_HMAC_SECRET)
//   - SUNDELION_PLUGIN_GATEWAY_CA_FILE: optional CA to trust for the gateway
//
// It blocks until the server stops and returns the serving error.
func Serve(d Definition) error {
	listenURL, err := parseListenURL(os.Getenv("SUNDELION_PLUGIN_LISTEN"))
	if err != nil {
		return err
	}
	instance, err := instanceFromEnvironment()
	if err != nil {
		return err
	}
	hmacSecret := os.Getenv("SUNDELION_PLUGIN_HMAC_SECRET")
	if gatewayURL := os.Getenv("SUNDELION_PLUGIN_GATEWAY_URL"); gatewayURL != "" {
		hc, err := gatewayHTTPClient(os.Getenv("SUNDELION_PLUGIN_GATEWAY_CA_FILE"))
		if err != nil {
			return err
		}
		instance.Resources, err = NewResourceClient(gatewayURL, ResourceToken(hmacSecret), hc)
		if err != nil {
			return err
		}
		instance.Host, err = NewHostClient(gatewayURL, ResourceToken(hmacSecret), hc)
		if err != nil {
			log.Printf("plugin: host callback unavailable: %v", err)
		}
	}
	handler, err := NewHTTPHandler(d, instance, hmacSecret)
	if err != nil {
		return err
	}

	server := &http.Server{
		Addr:              listenURL.Host,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	if listenURL.Scheme == "unix" {
		listener, err := net.Listen("unix", UnixSocketPath(listenURL.Path))
		if err != nil {
			return fmt.Errorf("plugin: listen on unix socket: %w", err)
		}
		defer listener.Close()
		return server.Serve(listener)
	}

	certFile := os.Getenv("SUNDELION_PLUGIN_TLS_CERT_FILE")
	keyFile := os.Getenv("SUNDELION_PLUGIN_TLS_KEY_FILE")
	if certFile == "" || keyFile == "" {
		return errors.New("plugin: HTTPS serving requires TLS certificate and key files")
	}
	if clientCAFile := os.Getenv("SUNDELION_PLUGIN_TLS_CLIENT_CA_FILE"); clientCAFile != "" {
		pemBytes, err := os.ReadFile(clientCAFile)
		if err != nil {
			return fmt.Errorf("plugin: read mTLS client CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pemBytes) {
			return errors.New("plugin: mTLS client CA file contains no certificates")
		}
		server.TLSConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
			ClientAuth: tls.RequireAndVerifyClientCert,
			ClientCAs:  pool,
		}
	}
	return server.ListenAndServeTLS(certFile, keyFile)
}

// gatewayHTTPClient builds the callback client, trusting caFile in addition
// to the system roots when given.
func gatewayHTTPClient(caFile string) (*http.Client, error) {
	hc := &http.Client{Timeout: 30 * time.Second}
	if caFile == "" {
		return hc, nil
	}
	pemBytes, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("plugin: read gateway CA: %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, errors.New("plugin: gateway CA file contains no certificates")
	}
	hc.Transport = &http.Transport{
		Proxy:           http.ProxyFromEnvironment,
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool},
	}
	return hc, nil
}

func parseListenURL(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, errors.New("plugin: SUNDELION_PLUGIN_LISTEN is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("plugin: parse listen URL: %w", err)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("plugin: listen URL must not contain user info, query, or fragment")
	}
	switch u.Scheme {
	case "unix":
		if u.Host != "" || !path.IsAbs(u.Path) {
			return nil, errors.New("plugin: unix listen URL must be unix:///absolute/path.sock")
		}
	case "https":
		if u.Host == "" || (u.Path != "" && u.Path != "/") {
			return nil, errors.New("plugin: HTTPS listen URL must be https://host:port")
		}
	default:
		return nil, errors.New("plugin: listen URL scheme must be unix or https")
	}
	return u, nil
}

func instanceFromEnvironment() (Instance, error) {
	var in Instance
	raw := os.Getenv("SUNDELION_PLUGIN_INSTANCE")
	if raw == "" {
		return in, errors.New("plugin: SUNDELION_PLUGIN_INSTANCE is required")
	}
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		return in, fmt.Errorf("plugin: decode instance: %w", err)
	}
	var secrets map[string]string
	if rawSecrets := os.Getenv("SUNDELION_PLUGIN_SECRETS"); rawSecrets != "" {
		if err := json.Unmarshal([]byte(rawSecrets), &secrets); err != nil {
			return in, fmt.Errorf("plugin: decode instance secrets: %w", err)
		}
	}
	in.Secrets = func(name string) (string, error) {
		value, ok := secrets[name]
		if !ok {
			return "", fmt.Errorf("plugin: secret %q is unavailable", name)
		}
		return value, nil
	}
	return in, nil
}

func authenticateHTTP(secret []byte, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodPost && !strings.HasPrefix(r.URL.Path, HTTPAPIPath) {
			w.Header().Set("Allow", http.MethodPost)
			writeHTTPError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		limit := httpMaxBodyBytes
		if strings.HasPrefix(r.URL.Path, HTTPAPIPath) {
			limit = 10 << 20
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
		if err != nil {
			writeHTTPError(w, http.StatusBadRequest, "could not read request")
			return
		}
		if int64(len(body)) > limit {
			writeHTTPError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		if err := verifyHTTPSignature(r, body, secret, time.Now()); err != nil {
			writeHTTPError(w, http.StatusUnauthorized, err.Error())
			return
		}
		next.ServeHTTP(w, r)
	})
}

func verifyHTTPSignature(r *http.Request, body, secret []byte, now time.Time) error {
	timestamp := r.Header.Get(httpTimestampHeader)
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return errors.New("invalid signature timestamp")
	}
	delta := now.Sub(time.Unix(seconds, 0))
	if delta > httpSignatureSkew || delta < -httpSignatureSkew {
		return errors.New("signature timestamp outside allowed skew")
	}
	provided, ok := strings.CutPrefix(r.Header.Get(httpSignatureHeader), "sha256=")
	if !ok {
		return errors.New("invalid signature scheme")
	}
	var expected string
	if strings.HasPrefix(r.URL.Path, HTTPAPIPath) {
		adminHeader, tokenHeader := r.Header.Get(HTTPAdminHeader), r.Header.Get(HTTPTokenHeader)
		if (adminHeader == "") == (tokenHeader == "") {
			return errors.New("need one signed admin or token identity")
		}
		expected = apiHTTPSignature(secret, timestamp, r.Method, httpRequestTarget(r.URL), body, adminHeader, tokenHeader)
	} else {
		expected = httpSignature(secret, timestamp, r.Method, r.URL.EscapedPath(), body)
	}
	if !hmac.Equal([]byte(provided), []byte(expected)) {
		return errors.New("signature mismatch")
	}
	return nil
}

func httpSignature(secret []byte, timestamp, method, requestPath string, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = io.WriteString(mac, timestamp+"\n"+method+"\n"+requestPath+"\n")
	_, _ = mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func apiHTTPSignature(secret []byte, timestamp, method, target string, body []byte, adminHeader, tokenHeader string) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = io.WriteString(mac, timestamp+"\n"+method+"\n"+target+"\n"+adminHeader+"\n"+tokenHeader+"\n")
	_, _ = mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func httpRequestTarget(u *url.URL) string {
	target := u.EscapedPath()
	if u.RawQuery != "" {
		target += "?" + u.RawQuery
	}
	return target
}

func decodeHTTPJSON(w http.ResponseWriter, r *http.Request, out any) bool {
	if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		writeHTTPError(w, http.StatusUnsupportedMediaType, "content type must be application/json")
		return false
	}
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(out); err != nil {
		writeHTTPError(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeHTTPError(w, http.StatusBadRequest, "JSON body must contain one value")
		return false
	}
	return true
}

func writeHTTPJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeHTTPError(w http.ResponseWriter, status int, message string) {
	writeHTTPJSON(w, status, map[string]any{"error": map[string]string{"message": message}})
}

// decodeIdentity reads a base64url JSON identity header into v.
func decodeIdentity(header string, v any) bool {
	raw, err := base64.RawURLEncoding.DecodeString(header)
	return err == nil && json.Unmarshal(raw, v) == nil
}
