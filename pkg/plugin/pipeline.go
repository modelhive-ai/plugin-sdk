package plugin

import (
	"context"
	"net/http"
	"time"
)

// Phase is where in the request path a Processor runs. Phases run in this
// fixed order; within a phase, instances run in the order the operator set.
// A plugin declares its phase in the manifest (spec.pipeline.phase), not in
// code, so the gateway can order instances without calling them.
type Phase string

const (
	// PhaseGuard decides whether the request may proceed: block, answer, or
	// let through. Guard instances should not rewrite.
	PhaseGuard Phase = "guard"
	// PhaseTransform rewrites the request: system notes, prompt rewrites.
	PhaseTransform Phase = "transform"
	// PhaseRoute picks the model.
	PhaseRoute Phase = "route"
)

// Request is a plugin's view of an LLM request, the same whatever API it
// arrived on (Anthropic Messages, OpenAI Chat, Responses, Gemini, Kiro).
// Plugins never see or return the provider body: they read this and answer
// with Ops, and the gateway applies them to the original format. That keeps
// the bytes a plugin did not touch identical, so provider prompt caches keep
// matching.
type Request struct {
	// ID is the gateway request ID, for correlating logs and output windows.
	ID string `json:"id"`
	// Format is the wire API: "anthropic", "openai", "completions", "responses",
	// "gemini", "kiro", or "raw" for a request the gateway cannot read. A raw request has
	// no Messages and only reaches guard instances that list "raw" in their
	// formats; of the answer, only Block, Annotations and Findings apply.
	Format string `json:"format"`
	Path   string `json:"path"`
	// Method is the HTTP method of the client's request.
	Method string `json:"method,omitempty"`
	// Headers are the client's request headers, one value per name (comma
	// joined). Credentials (authorization, cookies, API keys) arrive as
	// "[REDACTED]".
	Headers map[string]string `json:"headers,omitempty"`
	// RemoteAddr is the client's address as the gateway saw it.
	RemoteAddr string `json:"remote_addr,omitempty"`
	// Model is the model asked for, "" when the request names none.
	Model  string `json:"model,omitempty"`
	Stream bool   `json:"stream"`
	// System is the system prompt text, "" when absent.
	System string `json:"system,omitempty"`
	// Messages are the turns in order, system excluded. Ops refer to them by
	// index; the gateway never adds, drops or reorders them.
	Messages []Message `json:"messages"`
	Caller   Caller    `json:"caller"`
	// SessionID is the conversation the request belongs to, as AI traffic
	// will record it; the request ID when nothing names a conversation.
	SessionID string `json:"session_id,omitempty"`
	// Tools are the tool, MCP and file facts the request itself carries;
	// nil on a raw request.
	Tools *RequestTools `json:"tools,omitempty"`
	// GroupConfig is what the caller's access groups change in this
	// instance's config: only the group settings of the manifest whose value
	// differs, already merged across the groups. Nil when nothing changes.
	// Apply it with MergeConfig.
	GroupConfig map[string]any `json:"group_config,omitempty"`
	// Node is set when a workflow node calls the processor instead of the
	// request pipeline; nil on pipeline calls.
	Node *NodeCall `json:"node,omitempty"`
	// Task is the work the request belongs to, as the task judge sees it,
	// with this instance's answers (see TaskAsker). Nil when the pipeline has
	// no task asker or the request is not part of a task (a side call).
	Task *Task `json:"task,omitempty"`
}

// RequestTools is what a request says about tools, read from its body.
// Lists are bounded (200 declared tools, 500 calls, 500 files); calls and
// files keep the most recent.
type RequestTools struct {
	// Declared are the tools offered to the model; Name is empty when only
	// the MCP server is known (a Responses "mcp" tool without allowed_tools).
	Declared []ExchangeTool `json:"declared,omitempty"`
	// Calls are the tool calls already in the conversation history.
	Calls []ExchangeTool `json:"calls,omitempty"`
	// Files are the paths those calls touched, with operation read, write or
	// search.
	Files []ExchangeFile `json:"files,omitempty"`
}

// MergeConfig returns base with over applied: objects merge key by key, any
// other value replaces. Neither argument is modified.
func MergeConfig(base, over map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(over))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		if om, ok := v.(map[string]any); ok {
			if bm, ok := out[k].(map[string]any); ok {
				out[k] = MergeConfig(bm, om)
				continue
			}
		}
		out[k] = v
	}
	return out
}

// NodeCall describes the workflow node that invoked a Processor. The
// request's last user message holds the workflow's current prompt text.
type NodeCall struct {
	// Type is the contributed node type (spec.contributes), or "plugin" for
	// the generic node.
	Type string `json:"type"`
	// ID is the node's id in the workflow graph.
	ID string `json:"id"`
	// Config is the node's configuration with {{...}} references resolved.
	Config map[string]any `json:"config,omitempty"`
	// Vars are the workflow variables at the time of the call.
	Vars map[string]any `json:"vars,omitempty"`
	// Exchange is the ID of the gateway request the workflow runs for, empty
	// in a test run. State a node keeps for the answer (pseudonym tokens) is
	// keyed by it, as OutputWindow.SessionID is.
	Exchange string `json:"exchange,omitempty"`
}

// Message is one turn reduced to its text.
type Message struct {
	// Role is "user", "assistant" or "tool".
	Role string `json:"role"`
	// Text is the turn's text; "" for a turn without any (image only).
	Text string `json:"text"`
	// ToolResult marks a user turn that only returns tool output.
	ToolResult bool `json:"tool_result,omitempty"`
	// ToolText is tool output carried inside a user turn. A "tool" turn has
	// its output in Text. Read-only: ReplaceText does not change it.
	ToolText string `json:"tool_text,omitempty"`
}

// LastUser returns the index of the last turn a person typed, or -1.
func (r *Request) LastUser() int {
	for i := len(r.Messages) - 1; i >= 0; i-- {
		m := r.Messages[i]
		if m.Role == "user" && !m.ToolResult && m.Text != "" {
			return i
		}
	}
	return -1
}

// Caller is who sent the request, as the gateway authenticated it.
type Caller struct {
	KeyID string `json:"key_id,omitempty"`
	// KeyName is the name the key was given.
	KeyName string `json:"key_name,omitempty"`
	UserID  string `json:"user_id,omitempty"`
	TeamID  string `json:"team_id,omitempty"`
	// Groups are the access groups of the key, by name.
	Groups []string `json:"groups,omitempty"`
	// GroupID is the id of the key's access group, "" when it has none. Unlike
	// a name it survives a rename, so it is what to partition state by.
	GroupID string `json:"group_id,omitempty"`
	// Agent is the client the gateway recognised (claude-code, codex, kiro…).
	Agent string `json:"agent,omitempty"`
	// Steering is whether the caller asked for terse-output steering: "on" or
	// "off" from the request header, "" when it did not and the instance's own
	// setting applies.
	Steering string `json:"steering,omitempty"`
	// SteeringPreset is the steering preset the request header named:
	// "concise", "coding" or "coding-1".."coding-3"; "" follows the instance.
	SteeringPreset string `json:"steering_preset,omitempty"`
	// Workflow is the workflow slug named by the request header, "" when none.
	Workflow string `json:"workflow,omitempty"`
	// DefaultWorkflow is the workflow id configured by the caller's access groups.
	DefaultWorkflow string `json:"default_workflow,omitempty"`
	// ResponseWorkflow is the on_response workflow slug named by the request
	// header, "" when none.
	ResponseWorkflow string `json:"response_workflow,omitempty"`
	// DefaultResponseWorkflow is the on_response workflow id configured by the
	// caller's access groups.
	DefaultResponseWorkflow string `json:"default_response_workflow,omitempty"`
	// Budget is the key's own spending limit, nil when the key has none.
	Budget *CallerBudget `json:"budget,omitempty"`
}

// CallerBudget is a key's spending limit and what it had spent when the
// request arrived, in USD. Spent lags in-flight requests by a few seconds.
type CallerBudget struct {
	LimitUSD float64 `json:"limit_usd"`
	SpentUSD float64 `json:"spent_usd"`
}

// Ops is a Processor's answer. The zero value lets the request through
// unchanged. Block and Respond end the request: later instances do not run
// and the provider is not called.
type Ops struct {
	Block   *Block   `json:"block,omitempty"`
	Respond *Respond `json:"respond,omitempty"`
	// AppendSystem notes are added at the end of the system prompt, in
	// order. Keep them constant: a note that varies per request moves every
	// token after it and costs a prompt-cache miss each time.
	AppendSystem []string `json:"append_system,omitempty"`
	// ReplaceText sets the text of Request.Messages[i], keeping its images
	// and tool calls.
	ReplaceText map[int]string `json:"replace_text,omitempty"`
	// RedactSpans replaces literal substrings in every user-visible message
	// text and tool output in the native request body. It never changes system
	// prompts, tool definitions, identifiers, or keys. The gateway accepts at
	// most 4096 entries, each key must be 1..4096 bytes, and a replacement may
	// not contain its key; anything else refuses the request with 503.
	RedactSpans map[string]string `json:"redact_spans,omitempty"`
	// Model routes the request to another model.
	Model string `json:"model,omitempty"`
	// Annotations are recorded on the request's trace and metrics. Never put
	// prompt content or secrets here.
	Annotations map[string]string `json:"annotations,omitempty"`
	// Findings are what the plugin detected, reported in AI traffic under
	// their own kind and type instead of the generic plugin entry. A finding
	// returned with a Block is recorded as blocked; one returned without, or
	// with a Block that only applies when Access enforces and does not, as
	// monitored.
	Findings []Finding `json:"findings,omitempty"`
	// Savings are what the plugin removed from the request, for AI traffic.
	Savings []Saving `json:"savings,omitempty"`
	// Values are what a workflow node call (Request.Node set) writes into the
	// run's context, by dotted path. Only paths under "scores." and
	// "findings." are accepted; ignored outside node calls.
	Values map[string]any `json:"values,omitempty"`
	// CostUSD is what a workflow node call spent on paid model or classifier
	// calls (models/complete included), for the run's cost. The gateway bills nothing from it.
	CostUSD float64 `json:"cost_usd,omitempty"`
}

// Finding kinds a plugin may report. Any other Kind is recorded as
// FindingKindPlugin: a plugin cannot pose as the secret or PII detectors.
const (
	// FindingKindSafety is a content-safety category (injection, violence...).
	FindingKindSafety = "safety"
	// FindingKindPlugin is a generic finding attributed to the plugin.
	FindingKindPlugin = "plugin"
)

// Finding is one detection. It carries no matched text: AI traffic keeps
// findings by kind and type only.
type Finding struct {
	// Kind is FindingKindSafety or FindingKindPlugin; "" means plugin.
	Kind string `json:"kind,omitempty"`
	// Type names what was found, e.g. "Prompt injection or jailbreak".
	Type string `json:"type"`
}

// Block refuses the request.
type Block struct {
	// Status is the HTTP status; 0 means 403.
	Status int `json:"status,omitempty"`
	// Kind is a short machine-readable reason, e.g. "prompt_injection".
	Kind string `json:"kind"`
	// Message is shown to the caller.
	Message string `json:"message"`
	// OnlyIfEnforcing makes the block follow the Access policy: the request
	// is refused only when Access enforces, and otherwise goes through with
	// the plugin's Findings recorded as monitored. Later instances then run
	// as if the plugin had passed.
	OnlyIfEnforcing bool `json:"only_if_enforcing,omitempty"`
}

// Respond answers the request without calling a provider. The gateway writes
// Text in the caller's format, streamed if the caller asked to stream.
type Respond struct {
	Text string `json:"text"`
	// Header is added to the synthetic answer, e.g. which workflow gave it.
	Header http.Header `json:"-"`
}

// Processor shapes or stops requests. Process must be safe for concurrent use
// and should return within the instance's declared timeout. An error is
// handled by the instance's fail-open setting: let the request through, or
// refuse it.
type Processor interface {
	Process(ctx context.Context, r *Request) (Ops, error)
}

// Exchange is a finished request with its outcome.
type Exchange struct {
	// Time is when the exchange finished; zero means the observer's clock.
	Time    time.Time `json:"time,omitzero"`
	Request *Request  `json:"request"`
	// Status is the HTTP status returned to the caller.
	Status int `json:"status"`
	// Output is the answer text, "" when the request failed or was blocked.
	Output       string        `json:"output,omitempty"`
	Provider     string        `json:"provider,omitempty"`
	InputTokens  int           `json:"input_tokens,omitempty"`
	OutputTokens int           `json:"output_tokens,omitempty"`
	CostUSD      float64       `json:"cost_usd,omitempty"`
	Duration     time.Duration `json:"duration_ns"`
	// Annotations collected from every instance on the way.
	Annotations map[string]string `json:"annotations,omitempty"`
	// SessionID and TaskID place the exchange in its conversation and task.
	SessionID string `json:"session_id,omitempty"`
	TaskID    string `json:"task_id,omitempty"`
	// Client and ClientVersion are what the caller's software said it is;
	// ClientIP is where the request came from.
	Client        string `json:"client,omitempty"`
	ClientVersion string `json:"client_version,omitempty"`
	ClientIP      string `json:"client_ip,omitempty"`
	// CachedInputTokens were served from the provider's prompt cache;
	// SavedTokens were removed by the gateway before sending.
	CachedInputTokens int           `json:"cached_input_tokens,omitempty"`
	ReasoningTokens   int           `json:"reasoning_tokens,omitempty"`
	SavedTokens       int           `json:"saved_tokens,omitempty"`
	TTFB              time.Duration `json:"ttfb_ns,omitempty"`
	// BillingMode is "flat" when the caller's own subscription paid.
	BillingMode string `json:"billing_mode,omitempty"`
	// Residency is "eu" or "non_eu", where the provider ran it.
	Residency string `json:"residency,omitempty"`
	// CacheStatus is the semantic cache's outcome: hit, miss or bypass.
	CacheStatus string `json:"cache_status,omitempty"`
	// Findings are what the detectors and policies found, without the values.
	Findings []ExchangeFinding `json:"findings,omitempty"`
	// Tools are the calls the model asked for, without their arguments.
	Tools []ExchangeTool `json:"tools,omitempty"`
	// Files are the paths those calls touched.
	Files []ExchangeFile `json:"files,omitempty"`
	// Error is the message of a failed exchange.
	Error string `json:"error,omitempty"`
	// ClientHost is the caller's machine name when the client declared it.
	ClientHost string `json:"client_host,omitempty"`
	// Steered is set when the terse-output note was added to the request.
	Steered bool `json:"steered,omitempty"`
	// CacheSavedTokens is the size of the call a semantic-cache answer avoided;
	// NetSavedTokens is what compression saved after the cache it broke.
	CacheSavedTokens int `json:"cache_saved_tokens,omitempty"`
	NetSavedTokens   int `json:"net_saved_tokens,omitempty"`
	// Savings break SavedTokens down by technique.
	Savings []ExchangeSaving `json:"savings,omitempty"`
	// Quota is the subscription usage the provider reported with this answer.
	Quota *ExchangeQuota `json:"quota,omitempty"`
}

// ExchangeSaving is what one technique removed from the request.
type ExchangeSaving struct {
	Technique         string  `json:"technique"`
	Tokens            int     `json:"tokens"`
	ExtractionTokens  int     `json:"extraction_tokens,omitempty"`
	ExtractionCostUSD float64 `json:"extraction_cost_usd,omitempty"`
	Failures          int     `json:"failures,omitempty"`
}

// ExchangeQuota is a subscription's usage as its provider reported it.
type ExchangeQuota struct {
	Source       string                `json:"source"`
	Plan         string                `json:"plan,omitempty"`
	LimitReached bool                  `json:"limit_reached,omitempty"`
	Windows      []ExchangeQuotaWindow `json:"windows,omitempty"`
}

// ExchangeQuotaWindow is one rolling usage window.
type ExchangeQuotaWindow struct {
	Limit       string  `json:"limit"`
	Slot        string  `json:"slot"`
	UsedPercent float64 `json:"used_percent"`
	// ResetsAt is a Unix time in seconds.
	ResetsAt int64 `json:"resets_at,omitempty"`
}

// ExchangeFinding is one kind of detection in an exchange.
type ExchangeFinding struct {
	// Kind is secret, pii, safety, watchlist, access or plugin.
	Kind string `json:"kind"`
	Type string `json:"type"`
	// Origin is request or response.
	Origin string `json:"origin,omitempty"`
	Count  int    `json:"count"`
	// Action is blocked, redacted, restored or monitored; "" means reported.
	Action string `json:"action,omitempty"`
}

// ExchangeTool is a tool call the model asked for.
type ExchangeTool struct {
	Name string `json:"name"`
	// Server is the MCP server the tool belongs to, "" for native tools.
	Server string `json:"server,omitempty"`
	Source string `json:"source,omitempty"`
	// Args are a call's arguments as JSON text, cut to MaxToolArgs bytes; ""
	// for declared tools.
	Args string `json:"args,omitempty"`
}

// MaxToolArgs caps the arguments kept per tool call.
const MaxToolArgs = 8 << 10

// ExchangeFile is a path a tool call touched.
type ExchangeFile struct {
	Path      string `json:"path"`
	Operation string `json:"operation"`
}

// Observer sees each finished exchange. It is called asynchronously, off the
// request path: it can export, audit or alert, never change the outcome.
// Exchanges may be dropped under backpressure.
type Observer interface {
	Observe(ctx context.Context, e *Exchange) error
}

// Response hooks (OutputGuard, ResponseTap) are defined in response.go.
// They run after the provider answers, in reverse order of the request chain.
