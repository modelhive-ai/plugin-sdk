package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

// Host services: what the gateway offers a plugin beyond its pipeline hooks.
// They are the same for a compiled-in plugin and for a third-party one served
// over HTTP (each call then is an authenticated callback to
// /api/v1/plugin-resources/, see docs/plugin-http-openapi.yaml), so anything a
// builtin plugin does a third party can do too.
//
// Each service is nil unless the manifest asks for it in
// spec.permissions.sundelion:
//
//	storage         Storage   (also needs spec.storage)
//	vault           Vault
//	models:invoke   Models
//	pricing:read    Pricing
//	guard:check     Guard
//	groups:read     Groups
//	users:read      Users
//	sessions:read   Sessions
//	plugins:invoke  Plugins   (only the points the plugin declares in spec.points)
type Host struct {
	Storage  Storage
	Vault    Vault
	Models   Models
	Pricing  Pricing
	Guard    Guard
	Groups   Groups
	Users    Users
	Sessions Sessions
	Plugins  Plugins
}

// Errors a host service returns. Over HTTP they travel as status codes and
// the client maps them back, so errors.Is works on both sides.
var (
	ErrNotFound = errors.New("plugin host: not found")
	// ErrConflict: a revision precondition failed or a unique index is taken.
	ErrConflict = errors.New("plugin host: conflict")
	// ErrDenied: the manifest does not permit the operation.
	ErrDenied = errors.New("plugin host: not permitted")
	// ErrQuota: the plugin's storage quota is exhausted.
	ErrQuota = errors.New("plugin host: quota exceeded")
)

// --- Storage ----------------------------------------------------------------

// Storage is the plugin's own document store. Documents live in the
// collections declared in spec.storage; another plugin's documents are never
// visible. Every document carries a revision that starts at 1 and grows by one
// on each write.
type Storage interface {
	Get(ctx context.Context, collection, id string) (*Doc, error)
	// Put creates or replaces a document. IfRevision: 0 writes whatever is
	// there, -1 only creates (ErrConflict when it exists), n > 0 only replaces
	// revision n.
	Put(ctx context.Context, collection, id string, data json.RawMessage, opt PutOptions) (*Doc, error)
	// Delete removes a document; ifRevision 0 deletes whatever is there.
	// Deleting a missing document is ErrNotFound.
	Delete(ctx context.Context, collection, id string, ifRevision int64) error
	// Query lists documents of one collection filtered and ordered by
	// indexed fields only.
	Query(ctx context.Context, q Query) (*QueryResult, error)
	// Batch applies every operation or none. The results match ops by index
	// (nil for deletes and checks).
	Batch(ctx context.Context, ops []BatchOp) ([]*Doc, error)
}

// Doc is one stored document.
type Doc struct {
	Collection string          `json:"collection"`
	ID         string          `json:"id"`
	Revision   int64           `json:"revision"`
	Data       json.RawMessage `json:"data"`
	CreatedAt  time.Time       `json:"created_at"`
	UpdatedAt  time.Time       `json:"updated_at"`
	ExpiresAt  *time.Time      `json:"expires_at,omitempty"`
}

// PutOptions qualifies a write.
type PutOptions struct {
	IfRevision int64 `json:"if_revision,omitempty"`
	// ExpiresAt lets the gateway delete the document after this time (run
	// logs, caches). Nil keeps it until deleted.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// Query selects documents. Field names are top-level JSON keys of Data that
// the collection declares as indexes, or the pseudo-fields "_id",
// "_created_at", "_updated_at".
type Query struct {
	Collection string `json:"collection"`
	Where      []Cond `json:"where,omitempty"`
	OrderBy    string `json:"order_by,omitempty"` // default "_id"
	Desc       bool   `json:"desc,omitempty"`
	// Limit defaults to 50 and is capped at 500.
	Limit int `json:"limit,omitempty"`
	// Cursor is QueryResult.Next of the previous page.
	Cursor string `json:"cursor,omitempty"`
}

// Cond is one condition; all conditions of a Query must hold. Op is one of
// "=", "!=", "<", "<=", ">", ">=", "in" (Value is an array), "prefix"
// (strings). Strings compare as strings, numbers as numbers, booleans with =
// and != only; RFC 3339 timestamps should be stored as strings in UTC.
type Cond struct {
	Field string `json:"field"`
	Op    string `json:"op"`
	Value any    `json:"value"`
}

// QueryResult is one page.
type QueryResult struct {
	Docs []Doc  `json:"docs"`
	Next string `json:"next,omitempty"` // empty on the last page
}

// BatchOp is one operation of a Batch. Op is "put", "delete" or "check"
// (asserts that the document is at IfRevision, -1 meaning absent, without
// writing it).
type BatchOp struct {
	Op         string          `json:"op"`
	Collection string          `json:"collection"`
	ID         string          `json:"id"`
	Data       json.RawMessage `json:"data,omitempty"`
	IfRevision int64           `json:"if_revision,omitempty"`
	ExpiresAt  *time.Time      `json:"expires_at,omitempty"`
}

// --- Vault -------------------------------------------------------------------

// Vault keeps the plugin's own secrets, sealed by the gateway. Names are
// [A-Za-z0-9_]{1,64}; values at most 64 KiB. Unlike Instance.Secrets (values
// an administrator set on the instance), these are written by the plugin,
// typically from its own console page.
type Vault interface {
	PutSecret(ctx context.Context, name, value, actor string) error
	// GetSecret returns ErrNotFound for an unknown name.
	GetSecret(ctx context.Context, name string) (string, error)
	ListSecrets(ctx context.Context) ([]SecretInfo, error)
	DeleteSecret(ctx context.Context, name string) error
}

// SecretInfo describes a vault secret without its value.
type SecretInfo struct {
	Name      string    `json:"name"`
	UpdatedBy string    `json:"updated_by,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// --- Models ------------------------------------------------------------------

// Models calls the models configured on the gateway, with its routing,
// credentials and spend accounting. Calls are recorded as made by the plugin.
type Models interface {
	Complete(ctx context.Context, req CompletionRequest) (*CompletionResult, error)
	// List returns the model names a Complete may use.
	List(ctx context.Context) ([]string, error)
	// Info describes a model or alias the way Complete resolves it. A model
	// the gateway does not know is ErrNotFound.
	Info(ctx context.Context, model string) (ModelInfo, error)
	// Classify asks a SystemOne classifier (see Classifiers) a question about
	// a text. Any other model is ErrNotFound.
	Classify(ctx context.Context, req ClassifyRequest) (ClassifyResult, error)
	// Classifiers lists the SystemOne model names Classify accepts.
	Classifiers(ctx context.Context) ([]string, error)
	// Embed returns one embedding vector per text from req.Model, an
	// embedding model of this gateway, in the order of req.Texts.
	Embed(ctx context.Context, req EmbedRequest) (EmbedResult, error)
}

// EmbedRequest asks an embedding model for the vectors of up to 64 texts.
type EmbedRequest struct {
	Model string   `json:"model"`
	Texts []string `json:"texts"`
}

// EmbedResult holds one vector per text of the EmbedRequest and the prompt
// tokens the provider billed.
type EmbedResult struct {
	Vectors [][]float32 `json:"vectors"`
	Tokens  int         `json:"tokens,omitempty"`
}

// Classification kinds of a ClassifyRequest.
const (
	ClassifyYesNo  = "yes_no"
	ClassifyChoice = "choice"
	ClassifyScore  = "score"
)

// ClassifyRequest asks Model one question about Text. Options are the
// answers of a choice question.
type ClassifyRequest struct {
	Model        string   `json:"model"`
	Text         string   `json:"text"`
	Kind         string   `json:"kind"`
	Question     string   `json:"question"`
	Options      []string `json:"options,omitempty"`
	Instructions string   `json:"instructions,omitempty"`
}

// ClassifyResult is the verdict. Answer is "yes" or "no" for yes_no, the
// chosen option for choice and empty for score; Probability is that of yes,
// Score is in [0, 1].
type ClassifyResult struct {
	Answer      string  `json:"answer"`
	Probability float64 `json:"probability"`
	Choice      string  `json:"choice"`
	Score       float64 `json:"score"`
	Confidence  float64 `json:"confidence"`
	Model       string  `json:"model"`
	CostUSD     float64 `json:"cost_usd,omitempty"`
}

// Data residency of a model, from the regions of all its deployments.
const (
	ResidencyEU    = "eu"     // every deployment runs in the EU
	ResidencyNonEU = "non_eu" // no deployment runs in the EU
	ResidencyMixed = "mixed"  // some do, some do not
)

// ModelInfo is what the gateway knows about a model.
type ModelInfo struct {
	Name string `json:"name"`
	// Residency is ResidencyEU, ResidencyNonEU, ResidencyMixed, or empty when
	// the model has no deployments.
	Residency      string `json:"residency"`
	HasDeployments bool   `json:"has_deployments"`
	// Deployments counts the model's deployments; HealthyDeployments those
	// the router does not skip for repeated failures. A model with
	// deployments but none healthy is failing right now.
	Deployments        int `json:"deployments,omitempty"`
	HealthyDeployments int `json:"healthy_deployments,omitempty"`
	// LatencyMs is the recent mean response time of its deployments, 0 when
	// none has answered since the gateway started.
	LatencyMs float64 `json:"latency_ms,omitempty"`
}

// CompletionRequest is a single-turn chat completion.
type CompletionRequest struct {
	Model       string   `json:"model"`
	System      string   `json:"system,omitempty"`
	User        string   `json:"user"`
	MaxTokens   int      `json:"max_tokens,omitempty"`
	Temperature *float64 `json:"temperature,omitempty"`
}

// CompletionResult is the model's answer.
type CompletionResult struct {
	Text                string  `json:"text"`
	InputTokens         int     `json:"input_tokens,omitempty"`
	CachedInputTokens   int     `json:"cached_input_tokens,omitempty"`
	CacheCreationTokens int     `json:"cache_creation_tokens,omitempty"`
	OutputTokens        int     `json:"output_tokens,omitempty"`
	CostUSD             float64 `json:"cost_usd,omitempty"`
}

// Pricing exposes the gateway's effective, read-only price for a model. Model
// names are resolved through the same gateway alias and provider mapping as
// Models.Complete. A model without configured pricing returns ErrNotFound.
//
// Pricing deliberately has no write or spend-recording operation: the gateway
// accounts for genuine provider usage from Models.Complete itself, so an HTTP
// plugin cannot forge tenant spend or caller identity.
type Pricing interface {
	Model(ctx context.Context, model string) (ModelPrice, error)
}

// ModelPrice contains effective per-token rates in USD. Cache rates fall back
// to InputCostPerToken when the provider does not publish a distinct tariff.
type ModelPrice struct {
	InputCostPerToken           float64 `json:"input_cost_per_token"`
	OutputCostPerToken          float64 `json:"output_cost_per_token"`
	CacheReadInputTokenCost     float64 `json:"cache_read_input_token_cost"`
	CacheCreationInputTokenCost float64 `json:"cache_creation_input_token_cost"`
}

// --- Guard -------------------------------------------------------------------

// Guard runs the gateway's own detectors.
type Guard interface {
	// Check runs one check: "prompt_injection", "toxicity", "sensitive_data",
	// "moderation". ErrNotFound for a check the gateway does not have.
	Check(ctx context.Context, check, text string) (*GuardResult, error)
	// Redact masks what the gateway's redaction detectors find in text, for
	// logs and traces.
	Redact(ctx context.Context, text string) (string, error)
}

// GuardResult is the verdict of one check. Detail never contains the matched
// text.
type GuardResult struct {
	Flagged    bool     `json:"flagged"`
	Score      float64  `json:"score,omitempty"`
	Categories []string `json:"categories,omitempty"`
	Detail     string   `json:"detail,omitempty"`
}

// --- Groups ------------------------------------------------------------------

// Groups reads the Access groups a plugin may scope its objects to.
type Groups interface {
	// AccessGroups lists the Access groups; Request.Groups carries the ids of
	// the caller's groups.
	AccessGroups(ctx context.Context) ([]AccessGroup, error)
}

// AccessGroup is one Access group.
type AccessGroup struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Global bool   `json:"global"`
}

// --- Users -------------------------------------------------------------------

// Users reads the gateway's users, for a plugin that shows people by name.
type Users interface {
	// List returns every user (without the bootstrap owner).
	List(ctx context.Context) ([]User, error)
}

// User is a gateway user as a plugin sees it. Caller.UserID is its ID.
type User struct {
	ID         string `json:"id"`
	Email      string `json:"email"`
	Name       string `json:"name,omitempty"`
	Department string `json:"department,omitempty"`
	Active     bool   `json:"active"`
}

// --- Sessions ----------------------------------------------------------------

// Sessions reads the summary of a conversation. Without a traffic store it
// answers ErrNotFound.
type Sessions interface {
	// Get returns the session sessionID if it belongs to the key keyID
	// (Caller.KeyID of the request being handled); any other key, an unknown
	// session and an unavailable store are all ErrNotFound. The summary covers
	// the exchanges recorded so far, so not the request being handled.
	Get(ctx context.Context, sessionID, keyID string) (SessionInfo, error)
}

// SessionInfo is the aggregate of a session's recorded exchanges. It never
// holds prompt text; lists are capped at 200 entries.
type SessionInfo struct {
	ID        string    `json:"id"`
	StartedAt time.Time `json:"started_at"`
	// EndedAt is the time of the session's last recorded exchange.
	EndedAt  time.Time `json:"ended_at"`
	Requests int       `json:"requests"`
	Errors   int       `json:"errors"`
	// Client is the client product (claude-code, cursor, ...), Agent the agent.
	Client    string   `json:"client,omitempty"`
	Agent     string   `json:"agent,omitempty"`
	Models    []string `json:"models,omitempty"`
	Providers []string `json:"providers,omitempty"`

	Tokens       SessionTokens `json:"tokens"`
	CostUSD      float64       `json:"cost_usd"`
	LatencyP50Ms int64         `json:"latency_p50_ms"`
	LatencyP95Ms int64         `json:"latency_p95_ms"`
	CacheHits    int           `json:"cache_hits"`

	ToolCalls    int              `json:"tool_calls"`
	Tools        []SessionTool    `json:"tools,omitempty"`
	MCPServers   []SessionServer  `json:"mcp_servers,omitempty"`
	FilesRead    []string         `json:"files_read,omitempty"`
	FilesWritten []string         `json:"files_written,omitempty"`
	Findings     []SessionFinding `json:"findings,omitempty"`

	// PrimaryModel served the most successful turns; Tasks counts the pieces
	// of work the session was split into.
	PrimaryModel string `json:"primary_model,omitempty"`
	Tasks        int    `json:"tasks,omitempty"`
	// Savings are the tokens each optimisation technique removed.
	Savings map[string]int `json:"savings,omitempty"`
	// Plugins is what the pipeline plugins did over the session; nil for a
	// session summarised before it was recorded.
	Plugins *SessionPlugins `json:"plugins,omitempty"`
	// Last is the session's most recent turn, before the current request.
	Last SessionTurn `json:"last"`
}

// SessionTurn is the outcome of one recorded turn of a session.
type SessionTurn struct {
	Status   int       `json:"status"`
	Model    string    `json:"model,omitempty"`
	Provider string    `json:"provider,omitempty"`
	Error    string    `json:"error,omitempty"`
	At       time.Time `json:"at"`
}

// SessionPlugins counts, over a session's turns, what pipeline plugins did.
type SessionPlugins struct {
	// Steered counts the turns that carried the steering note.
	Steered int `json:"steered"`
	// Cache counts semantic cache verdicts: DIRECT, REUSE, TWEAK, MISS.
	Cache map[string]int `json:"cache,omitempty"`
	// Residency counts the turns served by "eu" and "non_eu" deployments.
	Residency map[string]int `json:"residency,omitempty"`
	// Annotations counts the key=value notes plugins attached, most frequent first.
	Annotations []SessionAnnotation `json:"annotations,omitempty"`
}

// SessionAnnotation counts one plugin annotation across a session.
type SessionAnnotation struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	Count int    `json:"count"`
}

// SessionTokens are the session's token counts. Saved is what live
// compression removed before sending.
type SessionTokens struct {
	Prompt     int `json:"prompt"`
	Completion int `json:"completion"`
	Total      int `json:"total"`
	Cached     int `json:"cached"`
	Reasoning  int `json:"reasoning"`
	Saved      int `json:"saved"`
}

// SessionTool counts the calls of one tool; Server is its MCP server, empty
// for a native tool.
type SessionTool struct {
	Name   string `json:"name"`
	Server string `json:"server,omitempty"`
	Count  int    `json:"count"`
}

// SessionServer counts the calls to one MCP server.
type SessionServer struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// SessionFinding counts the sensitive-data findings of one kind (secret,
// pii) and detector type, never their values.
type SessionFinding struct {
	Kind  string `json:"kind"`
	Type  string `json:"type"`
	Count int    `json:"count"`
}

// --- Plugins -----------------------------------------------------------------

// Plugins reaches the plugins that contribute to the contribution points this
// plugin declares in spec.points (the workflows plugin declares
// "workflow.node", and other plugins add node types to it).
type Plugins interface {
	// Contributions lists the contributions to one of the plugin's points,
	// from every installed plugin, with that plugin's instances.
	Contributions(ctx context.Context, point string) ([]PointContribution, error)
	// Invoke calls Process on an enabled instance of a contributing plugin.
	// The request goes through that instance's own timeout.
	Invoke(ctx context.Context, instanceID string, req *Request) (Ops, error)
}

// PointContribution is one contribution entry as written in the
// contributor's manifest (Raw is the whole spec.contributes item), plus the
// contributor's instances.
type PointContribution struct {
	PluginID  string           `json:"plugin_id"`
	Raw       json.RawMessage  `json:"raw"`
	Instances []PluginInstance `json:"instances"`
}

// PluginInstance is an instance of a contributing plugin.
type PluginInstance struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}

// --- Admin API ---------------------------------------------------------------

// APIHandler is implemented by the Handler of a plugin that contributes
// admin.api. The gateway serves /v1/plugins/<slug>/<path> by checking the
// console session or admin key and the route's permission, then calling
// ServeHTTP with the path relative to the slug (r.URL.Path starts with "/")
// and the caller in the context (see AdminFrom). A token route is checked by
// the plugin itself and carries TokenFrom instead. Over
// HTTP the request is forwarded signed to the plugin, and Serve does the same
// dispatch.
//
// The same handler serves the plugin's console app (console.app entries are
// fetched as GET /ui/<file>).
type APIHandler interface {
	http.Handler
}

// Admin is who made an admin API request.
type Admin struct {
	// UserID is empty for the master key.
	UserID      string   `json:"user_id,omitempty"`
	Email       string   `json:"email,omitempty"`
	Role        string   `json:"role"`
	Permissions []string `json:"permissions"`
	// Owner is true for the owner role and the master key.
	Owner bool `json:"owner,omitempty"`
}

// Actor names the caller for audit fields: the user id, or "master".
func (c *Admin) Actor() string {
	if c == nil || c.UserID == "" {
		return "master"
	}
	return c.UserID
}

// Has reports whether the caller holds permission.
func (c *Admin) Has(permission string) bool {
	if c == nil {
		return false
	}
	for _, p := range c.Permissions {
		if p == permission {
			return true
		}
	}
	return false
}

type adminKey struct{}

// WithAdmin returns ctx carrying a. The gateway and Serve set it.
func WithAdmin(ctx context.Context, a *Admin) context.Context {
	return context.WithValue(ctx, adminKey{}, a)
}

// AdminFrom returns the caller of an admin API request, nil outside one.
func AdminFrom(ctx context.Context) *Admin {
	a, _ := ctx.Value(adminKey{}).(*Admin)
	return a
}

// APIToken is a call to a token route (auth: token in spec.api.routes): the
// gateway checks no key or session and passes the request's bearer token,
// which the plugin checks against the tokens it issued itself. A collector on
// people's computers sends its data this way, with a key the admin created in
// the plugin.
type APIToken struct {
	// Bearer is the "Authorization: Bearer" value, empty without one.
	Bearer string `json:"bearer"`
}

type apiTokenKey struct{}

// WithToken returns ctx carrying t. The gateway and Serve set it.
func WithToken(ctx context.Context, t *APIToken) context.Context {
	return context.WithValue(ctx, apiTokenKey{}, t)
}

// TokenFrom returns the bearer of a token route, nil on any other request
// (AdminFrom has the console user there).
func TokenFrom(ctx context.Context) *APIToken {
	t, _ := ctx.Value(apiTokenKey{}).(*APIToken)
	return t
}
