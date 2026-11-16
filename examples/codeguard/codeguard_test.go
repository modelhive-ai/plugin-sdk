package codeguard

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/modelhive-ai/plugin-sdk/pkg/plugin"
	"github.com/modelhive-ai/plugin-sdk/pkg/plugin/plugintest"
)

func userTurn(text string) plugin.Request {
	return plugin.Request{
		ID: "req-1", Format: "openai", Model: "gpt-test",
		Messages: []plugin.Message{{Role: "user", Text: text}},
	}
}

// Conformance runs each case in process and over unix:// and https:// and
// checks that the request is not mutated.
func TestConformance(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	plugintest.Run(t, Definition, plugin.Instance{Config: map[string]any{"mode": "mask"}},
		plugintest.ProcessCase{
			Name:    "text without codes passes unchanged",
			Request: userTurn("Come si configura il ritiro in negozio?"),
			Want:    plugin.Ops{},
		},
		plugintest.ProcessCase{
			Name:    "codes are masked",
			Request: userTurn("La carta FID-0123456789 del cliente CLI00112233 non funziona"),
			Want: plugin.Ops{
				RedactSpans: map[string]string{
					"FID-0123456789": "[carta_fedelta]",
					"CLI00112233":   "[codice_cliente]",
				},
				Findings: []plugin.Finding{
					{Kind: plugin.FindingKindPlugin, Type: "Codice aziendale: carta_fedelta"},
					{Kind: plugin.FindingKindPlugin, Type: "Codice aziendale: codice_cliente"},
				},
				Annotations: map[string]string{"codeguard.matches": "2", "codeguard.masked": "2"},
			},
		},
		plugintest.ProcessCase{
			Name:      "honors cancellation",
			Context:   canceled,
			Request:   userTurn("FID-0123456789"),
			WantError: context.Canceled.Error(),
		},
	)
}

func TestBlockMode(t *testing.T) {
	plugintest.Run(t, Definition, plugin.Instance{Config: map[string]any{"mode": "block"}},
		plugintest.ProcessCase{
			Name:    "a code blocks the request",
			Request: userTurn("Controlla CLI00112233"),
			Want: plugin.Ops{
				Block: &plugin.Block{
					Status:  http.StatusForbidden,
					Kind:    "customer_code",
					Message: "La richiesta contiene un codice cliente o una carta fedeltà e non può essere inviata al modello.",
				},
				Findings:    []plugin.Finding{{Kind: plugin.FindingKindPlugin, Type: "Codice aziendale: codice_cliente"}},
				Annotations: map[string]string{"codeguard.matches": "1"},
			},
		},
		plugintest.ProcessCase{
			Name:    "no code, no block",
			Request: userTurn("Nessun codice qui"),
			Want:    plugin.Ops{},
		},
	)
}

// The gateway sends the history on every request. A turn must be masked the
// same way whatever its position, or the provider's prompt cache breaks.
func TestSameTextSameSpansWhateverTheTurn(t *testing.T) {
	h, err := New(context.Background(), plugin.Instance{})
	if err != nil {
		t.Fatal(err)
	}
	p := h.(plugin.Processor)
	first := userTurn("Cliente CLI00112233, ordine in ritardo")
	second := plugin.Request{Messages: []plugin.Message{
		{Role: "user", Text: "Ciao"},
		{Role: "assistant", Text: "Come posso aiutarti?"},
		{Role: "user", Text: "Cliente CLI00112233, ordine in ritardo"},
	}}
	a, err := p.Process(context.Background(), &first)
	if err != nil {
		t.Fatal(err)
	}
	b, err := p.Process(context.Background(), &second)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a.RedactSpans, b.RedactSpans) {
		t.Fatalf("spans differ with the turn position: %v vs %v", a.RedactSpans, b.RedactSpans)
	}
}

func TestToolOutput(t *testing.T) {
	req := plugin.Request{Messages: []plugin.Message{
		{Role: "user", Text: "Cerca il cliente", ToolResult: false},
		{Role: "user", ToolResult: true, ToolText: `{"carta":"FID0123456789"}`},
	}}
	for _, include := range []bool{true, false} {
		h, err := New(context.Background(), plugin.Instance{Config: map[string]any{"include_tool_output": include}})
		if err != nil {
			t.Fatal(err)
		}
		ops, err := h.(plugin.Processor).Process(context.Background(), &req)
		if err != nil {
			t.Fatal(err)
		}
		got := ops.RedactSpans["FID0123456789"] == "[carta_fedelta]"
		if got != include {
			t.Errorf("include_tool_output=%v: masked=%v", include, got)
		}
	}
}

// An access group can tighten the mode: the gateway sends only the settings
// the caller's groups changed.
func TestGroupConfigTightensMode(t *testing.T) {
	h, err := New(context.Background(), plugin.Instance{Config: map[string]any{"mode": "mask"}})
	if err != nil {
		t.Fatal(err)
	}
	req := userTurn("CLI00112233")
	req.GroupConfig = map[string]any{"mode": "block"}
	ops, err := h.(plugin.Processor).Process(context.Background(), &req)
	if err != nil {
		t.Fatal(err)
	}
	if ops.Block == nil || ops.Block.Kind != "customer_code" {
		t.Fatalf("Ops = %#v, want a block", ops)
	}
}

func TestTooManyCodesIsBlocked(t *testing.T) {
	var b strings.Builder
	for i := 0; i <= maxSpans; i++ {
		fmt.Fprintf(&b, "CLI%08d ", i)
	}
	h, err := New(context.Background(), plugin.Instance{})
	if err != nil {
		t.Fatal(err)
	}
	req := userTurn(b.String())
	ops, err := h.(plugin.Processor).Process(context.Background(), &req)
	if err != nil {
		t.Fatal(err)
	}
	if ops.Block == nil || ops.Block.Kind != "customer_code_overflow" || len(ops.RedactSpans) != 0 {
		t.Fatalf("Ops = %#v, want an overflow block", ops)
	}
}

func TestNewRejectsBadConfig(t *testing.T) {
	for name, cfg := range map[string]map[string]any{
		"unknown mode": {"mode": "log"},
		"bad regex":    {"patterns": []any{map[string]any{"name": "x", "regex": "("}}},
		"empty match":  {"patterns": []any{map[string]any{"name": "x", "regex": "a*"}}},
		"bad name":     {"patterns": []any{map[string]any{"name": "a b", "regex": "a"}}},
		"duplicate":    {"patterns": []any{map[string]any{"name": "x", "regex": "a"}, map[string]any{"name": "x", "regex": "b"}}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := New(context.Background(), plugin.Instance{Config: cfg}); err == nil {
				t.Fatal("New() accepted the configuration")
			}
		})
	}
}
