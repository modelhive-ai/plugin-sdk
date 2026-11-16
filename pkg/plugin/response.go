package plugin

import (
	"context"
	"net/http"
	"time"
)

// Response hooks are optional: a Processor that implements neither
// OutputGuard nor ResponseTap runs on requests only, as before.
//
// An instance sees the answer to a request only if the request reached it (in
// scope, of a format it takes) and went on to a provider or the cache: a
// request a plugin blocked or answered has no provider answer. Hooks run in
// the reverse of the request order, route first and guard last, so the guard
// that saw the original request is the last to touch the answer, like nested
// middleware. Only 2xx answers in a format the gateway reads are windowed;
// taps see every answer.

// Output windowing, set per plugin in the manifest (spec.pipeline.output.window).
// A non-streamed answer is always one Final window, whatever the setting.
const (
	// WindowChunk passes each piece of text as the provider streams it,
	// holding nothing back. The default for compiled plugins.
	WindowChunk = "chunk"
	// WindowSentence holds text up to a sentence end or a size bound. The
	// default for HTTP plugins, where a call per chunk costs too much.
	WindowSentence = "sentence"
	// WindowFull holds the whole answer for one window: the caller receives
	// nothing until the guard answered. For judges that need the full text.
	WindowFull = "full"
)

// OutputWindow is a slice of a model's answer text. Only text is windowed:
// tool calls and other events keep their place in the stream, held back while
// text before them is.
type OutputWindow struct {
	// SessionID is stable across all windows of one answer.
	SessionID string `json:"session_id"`
	// Request is the request as forwarded, edits applied. In process it is
	// set on every window; over HTTP only on Seq 0, so keep what you need by
	// SessionID.
	Request *Request `json:"request,omitempty"`
	// Seq numbers the windows of one answer from 0.
	Seq int `json:"seq"`
	// Text is the output since the previous window, as the instances after
	// this one in request order left it.
	Text string `json:"text"`
	// Final is set on the last window, after the answer ended. With chunk
	// windows its Text is often "".
	Final bool `json:"final"`
	// Usage is the provider's count, on the Final window when it sent one.
	Usage *Usage `json:"usage,omitempty"`
}

// OutputVerdict is an OutputGuard's answer. The zero value lets the window
// through.
type OutputVerdict struct {
	// Block stops the answer. Before anything was sent the caller gets
	// Block.Status in its format; mid-stream the status is already 200, so
	// the stream ends with an error event in its format instead. A block
	// with OnlyIfEnforcing follows the Access policy as on requests.
	Block *Block `json:"block,omitempty"`
	// Replace, when set, is sent instead of the window's text; later hooks
	// see the replacement.
	Replace *string `json:"replace,omitempty"`
	// HoldTail, on a window of a streamed answer, holds the events that
	// follow its text (a tool call, the stream's closing events) until the
	// next window. A Final window's Replace then goes out as a new text event
	// before them, even when the window had no text of its own. Without it
	// those events pass as they come, and a Final Replace with no text to
	// carry it is lost.
	HoldTail bool `json:"hold_tail,omitempty"`
	// Annotations as in Ops.
	Annotations map[string]string `json:"annotations,omitempty"`
	// Findings as in Ops, recorded with a response origin.
	Findings []Finding `json:"findings,omitempty"`
}

// OutputGuard checks or rewrites a model's answer as it is produced.
// CheckOutput is called once per window, in order, never concurrently for the
// same SessionID. An error, panic or timeout (the instance's timeout, per
// window) is handled by the instance's fail-open setting: open, the window
// goes through unchanged and the instance gets no more windows of this
// answer; closed, the answer is stopped as by a Block with status 503.
type OutputGuard interface {
	CheckOutput(ctx context.Context, w *OutputWindow) (OutputVerdict, error)
}

// Usage is what the provider reported for one answer.
type Usage struct {
	InputTokens int `json:"input_tokens"`
	// CachedInputTokens and CacheCreationTokens are parts of InputTokens.
	CachedInputTokens   int     `json:"cached_input_tokens,omitempty"`
	CacheCreationTokens int     `json:"cache_creation_tokens,omitempty"`
	OutputTokens        int     `json:"output_tokens"`
	CostUSD             float64 `json:"cost_usd,omitempty"`
}

// ResponseTap sees the answer's bytes exactly as the caller receives them,
// after every OutputGuard. It cannot change them. Compiled plugins only: the
// HTTP engine never copies response bytes to a plugin.
//
// TapResponse is called when the status is known; a nil sink skips the
// answer at no cost. Errors are the tap's own to handle and panics are
// recovered: a tap never fails the request.
type ResponseTap interface {
	TapResponse(ctx context.Context, r *Request, status int, header http.Header) ResponseSink
}

// ResponseSink receives one answer. Write is called inline with the stream
// and must not block or keep p: copy what you need and return.
type ResponseSink interface {
	Write(p []byte)
	// Close is called once, after the last Write, off the request path.
	Close(end *ResponseEnd)
}

// ResponseEnd is how an answer finished.
type ResponseEnd struct {
	Status int
	// Usage is nil when the provider reported none.
	Usage *Usage
	// Complete is false when the answer was cut short: the client went away,
	// a guard stopped it, or the provider broke off.
	Complete bool
	Duration time.Duration
}
