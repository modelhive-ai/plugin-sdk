// Package codeguard masks or blocks customer codes and loyalty card numbers
// found in prompts, before they reach an LLM provider.
//
// The codes are recognised with regular expressions set by the administrator.
// In mask mode each code is replaced by a fixed token such as [carta_fedelta];
// in block mode the request is refused. The plugin keeps no state and its
// answer depends only on the text it is given, so the provider's prompt cache
// keeps matching across the turns of a conversation.
package codeguard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/modelhive-ai/plugin-sdk/pkg/plugin"
)

const (
	// Ref is the Definition.Ref and the manifest's spec.connector.ref.
	Ref = "codeguard"

	ModeMask  = "mask"
	ModeBlock = "block"

	// Limits of Ops.RedactSpans: the gateway refuses the request (503) when a
	// key exceeds maxKeyBytes or a request carries more than 4096 spans in
	// total, shared by every instance. Stay well below.
	maxKeyBytes = 4096
	maxSpans    = 1024
)

// PatternConfig is one kind of code to look for.
type PatternConfig struct {
	// Name labels the finding and gives the mask token: [name].
	Name  string `json:"name"`
	Regex string `json:"regex"`
}

// Config is the instance configuration (spec.config.schema in the manifest).
type Config struct {
	Mode              string          `json:"mode"`
	Patterns          []PatternConfig `json:"patterns"`
	IncludeToolOutput bool            `json:"include_tool_output"`
}

func defaultConfig() Config {
	return Config{
		Mode: ModeMask,
		Patterns: []PatternConfig{
			{Name: "carta_fedelta", Regex: `\bFID-?[0-9]{10}\b`},
			{Name: "codice_cliente", Regex: `\bCLI[0-9]{8}\b`},
		},
		IncludeToolOutput: true,
	}
}

var nameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

type pattern struct {
	name string
	mask string
	re   *regexp.Regexp
}

// Plugin is the handler of one instance. It is immutable after New, so
// Process is safe for concurrent use.
type Plugin struct {
	cfg      Config
	patterns []pattern
}

// New builds the handler of one instance. A bad configuration is an error:
// the gateway then reports the instance as failed instead of running it
// half configured.
func New(_ context.Context, in plugin.Instance) (plugin.Handler, error) {
	cfg := defaultConfig()
	if in.Config != nil {
		raw, err := json.Marshal(in.Config)
		if err != nil {
			return nil, fmt.Errorf("encode codeguard config: %w", err)
		}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, fmt.Errorf("decode codeguard config: %w", err)
		}
	}
	if cfg.Mode != ModeMask && cfg.Mode != ModeBlock {
		return nil, fmt.Errorf("codeguard config: mode must be %q or %q, got %q", ModeMask, ModeBlock, cfg.Mode)
	}
	if len(cfg.Patterns) == 0 {
		return nil, errors.New("codeguard config: at least one pattern is required")
	}
	p := &Plugin{cfg: cfg}
	seen := map[string]bool{}
	for i, pc := range cfg.Patterns {
		if !nameRe.MatchString(pc.Name) {
			return nil, fmt.Errorf("codeguard config: patterns[%d].name must match %s", i, nameRe)
		}
		if seen[pc.Name] {
			return nil, fmt.Errorf("codeguard config: duplicate pattern name %q", pc.Name)
		}
		seen[pc.Name] = true
		re, err := regexp.Compile(pc.Regex)
		if err != nil {
			return nil, fmt.Errorf("codeguard config: patterns[%d].regex: %w", i, err)
		}
		if re.MatchString("") {
			return nil, fmt.Errorf("codeguard config: patterns[%d].regex matches the empty string", i)
		}
		p.patterns = append(p.patterns, pattern{name: pc.Name, mask: "[" + pc.Name + "]", re: re})
	}
	return p, nil
}

// modeFor applies the access-group setting: the gateway sends in
// Request.GroupConfig only what the caller's groups change.
func (p *Plugin) modeFor(r *plugin.Request) string {
	if len(r.GroupConfig) == 0 {
		return p.cfg.Mode
	}
	merged := plugin.MergeConfig(map[string]any{"mode": p.cfg.Mode}, r.GroupConfig)
	if m, _ := merged["mode"].(string); m == ModeMask || m == ModeBlock {
		return m
	}
	return p.cfg.Mode
}

// Process looks for codes in the turns of the conversation and answers with
// Ops. It never modifies the request.
func (p *Plugin) Process(ctx context.Context, r *plugin.Request) (plugin.Ops, error) {
	if err := ctx.Err(); err != nil {
		return plugin.Ops{}, err
	}

	spans := map[string]string{} // literal code -> mask
	hits := map[string]int{}     // pattern name -> occurrences
	scan := func(text string) error {
		if text == "" {
			return nil
		}
		for _, pt := range p.patterns {
			for _, code := range pt.re.FindAllString(text, -1) {
				if len(code) > maxKeyBytes {
					return fmt.Errorf("pattern %q matched %d bytes, the limit is %d", pt.name, len(code), maxKeyBytes)
				}
				hits[pt.name]++
				// The gateway refuses a replacement that contains its key.
				if !strings.Contains(pt.mask, code) {
					spans[code] = pt.mask
				}
			}
		}
		return nil
	}
	for _, m := range r.Messages {
		if err := scan(m.Text); err != nil {
			return plugin.Ops{}, err
		}
		if p.cfg.IncludeToolOutput {
			if err := scan(m.ToolText); err != nil {
				return plugin.Ops{}, err
			}
		}
	}
	if len(hits) == 0 {
		return plugin.Ops{}, nil // zero Ops: the request goes through unchanged
	}

	names := make([]string, 0, len(hits))
	for name := range hits {
		names = append(names, name)
	}
	sort.Strings(names) // same input, same Ops: no map order in the answer
	findings := make([]plugin.Finding, 0, len(names))
	for _, name := range names {
		findings = append(findings, plugin.Finding{Kind: plugin.FindingKindPlugin, Type: "Codice aziendale: " + name})
	}
	total := 0
	for _, n := range hits {
		total += n
	}
	annotations := map[string]string{"codeguard.matches": strconv.Itoa(total)}

	if p.modeFor(r) == ModeBlock || len(spans) > maxSpans {
		kind, msg := "customer_code", "La richiesta contiene un codice cliente o una carta fedeltà e non può essere inviata al modello."
		if len(spans) > maxSpans {
			// Too many distinct codes to mask within the gateway's limit:
			// refusing is safer than letting some through.
			kind, msg = "customer_code_overflow", "La richiesta contiene troppi codici cliente da mascherare."
		}
		return plugin.Ops{
			Block:       &plugin.Block{Status: http.StatusForbidden, Kind: kind, Message: msg},
			Findings:    findings,
			Annotations: annotations,
		}, nil
	}
	annotations["codeguard.masked"] = strconv.Itoa(len(spans))
	return plugin.Ops{
		RedactSpans: spans,
		Findings:    findings,
		Annotations: annotations,
	}, nil
}

// Definition is what cmd/plugin-codeguard hands to plugin.Serve.
var Definition = plugin.Definition{
	Ref: Ref,
	New: New,
}

var _ plugin.Processor = (*Plugin)(nil)
