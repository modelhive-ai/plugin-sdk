package pipelinehttp

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
)

var tlsFileRoot atomic.Value

// SetTLSFileRoot names the only directory instance configs may read tls_ca_file,
// tls_cert_file and tls_key_file from. Until it is set every such path is
// refused, so a config cannot make the gateway read an arbitrary host file.
func SetTLSFileRoot(dir string) { tlsFileRoot.Store(dir) }

// resolveTLSFile returns path when, with symlinks resolved, it is inside the
// TLS file root; relative paths are taken relative to the root.
func resolveTLSFile(name, path string) (string, error) {
	if path == "" {
		return "", nil
	}
	root, _ := tlsFileRoot.Load().(string)
	if root == "" {
		return "", fmt.Errorf("pipeline plugin: %s is not allowed: no TLS file directory is configured", name)
	}
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("pipeline plugin: %s: TLS file directory is unavailable", name)
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(rootReal, path)
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("pipeline plugin: %s: file not found in the TLS file directory", name)
	}
	rel, err := filepath.Rel(rootReal, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("pipeline plugin: %s must be inside the TLS file directory", name)
	}
	return real, nil
}

// SecretHMAC is the instance secret that signs calls to the plugin.
const SecretHMAC = "hmac_secret"

// NewFromInstance builds the client of an `engine: http` instance from its
// non-secret config and secret accessor. Config keys: endpoint (required,
// https://host[:port]), tls_ca_file, tls_cert_file,
// tls_key_file (paths inside the directory set by SetTLSFileRoot), tls_server_name. The signing secret is the instance secret
// hmac_secret and is required.
//
// allow decides whether the gateway may dial an https host:port; it is how
// permissions.network applies to an endpoint an admin typed in. Unix sockets
// are reserved for supervisor-managed local plugins and are never accepted
// from an instance configuration. A nil allow refuses every https endpoint.
func NewFromInstance(cfg map[string]any, secret func(name string) (string, error), allow func(host string, port int) bool) (*Client, error) {
	str := func(key string) string { s, _ := cfg[key].(string); return s }
	endpoint := str("endpoint")
	if endpoint == "" {
		return nil, errors.New("pipeline plugin: config.endpoint is required")
	}
	u, err := parseEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	if u.Scheme == "unix" {
		return nil, errors.New("pipeline plugin: unix endpoints require a managed local plugin")
	}
	if u.Scheme == "https" {
		port := 443
		if p := u.Port(); p != "" {
			if port, err = strconv.Atoi(p); err != nil {
				return nil, errors.New("pipeline plugin: endpoint has an invalid port")
			}
		}
		if allow == nil || !allow(u.Hostname(), port) {
			return nil, fmt.Errorf("pipeline plugin: endpoint host %s:%d is not declared in permissions.network", u.Hostname(), port)
		}
	}
	if secret == nil {
		return nil, errors.New("pipeline plugin: no secret store")
	}
	key, err := secret(SecretHMAC)
	if err != nil || key == "" {
		return nil, fmt.Errorf("pipeline plugin: secret %s is required", SecretHMAC)
	}
	var files [3]string
	for i, name := range []string{"tls_ca_file", "tls_cert_file", "tls_key_file"} {
		if files[i], err = resolveTLSFile(name, str(name)); err != nil {
			return nil, err
		}
	}
	return NewClient(Config{
		Endpoint: endpoint,
		Secret:   key,
		TLS: TLSConfig{
			CAFile: files[0], CertFile: files[1],
			KeyFile: files[2], ServerName: str("tls_server_name"),
		},
	})
}
