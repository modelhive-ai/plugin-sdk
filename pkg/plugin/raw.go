package plugin

import (
	"context"
	"net/http"
)

// RawProcessor is a compiled-only extension of Processor for plugins that
// must restructure the provider body itself: compressors that summarise
// history, shorten tool output or drop turns, which Ops cannot express. When
// a handler implements it the gateway calls ProcessRaw instead of Process.
// The HTTP engine never offers it: a served plugin would copy the whole body
// twice per request (docs/BENCH-plugin-http.md).
//
// Body is the request as the instances before this one left it, in the
// format named by Request.Format, already edited. The gateway re-reads the
// returned body for the instances after this one; a body it cannot read is
// handled like an error, by the instance's fail-open setting. Keep bytes you
// did not mean to change identical, or provider prompt caches stop matching.
type RawProcessor interface {
	ProcessRaw(ctx context.Context, r *Request, body []byte) (RawOps, error)
}

// RawOps is a RawProcessor's answer: Ops, plus the rewritten body.
type RawOps struct {
	Ops
	// Body replaces the request body; nil keeps it. It must stay in the
	// request's format. Edits in Ops are applied on top of it.
	Body []byte `json:"-"`
	// Response, when set, answers the request with these exact bytes instead
	// of calling the provider (a cache hit replayed in the caller's format).
	// Later instances do not run. The answer passes through the response
	// hooks of the instances that already ran, as a provider answer would.
	Response *RawResponse `json:"-"`
	// ResponseHeader is added to the caller's response, whoever answers it.
	// Framing, hop-by-hop and cookie headers are dropped.
	ResponseHeader http.Header `json:"-"`
}

// RawResponse is an answer a RawProcessor serves itself.
type RawResponse struct {
	// Status is the HTTP status; 0 means 200.
	Status int
	Header http.Header
	Body   []byte
}

// Saving is what a plugin removed from one request, shown in AI traffic next
// to the gateway's own figures. Tokens are the plugin's estimate: the
// provider only ever counts what it received.
type Saving struct {
	// Technique names the method, e.g. "json_truncate", "state_compression".
	Technique string `json:"technique"`
	Blocks    int    `json:"blocks,omitempty"`
	Tokens    int    `json:"tokens"`
	// ExtractionTokens and ExtractionCostUSD are what the plugin spent to make
	// the saving (a summarising model call); AI traffic reports the saving net
	// of them.
	ExtractionTokens  int     `json:"extraction_tokens,omitempty"`
	ExtractionCostUSD float64 `json:"extraction_cost_usd,omitempty"`
	// Failures counts attempts that failed and left the request as it was.
	Failures int `json:"failures,omitempty"`
}
