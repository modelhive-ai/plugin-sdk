// Package pipelinehttp implements the gateway side of the out-of-process
// plugin protocol. The wire contract is documented in
// docs/plugin-http-openapi.yaml.
package pipelinehttp

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
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/modelhive-ai/plugin-sdk/pkg/plugin"
)

const (
	ProcessPath     = "/v1/plugin/process"
	OutputCheckPath = "/v1/plugin/output/check"
	ObservePath     = "/v1/plugin/observe"

	SignatureHeader = "X-Sundelion-Signature"
	TimestampHeader = "X-Sundelion-Timestamp"

	defaultMaxResponseBytes = int64(1 << 20)
	// defaultTimeout caps a call whose context carries no deadline.
	defaultTimeout = 30 * time.Second
)

// TLSConfig configures server authentication and, when CertFile and KeyFile
// are set, mutual TLS. CAFile is optional; without it the system trust store is
// used. PEM files are read once when the client is constructed.
type TLSConfig struct {
	CAFile     string
	CertFile   string
	KeyFile    string
	ServerName string
}

// Config describes one HTTP plugin instance.
type Config struct {
	Endpoint         string
	Secret           string
	Headers          map[string]string
	TLS              TLSConfig
	MaxResponseBytes int64
	// Timeout caps one call when its context has no deadline; 0 means 30s.
	Timeout time.Duration
}

// Client is safe for concurrent use and keeps idle connections alive.
type Client struct {
	http             *http.Client
	base             *url.URL
	secret           []byte
	headers          http.Header
	maxResponseBytes int64
	now              func() time.Time
}

var (
	_ plugin.Processor   = (*Client)(nil)
	_ plugin.OutputGuard = (*Client)(nil)
	_ plugin.Observer    = (*Client)(nil)
)

// APIError is returned when a plugin answers with a non-2xx status.
type APIError struct {
	StatusCode int
	Body       string
}

// ForwardedResponse is the bounded response from a plugin admin API call.
// Headers are copied before the response body is closed.
type ForwardedResponse struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

func (e *APIError) Error() string {
	return fmt.Sprintf("pipeline plugin: HTTP %d: %s", e.StatusCode, e.Body)
}

// NewClient validates cfg, loads TLS material, and creates a keep-alive HTTP
// transport. Only unix:// and https:// endpoints are accepted.
func NewClient(cfg Config) (*Client, error) {
	base, err := parseEndpoint(cfg.Endpoint)
	if err != nil {
		return nil, err
	}

	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}

	if base.Scheme == "unix" {
		socket := plugin.UnixSocketPath(base.Path)
		transport.ForceAttemptHTTP2 = false
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socket)
		}
		base = &url.URL{Scheme: "http", Host: "plugin"}
	} else {
		tlsConfig, err := loadTLSConfig(cfg.TLS)
		if err != nil {
			return nil, err
		}
		transport.TLSClientConfig = tlsConfig
	}

	maxResponseBytes := cfg.MaxResponseBytes
	if maxResponseBytes == 0 {
		maxResponseBytes = defaultMaxResponseBytes
	}
	if maxResponseBytes < 0 {
		return nil, errors.New("pipeline plugin: max response bytes must not be negative")
	}

	headers := make(http.Header, len(cfg.Headers))
	for k, v := range cfg.Headers {
		if strings.TrimSpace(k) == "" || strings.ContainsAny(k, "\r\n") || strings.ContainsAny(v, "\r\n") {
			return nil, fmt.Errorf("pipeline plugin: invalid HTTP header %q", k)
		}
		headers.Set(k, v)
	}

	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = defaultTimeout
	}
	if timeout < 0 {
		return nil, errors.New("pipeline plugin: timeout must not be negative")
	}

	return &Client{
		http: &http.Client{
			Transport: transport,
			Timeout:   timeout,
			// A redirect would replay the signed body to a host nobody configured.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		base:             base,
		secret:           []byte(cfg.Secret),
		headers:          headers,
		maxResponseBytes: maxResponseBytes,
		now:              time.Now,
	}, nil
}

func parseEndpoint(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("pipeline plugin: parse endpoint: %w", err)
	}
	if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return nil, errors.New("pipeline plugin: endpoint must not contain user info, query, or fragment")
	}
	switch u.Scheme {
	case "unix":
		if u.Host != "" || !path.IsAbs(u.Path) {
			return nil, errors.New("pipeline plugin: unix endpoint must be unix:///absolute/path.sock")
		}
	case "https":
		if u.Host == "" {
			return nil, errors.New("pipeline plugin: https endpoint requires a host")
		}
	default:
		return nil, errors.New("pipeline plugin: endpoint scheme must be unix or https")
	}
	return u, nil
}

func loadTLSConfig(cfg TLSConfig) (*tls.Config, error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: cfg.ServerName}
	if (cfg.CertFile == "") != (cfg.KeyFile == "") {
		return nil, errors.New("pipeline plugin: both TLS certificate and key are required for mTLS")
	}
	if cfg.CertFile != "" {
		cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("pipeline plugin: load mTLS key pair: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
	}
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("pipeline plugin: read CA file: %w", err)
		}
		roots, err := x509.SystemCertPool()
		if err != nil || roots == nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(pem) {
			return nil, errors.New("pipeline plugin: CA file contains no certificates")
		}
		tlsConfig.RootCAs = roots
	}
	return tlsConfig, nil
}

// Close drops idle connections; the client stays usable.
func (c *Client) Close() error {
	c.http.CloseIdleConnections()
	return nil
}

// Process implements plugin.Processor.
func (c *Client) Process(ctx context.Context, request *plugin.Request) (plugin.Ops, error) {
	var response plugin.Ops
	err := c.postJSON(ctx, ProcessPath, request, &response)
	return response, err
}

// CheckOutput implements plugin.OutputGuard. SessionID must remain stable for
// every window of the same generated response.
func (c *Client) CheckOutput(ctx context.Context, window *plugin.OutputWindow) (plugin.OutputVerdict, error) {
	var response plugin.OutputVerdict
	if window == nil || window.SessionID == "" {
		return response, errors.New("pipeline plugin: output check requires a session ID")
	}
	if window.Seq > 0 && window.Request != nil {
		stripped := *window
		stripped.Request = nil
		window = &stripped
	}
	err := c.postJSON(ctx, OutputCheckPath, window, &response)
	return response, err
}

// Observe implements plugin.Observer.
func (c *Client) Observe(ctx context.Context, exchange *plugin.Exchange) error {
	return c.postJSON(ctx, ObservePath, exchange, nil)
}

// ForwardAPI proxies an authenticated gateway admin request to this instance's
// declared endpoint. The caller supplies a path relative to the plugin API;
// the identity, admin or token (exactly one), is encoded and included in the
// API-specific signature.
func (c *Client) ForwardAPI(ctx context.Context, method, relativePath, rawQuery string, body []byte, admin *plugin.Admin, token *plugin.APIToken) (*ForwardedResponse, error) {
	if !strings.HasPrefix(relativePath, "/") {
		return nil, errors.New("pipeline plugin: API path must start with /")
	}
	if int64(len(body)) > 10<<20 {
		return nil, errors.New("pipeline plugin: API request exceeds 10485760 bytes")
	}
	if (admin == nil) == (token == nil) {
		return nil, errors.New("pipeline plugin: API request needs one admin or token identity")
	}
	var adminHeader, tokenHeader string
	identity, header := any(admin), &adminHeader
	if token != nil {
		identity, header = token, &tokenHeader
	}
	identityJSON, err := json.Marshal(identity)
	if err != nil {
		return nil, fmt.Errorf("pipeline plugin: encode API identity: %w", err)
	}
	*header = base64.RawURLEncoding.EncodeToString(identityJSON)
	u := *c.base
	u.Path = joinURLPath(c.base.Path, plugin.HTTPAPIPath+strings.TrimPrefix(relativePath, "/"))
	u.RawQuery = rawQuery
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("pipeline plugin: create API request: %w", err)
	}
	for k, values := range c.headers {
		for _, value := range values {
			req.Header.Add(k, value)
		}
	}
	if adminHeader != "" {
		req.Header.Set(plugin.HTTPAdminHeader, adminHeader)
	} else {
		req.Header.Set(plugin.HTTPTokenHeader, tokenHeader)
	}
	if len(c.secret) != 0 {
		timestamp := strconv.FormatInt(c.now().Unix(), 10)
		req.Header.Set(TimestampHeader, timestamp)
		req.Header.Set(SignatureHeader, "sha256="+apiSignature(c.secret, timestamp, req.Method, requestTarget(req.URL), body, adminHeader, tokenHeader))
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("pipeline plugin: API request: %w", err)
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20+1))
	if err != nil {
		return nil, fmt.Errorf("pipeline plugin: read API response: %w", err)
	}
	if int64(len(responseBody)) > 10<<20 {
		return nil, errors.New("pipeline plugin: API response exceeds 10485760 bytes")
	}
	return &ForwardedResponse{StatusCode: resp.StatusCode, Header: resp.Header.Clone(), Body: responseBody}, nil
}

func (c *Client) postJSON(ctx context.Context, requestPath string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("pipeline plugin: encode request: %w", err)
	}
	u := *c.base
	u.Path = joinURLPath(c.base.Path, requestPath)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("pipeline plugin: create request: %w", err)
	}
	for k, values := range c.headers {
		for _, v := range values {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if len(c.secret) != 0 {
		timestamp := strconv.FormatInt(c.now().Unix(), 10)
		req.Header.Set(TimestampHeader, timestamp)
		req.Header.Set(SignatureHeader, "sha256="+signature(c.secret, timestamp, req.Method, req.URL.EscapedPath(), body))
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("pipeline plugin: request: %w", err)
	}
	defer resp.Body.Close()

	limited := io.LimitReader(resp.Body, c.maxResponseBytes+1)
	responseBody, err := io.ReadAll(limited)
	if err != nil {
		return fmt.Errorf("pipeline plugin: read response: %w", err)
	}
	if int64(len(responseBody)) > c.maxResponseBytes {
		return fmt.Errorf("pipeline plugin: response exceeds %d bytes", c.maxResponseBytes)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		message := strings.TrimSpace(string(responseBody))
		if message == "" {
			message = http.StatusText(resp.StatusCode)
		}
		return &APIError{StatusCode: resp.StatusCode, Body: message}
	}
	if out == nil || len(bytes.TrimSpace(responseBody)) == 0 {
		return nil
	}
	if err := json.Unmarshal(responseBody, out); err != nil {
		return fmt.Errorf("pipeline plugin: decode response: %w", err)
	}
	return nil
}

func joinURLPath(basePath, requestPath string) string {
	if basePath == "" || basePath == "/" {
		return requestPath
	}
	return strings.TrimRight(basePath, "/") + requestPath
}

func signature(secret []byte, timestamp, method, requestPath string, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = io.WriteString(mac, timestamp)
	_, _ = io.WriteString(mac, "\n")
	_, _ = io.WriteString(mac, method)
	_, _ = io.WriteString(mac, "\n")
	_, _ = io.WriteString(mac, requestPath)
	_, _ = io.WriteString(mac, "\n")
	_, _ = mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func apiSignature(secret []byte, timestamp, method, target string, body []byte, adminHeader, tokenHeader string) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = io.WriteString(mac, timestamp+"\n"+method+"\n"+target+"\n"+adminHeader+"\n"+tokenHeader+"\n")
	_, _ = mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func requestTarget(u *url.URL) string {
	target := u.EscapedPath()
	if u.RawQuery != "" {
		target += "?" + u.RawQuery
	}
	return target
}
