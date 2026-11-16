package plugintest

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/modelhive-ai/plugin-sdk/internal/pipelinehttp"
	"github.com/modelhive-ai/plugin-sdk/pkg/plugin"
)

const wireSecret = "plugintest-hmac-secret"

// Transport is a way the gateway can reach a served plugin.
type Transport struct {
	// Name is "unix" or "https".
	Name string
	// Client is the gateway's HTTP engine pointed at a server running the plugin.
	Client *pipelinehttp.Client

	endpoint string
	tls      pipelinehttp.TLSConfig
}

// Serve starts d's runtime handler for in behind the HTTP protocol, once over
// a unix socket and once over TLS, and returns a gateway client for each. The
// servers stop when t ends.
func Serve(t *testing.T, d plugin.Definition, in plugin.Instance) []Transport {
	t.Helper()
	h, err := plugin.NewHTTPHandler(d, in, wireSecret)
	if err != nil {
		t.Fatalf("plugintest: serve %q: %v", d.Ref, err)
	}

	// A short directory: unix socket paths are limited to about 100 bytes.
	dir, err := os.MkdirTemp("", "sdp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	listener, err := net.Listen("unix", filepath.Join(dir, "p.sock"))
	if err != nil {
		t.Fatalf("plugintest: listen: %v", err)
	}
	unixServer := httptest.NewUnstartedServer(h)
	_ = unixServer.Listener.Close()
	unixServer.Listener = listener
	unixServer.Start()
	t.Cleanup(unixServer.Close)

	tlsServer := httptest.NewUnstartedServer(h)
	tlsServer.StartTLS()
	t.Cleanup(tlsServer.Close)
	caFile := filepath.Join(dir, "ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tlsServer.Certificate().Raw})
	if pool := x509.NewCertPool(); !pool.AppendCertsFromPEM(pemBytes) {
		t.Fatal("plugintest: test server certificate is not valid PEM")
	}
	if err := os.WriteFile(caFile, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	endpoints := []struct {
		name, url string
		tls       pipelinehttp.TLSConfig
	}{
		{"unix", "unix://" + filepath.Join(dir, "p.sock"), pipelinehttp.TLSConfig{}},
		{"https", tlsServer.URL, pipelinehttp.TLSConfig{CAFile: caFile}},
	}
	var out []Transport
	for _, e := range endpoints {
		c, err := pipelinehttp.NewClient(pipelinehttp.Config{Endpoint: e.url, Secret: wireSecret, TLS: e.tls})
		if err != nil {
			t.Fatalf("plugintest: %s client: %v", e.name, err)
		}
		t.Cleanup(func() { _ = c.Close() })
		out = append(out, Transport{Name: e.name, Client: c, endpoint: e.url, tls: e.tls})
	}
	return out
}

// Run is the full conformance suite for a plugin that implements Processor.
// It checks, for every case, that the plugin returns the expected Ops
// in-process, and that the same Ops come back when the plugin is served over
// unix:// and https://. It also checks that an expired context does not leave
// Process running, and that the served plugin refuses calls signed with the
// wrong secret. A panic is reported as an error in-process and as a 500 over
// the wire; both count as the same failure.
func Run(t *testing.T, d plugin.Definition, in plugin.Instance, cases ...ProcessCase) {
	t.Helper()
	TestDefinition(t, d, in, cases...)

	h, err := d.New(context.Background(), in)
	if err != nil {
		t.Fatalf("plugintest: factory: %v", err)
	}
	local, ok := h.(plugin.Processor)
	if !ok {
		t.Fatalf("plugintest: handler %T does not implement plugin.Processor", h)
	}
	transports := Serve(t, d, in)

	for _, tr := range transports {
		t.Run("wire/"+tr.Name, func(t *testing.T) {
			for i, tc := range cases {
				name := tc.Name
				if name == "" {
					name = "case"
				}
				t.Run(name, func(t *testing.T) {
					ctx := tc.Context
					if ctx == nil {
						ctx = context.Background()
					}
					req := cloneRequest(tc.Request)
					want, wantErr := process(local, ctx, &req)
					req = cloneRequest(tc.Request)
					got, gotErr := tr.Client.Process(ctx, &req)
					if (wantErr == nil) != (gotErr == nil) {
						t.Fatalf("case %d: in-process error = %v, over %s error = %v", i, wantErr, tr.Name, gotErr)
					}
					if wantErr == nil && !sameOps(want, got) {
						t.Errorf("case %d: in-process Ops %#v differ from Ops over %s %#v", i, want, tr.Name, got)
					}
				})
			}
			t.Run("rejects bad signature", func(t *testing.T) {
				if status := rawStatus(t, tr, "wrong-secret"); status != http.StatusUnauthorized {
					t.Errorf("wrongly signed call: status %d, want 401", status)
				}
			})
		})
	}

	t.Run("expired context", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
		defer cancel()
		<-ctx.Done()
		done := make(chan struct{})
		go func() {
			defer close(done)
			req := cloneRequest(cases[0].Request)
			_, _ = process(local, ctx, &req)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Process ignored an expired context and kept running")
		}
	})
}

// sameOps compares Ops as the wire sees them, where nil and empty collapse.
func sameOps(a, b plugin.Ops) bool {
	norm := func(o plugin.Ops) any {
		raw, _ := json.Marshal(o)
		var v any
		_ = json.Unmarshal(raw, &v)
		return v
	}
	return reflect.DeepEqual(norm(a), norm(b))
}

// rawStatus posts a Process call signed with secret and returns the status.
func rawStatus(t *testing.T, tr Transport, secret string) int {
	t.Helper()
	c, err := pipelinehttp.NewClient(pipelinehttp.Config{Endpoint: tr.endpoint, Secret: secret, TLS: tr.tls})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, err = c.Process(context.Background(), &plugin.Request{})
	var api *pipelinehttp.APIError
	if errors.As(err, &api) {
		return api.StatusCode
	}
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	return http.StatusOK
}
