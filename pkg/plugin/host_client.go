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

const hostPath = ResourcesPath + "host"

var hostDiscoveryMaxWait = 8 * time.Second

// HostError is returned for malformed or failed host callback requests that
// cannot be represented by a Host service sentinel.
type HostError struct {
	StatusCode int
	Message    string
}

func (e *HostError) Error() string {
	return fmt.Sprintf("plugin host API: HTTP %d: %s", e.StatusCode, e.Message)
}

type hostClient struct {
	base  *url.URL
	token string
	hc    *http.Client
}

// NewHostClient discovers the permitted host services and returns a Host that
// exposes only those services. token is normally ResourceToken(hmacSecret).
func NewHostClient(gatewayURL, token string, hc *http.Client) (Host, error) {
	c, err := newHostClient(gatewayURL, token, hc)
	if err != nil {
		return Host{}, err
	}
	var out struct {
		Services map[string]bool `json:"services"`
	}
	// The gateway may grant this instance's token just after starting it.
	for wait := 250 * time.Millisecond; ; wait *= 2 {
		err = c.get(context.Background(), "", &out)
		var he *HostError
		if err == nil || wait > hostDiscoveryMaxWait || errors.Is(err, ErrDenied) || errors.As(err, &he) && he.StatusCode != http.StatusUnauthorized {
			break
		}
		time.Sleep(wait)
	}
	if err != nil {
		return Host{}, err
	}
	host := Host{}
	if out.Services["storage"] {
		host.Storage = hostStorage{c}
	}
	if out.Services["vault"] {
		host.Vault = hostVault{c}
	}
	if out.Services["models"] {
		host.Models = hostModels{c}
	}
	if out.Services["pricing"] {
		host.Pricing = hostPricing{c}
	}
	if out.Services["guard"] {
		host.Guard = hostGuard{c}
	}
	if out.Services["groups"] {
		host.Groups = hostGroups{c}
	}
	if out.Services["users"] {
		host.Users = hostUsers{c}
	}
	if out.Services["sessions"] {
		host.Sessions = hostSessions{c}
	}
	if out.Services["plugins"] {
		host.Plugins = hostPlugins{c}
	}
	return host, nil
}

func newHostClient(gatewayURL, token string, hc *http.Client) (*hostClient, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, errors.New("plugin: host token is required")
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
	return &hostClient{base: u, token: token, hc: hc}, nil
}

func (c *hostClient) endpoint(op string) (*url.URL, error) {
	u := *c.base
	u.RawPath = strings.TrimRight(c.base.EscapedPath(), "/") + hostPath
	if op != "" {
		u.RawPath += "/" + op
	}
	decoded, err := url.PathUnescape(u.RawPath)
	if err != nil {
		return nil, fmt.Errorf("plugin host API: bad path: %w", err)
	}
	u.Path = decoded
	return &u, nil
}

func (c *hostClient) get(ctx context.Context, op string, out any) error {
	return c.call(ctx, http.MethodGet, op, nil, out)
}

func (c *hostClient) post(ctx context.Context, op string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("plugin host API: encode request: %w", err)
	}
	return c.call(ctx, http.MethodPost, op, body, out)
}

func (c *hostClient) call(ctx context.Context, method, op string, body []byte, out any) error {
	u, err := c.endpoint(op)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("plugin host API: create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("plugin host API: request: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, resourceMaxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("plugin host API: read response: %w", err)
	}
	if int64(len(data)) > resourceMaxResponseBytes {
		return fmt.Errorf("plugin host API: response exceeds %d bytes", resourceMaxResponseBytes)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return hostStatusError(resp.StatusCode, errorMessage(data, resp.StatusCode))
	}
	if out != nil && len(bytes.TrimSpace(data)) != 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("plugin host API: decode response: %w", err)
		}
	}
	return nil
}

func hostStatusError(status int, message string) error {
	var sentinel error
	switch status {
	case http.StatusNotFound:
		sentinel = ErrNotFound
	case http.StatusConflict:
		sentinel = ErrConflict
	case http.StatusForbidden:
		sentinel = ErrDenied
	case http.StatusRequestEntityTooLarge:
		sentinel = ErrQuota
	}
	if sentinel != nil {
		return fmt.Errorf("%w: %s", sentinel, message)
	}
	return &HostError{StatusCode: status, Message: message}
}

type hostStorage struct{ c *hostClient }

func (s hostStorage) Get(ctx context.Context, collection, id string) (*Doc, error) {
	var out Doc
	err := s.c.post(ctx, "storage/get", struct{ Collection, ID string }{collection, id}, &out)
	return &out, err
}
func (s hostStorage) Put(ctx context.Context, collection, id string, data json.RawMessage, options PutOptions) (*Doc, error) {
	var out Doc
	err := s.c.post(ctx, "storage/put", struct {
		Collection, ID string
		Data           json.RawMessage
		Options        PutOptions
	}{collection, id, data, options}, &out)
	return &out, err
}
func (s hostStorage) Delete(ctx context.Context, collection, id string, ifRevision int64) error {
	return s.c.post(ctx, "storage/delete", struct {
		Collection, ID string
		IfRevision     int64 `json:"if_revision"`
	}{collection, id, ifRevision}, nil)
}
func (s hostStorage) Query(ctx context.Context, q Query) (*QueryResult, error) {
	var out QueryResult
	err := s.c.post(ctx, "storage/query", q, &out)
	return &out, err
}
func (s hostStorage) Batch(ctx context.Context, ops []BatchOp) ([]*Doc, error) {
	var out struct {
		Docs []*Doc `json:"docs"`
	}
	err := s.c.post(ctx, "storage/batch", struct {
		Ops []BatchOp `json:"ops"`
	}{ops}, &out)
	return out.Docs, err
}

type hostVault struct{ c *hostClient }

func (v hostVault) PutSecret(ctx context.Context, name, value, actor string) error {
	return v.c.post(ctx, "vault/put", struct{ Name, Value, Actor string }{name, value, actor}, nil)
}
func (v hostVault) GetSecret(ctx context.Context, name string) (string, error) {
	var out struct {
		Value string `json:"value"`
	}
	err := v.c.post(ctx, "vault/get", struct{ Name string }{name}, &out)
	return out.Value, err
}
func (v hostVault) ListSecrets(ctx context.Context) ([]SecretInfo, error) {
	var out struct {
		Secrets []SecretInfo `json:"secrets"`
	}
	err := v.c.post(ctx, "vault/list", struct{}{}, &out)
	return out.Secrets, err
}
func (v hostVault) DeleteSecret(ctx context.Context, name string) error {
	return v.c.post(ctx, "vault/delete", struct{ Name string }{name}, nil)
}

type hostModels struct{ c *hostClient }

func (m hostModels) Complete(ctx context.Context, req CompletionRequest) (*CompletionResult, error) {
	var out CompletionResult
	err := m.c.post(ctx, "models/complete", req, &out)
	return &out, err
}
func (m hostModels) List(ctx context.Context) ([]string, error) {
	var out struct {
		Models []string `json:"models"`
	}
	err := m.c.post(ctx, "models/list", struct{}{}, &out)
	return out.Models, err
}
func (m hostModels) Info(ctx context.Context, model string) (ModelInfo, error) {
	var out ModelInfo
	err := m.c.post(ctx, "models/info", struct {
		Model string `json:"model"`
	}{Model: model}, &out)
	return out, err
}
func (m hostModels) Classify(ctx context.Context, req ClassifyRequest) (ClassifyResult, error) {
	var out ClassifyResult
	err := m.c.post(ctx, "models/classify", req, &out)
	return out, err
}
func (m hostModels) Embed(ctx context.Context, req EmbedRequest) (EmbedResult, error) {
	var out EmbedResult
	err := m.c.post(ctx, "models/embed", req, &out)
	return out, err
}
func (m hostModels) Classifiers(ctx context.Context) ([]string, error) {
	var out struct {
		Classifiers []string `json:"classifiers"`
	}
	err := m.c.post(ctx, "models/classifiers", struct{}{}, &out)
	return out.Classifiers, err
}

type hostPricing struct{ c *hostClient }

func (p hostPricing) Model(ctx context.Context, model string) (ModelPrice, error) {
	var out ModelPrice
	err := p.c.post(ctx, "pricing/model", struct {
		Model string `json:"model"`
	}{Model: model}, &out)
	return out, err
}

type hostGuard struct{ c *hostClient }

func (g hostGuard) Check(ctx context.Context, check, text string) (*GuardResult, error) {
	var out GuardResult
	err := g.c.post(ctx, "guard/check", struct{ Check, Text string }{check, text}, &out)
	return &out, err
}
func (g hostGuard) Redact(ctx context.Context, text string) (string, error) {
	var out struct {
		Text string `json:"text"`
	}
	err := g.c.post(ctx, "guard/redact", struct{ Text string }{text}, &out)
	return out.Text, err
}

type hostGroups struct{ c *hostClient }

func (g hostGroups) AccessGroups(ctx context.Context) ([]AccessGroup, error) {
	var out struct {
		Groups []AccessGroup `json:"groups"`
	}
	err := g.c.post(ctx, "groups/list", struct{}{}, &out)
	return out.Groups, err
}

type hostUsers struct{ c *hostClient }

func (u hostUsers) List(ctx context.Context) ([]User, error) {
	var out struct {
		Users []User `json:"users"`
	}
	err := u.c.post(ctx, "users/list", struct{}{}, &out)
	return out.Users, err
}

type hostSessions struct{ c *hostClient }

func (s hostSessions) Get(ctx context.Context, sessionID, keyID string) (SessionInfo, error) {
	var out SessionInfo
	err := s.c.post(ctx, "sessions/get", struct {
		SessionID string `json:"session_id"`
		KeyID     string `json:"key_id"`
	}{sessionID, keyID}, &out)
	return out, err
}

type hostPlugins struct{ c *hostClient }

func (p hostPlugins) Contributions(ctx context.Context, point string) ([]PointContribution, error) {
	var out struct {
		Contributions []PointContribution `json:"contributions"`
	}
	err := p.c.post(ctx, "plugins/contributions", struct{ Point string }{point}, &out)
	return out.Contributions, err
}
func (p hostPlugins) Invoke(ctx context.Context, instanceID string, req *Request) (Ops, error) {
	var out Ops
	err := p.c.post(ctx, "plugins/invoke", struct {
		InstanceID string  `json:"instance_id"`
		Request    Request `json:"request"`
	}{instanceID, *req}, &out)
	return out, err
}
