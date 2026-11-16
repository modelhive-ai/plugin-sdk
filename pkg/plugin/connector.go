package plugin

import (
	"context"
	"net/http"
)

// Connector executes the lifecycle operations of one plugin on an instance.
// All operations are idempotent, honour ctx cancellation and must never log
// secrets or Outputs values.
type Connector interface {
	// Validate checks the instance config only; it performs no network I/O.
	Validate(ctx context.Context, env Env, in Instance) error
	// Test checks reachability and credentials.
	Test(ctx context.Context, env Env, in Instance) (Report, error)
	// Reconcile creates or updates owned resources and renders the outputs.
	Reconcile(ctx context.Context, env Env, in Instance) (Outputs, error)
	// Discover lists what the remote product exposes (for example visible models).
	Discover(ctx context.Context, env Env, in Instance) (Discovery, error)
	// RotateCredentials replaces the credentials the instance owns.
	RotateCredentials(ctx context.Context, env Env, in Instance) error
	// Deprovision removes the owned resources and drops the others' bindings.
	Deprovision(ctx context.Context, env Env, in Instance) error
}

// SSOStarter is a connector whose app starts "Sign in with Modelhive" at a
// path of its own: the console sends a signed-in user straight there.
type SSOStarter interface {
	SSOLoginPath() string
}

// Env carries what a connector may use to act. The gateway builds it per run
// from the manifest permissions.
type Env struct {
	// HTTPClient only reaches hosts declared in permissions.network.
	HTTPClient *http.Client
	// Resources exposes only the Modelhive operations the manifest permits.
	// Served out of process, each call is an authenticated callback.
	Resources Resources
	// SecretOutput hands a secret produced outside Reconcile (for example by
	// RotateCredentials) to the gateway, which seals it in the instance's
	// one-time output slot. Nil outside a lifecycle run.
	SecretOutput func(name, value string)
}

// Resources is the facade over Modelhive objects. Every object a method
// creates is recorded as an owned binding of the instance.
type Resources interface {
	// CreateOIDCClient creates an OIDC client and returns its ID and one-time secret (needs apps:write).
	CreateOIDCClient(ctx context.Context, spec OIDCClientSpec) (id, secret string, err error)
	// UpdateOIDCClient updates name, redirect URIs, post-logout URIs and PKCE of an owned client (needs apps:write).
	UpdateOIDCClient(ctx context.Context, id string, spec OIDCClientSpec) error
	// RotateOIDCClientSecret issues a new secret for an owned confidential client and returns it once (needs apps:write).
	RotateOIDCClientSecret(ctx context.Context, id string) (secret string, err error)
	// OwnedOIDCClient returns the OIDC client this instance owns, or nil when it has none (needs apps:write).
	OwnedOIDCClient(ctx context.Context) (*OIDCClientInfo, error)
	// DeleteOIDCClient removes an owned OIDC client (needs apps:write).
	DeleteOIDCClient(ctx context.Context, id string) error
	// CreateServiceKey issues a virtual key for the instance and returns its ID and plaintext (needs keys:write).
	CreateServiceKey(ctx context.Context, spec ServiceKeySpec) (id, plaintext string, err error)
	// RevokeServiceKey revokes an owned service key (needs keys:write).
	RevokeServiceKey(ctx context.Context, id string) error
	// BindAccessGroup records a not-owned binding to an existing Access group (needs access:write).
	BindAccessGroup(ctx context.Context, groupID string) error
	// StoreSecret persists a plugin-provided secret listed in
	// permissions.secrets as an instance secret, so {{secret:<name>}}
	// resolves it. Other names are refused.
	StoreSecret(ctx context.Context, name, value string) error
	// ApplyStack renders the manifest workload with in and hands it to
	// sundelion-runtime; it returns once the runtime accepted it (needs spec.workload).
	ApplyStack(ctx context.Context, in StackInput) error
	// StackStatus reads the instance's stack from the runtime (needs spec.workload).
	StackStatus(ctx context.Context) (StackStatus, error)
	// RemoveStack stops and removes the instance's containers; volumes are
	// deleted only when deleteVolumes is set (needs spec.workload).
	RemoveStack(ctx context.Context, deleteVolumes bool) error
}

// StackInput is what the connector supplies to render the workload: the IDs
// of the resources it created and the host port of the exposed service.
// Config, secrets and the sundelion.* values come from the gateway.
type StackInput struct {
	Resources map[string]string `json:"resources,omitempty"` // oidc.client_id, service_key.id
	HostPort  int               `json:"host_port,omitempty"`
	// PublicURL is the URL browsers use to reach the service ({{instance.public_url}}).
	PublicURL string `json:"public_url,omitempty"`
}

// StackStatus is the runtime state of an instance's containers.
type StackStatus struct {
	Instance string          `json:"instance"`
	Phase    string          `json:"phase"`
	Error    string          `json:"error,omitempty"`
	Services []ServiceStatus `json:"services"`
}

// ServiceStatus is one container of a stack.
type ServiceStatus struct {
	Name         string `json:"name"`
	State        string `json:"state"`
	Image        string `json:"image,omitempty"`
	ExitCode     int    `json:"exit_code,omitempty"`
	RestartCount int    `json:"restart_count,omitempty"`
	HostPort     int    `json:"host_port,omitempty"`
}

// OIDCClientSpec describes an OIDC client to create or update.
type OIDCClientSpec struct {
	Name         string   `json:"name"`
	RedirectURIs []string `json:"redirect_uris"`
	// PostLogoutRedirectURIs: on update nil leaves the list unchanged, an empty slice clears it.
	PostLogoutRedirectURIs []string `json:"post_logout_redirect_uris,omitempty"`
	// NoPKCE turns PKCE off for a confidential client (default: required).
	NoPKCE bool `json:"no_pkce,omitempty"`
	// Public creates a client without secret (auth method none, PKCE forced). Immutable after creation.
	Public bool `json:"public,omitempty"`
}

// OIDCClientInfo is a plugin's view of an owned OIDC client; it never carries the secret.
type OIDCClientInfo struct {
	ID                     string   `json:"id"`
	Name                   string   `json:"name"`
	Active                 bool     `json:"active"`
	Public                 bool     `json:"public"`
	RequirePKCE            bool     `json:"require_pkce"`
	RedirectURIs           []string `json:"redirect_uris"`
	PostLogoutRedirectURIs []string `json:"post_logout_redirect_uris"`
}

// ServiceKeySpec describes a service key to create.
type ServiceKeySpec struct {
	Name          string `json:"name"`
	AccessGroupID string `json:"access_group_id,omitempty"`
	// AccessGroupName, when AccessGroupID is empty, puts the key in the group
	// with this name, creating it if missing (needs access:write). The group
	// is not owned: it outlives the instance with its budget and history.
	AccessGroupName string `json:"access_group_name,omitempty"`
}

// Report is the result of Test.
type Report struct {
	OK      bool
	Message string // sanitised, no remote raw output
	Checks  []Check
}

// Check is one line of a Report.
type Check struct {
	Name    string
	OK      bool
	Message string
}

// Outputs is what Reconcile renders. Values may be secrets: never log them.
type Outputs struct {
	// Env values may be secrets: the gateway seals them in the instance's
	// one-time output slot and never stores them in the job result.
	Env map[string]string
	// Public values are not secret and are kept in clear in the job result.
	Public map[string]string
	Files  map[string][]byte
}

// Discovery is the result of Discover.
type Discovery struct {
	Models []string
	Info   map[string]string
}
