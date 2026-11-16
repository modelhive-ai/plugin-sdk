package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// ResourcesPath is the gateway path prefix of the resource callback.
	ResourcesPath = "/api/v1/plugin-resources/"

	resourceMaxResponseBytes = int64(1 << 20)
)

// ResourceError is returned when the gateway refuses or fails a callback.
// 401 means the instance token is unknown or revoked, 403 that the manifest
// does not permit the operation.
type ResourceError struct {
	StatusCode int
	Message    string
}

func (e *ResourceError) Error() string {
	return fmt.Sprintf("plugin resource API: HTTP %d: %s", e.StatusCode, e.Message)
}

type resourceClient struct {
	base  *url.URL
	token string
	hc    *http.Client
}

// NewResourceClient returns a Resources that calls the gateway's resource
// callback at gatewayURL (http or https) with the instance bearer token; see
// ResourceToken. hc may be nil, which uses a client with a 30s timeout. What
// the calls may do is decided by the gateway from the manifest permissions.
func NewResourceClient(gatewayURL, token string, hc *http.Client) (Resources, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, errors.New("plugin: resource token is required")
	}
	u, err := url.Parse(strings.TrimSpace(gatewayURL))
	if err != nil {
		return nil, fmt.Errorf("plugin: parse gateway URL: %w", err)
	}
	if (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("plugin: gateway URL must be http(s)://host[:port][/base]")
	}
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &resourceClient{base: u, token: token, hc: hc}, nil
}

func (c *resourceClient) CreateOIDCClient(ctx context.Context, spec OIDCClientSpec) (string, string, error) {
	var out struct {
		ID     string `json:"id"`
		Secret string `json:"secret"`
	}
	err := c.post(ctx, "oidc-clients", struct {
		Spec OIDCClientSpec `json:"spec"`
	}{spec}, &out)
	return out.ID, out.Secret, err
}

func (c *resourceClient) UpdateOIDCClient(ctx context.Context, id string, spec OIDCClientSpec) error {
	return c.post(ctx, "oidc-clients/"+url.PathEscape(id)+"/update", struct {
		Spec OIDCClientSpec `json:"spec"`
	}{spec}, nil)
}

func (c *resourceClient) RotateOIDCClientSecret(ctx context.Context, id string) (string, error) {
	var out struct {
		Secret string `json:"secret"`
	}
	err := c.post(ctx, "oidc-clients/"+url.PathEscape(id)+"/rotate-secret", struct{}{}, &out)
	return out.Secret, err
}

func (c *resourceClient) OwnedOIDCClient(ctx context.Context) (*OIDCClientInfo, error) {
	var out struct {
		Client *OIDCClientInfo `json:"client"`
	}
	err := c.post(ctx, "oidc-clients/owned", struct{}{}, &out)
	return out.Client, err
}

func (c *resourceClient) DeleteOIDCClient(ctx context.Context, id string) error {
	return c.post(ctx, "oidc-clients/"+url.PathEscape(id)+"/delete", struct{}{}, nil)
}

func (c *resourceClient) CreateServiceKey(ctx context.Context, spec ServiceKeySpec) (string, string, error) {
	var out struct {
		ID  string `json:"id"`
		Key string `json:"key"`
	}
	err := c.post(ctx, "service-keys", struct {
		Spec ServiceKeySpec `json:"spec"`
	}{spec}, &out)
	return out.ID, out.Key, err
}

func (c *resourceClient) RevokeServiceKey(ctx context.Context, id string) error {
	return c.post(ctx, "service-keys/"+url.PathEscape(id)+"/revoke", struct{}{}, nil)
}

func (c *resourceClient) BindAccessGroup(ctx context.Context, groupID string) error {
	return c.post(ctx, "access-groups/bind", map[string]string{"group_id": groupID}, nil)
}

func (c *resourceClient) StoreSecret(ctx context.Context, name, value string) error {
	return c.post(ctx, "secrets/store", map[string]string{"name": name, "value": value}, nil)
}

func (c *resourceClient) ApplyStack(ctx context.Context, in StackInput) error {
	return c.post(ctx, "stack/apply", in, nil)
}

func (c *resourceClient) StackStatus(ctx context.Context) (StackStatus, error) {
	var out StackStatus
	err := c.post(ctx, "stack/status", struct{}{}, &out)
	return out, err
}

func (c *resourceClient) RemoveStack(ctx context.Context, deleteVolumes bool) error {
	return c.post(ctx, "stack/remove", map[string]bool{"delete_volumes": deleteVolumes}, nil)
}

func (c *resourceClient) post(ctx context.Context, op string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("plugin resource API: encode request: %w", err)
	}
	u := *c.base
	u.RawPath = strings.TrimRight(c.base.EscapedPath(), "/") + ResourcesPath + op
	decoded, err := url.PathUnescape(u.RawPath)
	if err != nil {
		return fmt.Errorf("plugin resource API: bad path: %w", err)
	}
	u.Path = decoded
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("plugin resource API: create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("plugin resource API: request: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, resourceMaxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("plugin resource API: read response: %w", err)
	}
	if int64(len(data)) > resourceMaxResponseBytes {
		return fmt.Errorf("plugin resource API: response exceeds %d bytes", resourceMaxResponseBytes)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &ResourceError{StatusCode: resp.StatusCode, Message: errorMessage(data, resp.StatusCode)}
	}
	if out == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("plugin resource API: decode response: %w", err)
	}
	return nil
}

func errorMessage(data []byte, status int) string {
	var env struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &env) == nil && env.Error.Message != "" {
		return env.Error.Message
	}
	if s := strings.TrimSpace(string(data)); s != "" {
		return s
	}
	return http.StatusText(status)
}
