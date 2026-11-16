package plugincompress

import (
	"context"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/modelhive-ai/plugin-sdk/pkg/plugin"
	"github.com/modelhive-ai/plugin-sdk/pkg/plugin/plugintest"
)

func toolTurn(text string) plugin.Message {
	return plugin.Message{Role: "user", ToolResult: true, ToolText: text}
}

func request(msgs ...plugin.Message) plugin.Request {
	return plugin.Request{ID: "req-1", Format: "anthropic", Model: "claude-test", Messages: msgs}
}

func newPlugin(t *testing.T, cfg map[string]any) *Plugin {
	t.Helper()
	h, err := New(context.Background(), plugin.Instance{Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	return h.(*Plugin)
}

func process(t *testing.T, p *Plugin, r plugin.Request) plugin.Ops {
	t.Helper()
	ops, err := p.Process(context.Background(), &r)
	if err != nil {
		t.Fatal(err)
	}
	return ops
}

// apply returns the tool outputs of r as the gateway would forward them.
func apply(r plugin.Request, ops plugin.Ops) []string {
	keys := orderedKeys(ops.RedactSpans)
	var out []string
	for _, m := range r.Messages {
		out = append(out, applySpans(m.ToolText, keys, ops.RedactSpans))
	}
	return out
}

func checkSpans(t *testing.T, ops plugin.Ops) {
	t.Helper()
	if err := plugintest.ValidateOps(ops); err != nil {
		t.Fatal(err)
	}
	if len(ops.RedactSpans) > maxSpans {
		t.Errorf("%d spans, want at most %d", len(ops.RedactSpans), maxSpans)
	}
	for key, repl := range ops.RedactSpans {
		if key == "" || len(key) > maxSpanBytes || strings.Contains(repl, key) || !utf8.ValidString(key) {
			t.Errorf("span %q -> %q would be refused by the gateway", key, repl)
		}
	}
}

const repeatedLine = "connection refused\n"

func TestConformance(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	repeated := "start\n" + strings.Repeat(repeatedLine, 10) + "end\n"
	spaced := "a\n\n\n\n\n\nb" + strings.Repeat(" ", 20) + "c"

	plugintest.Run(t, Definition, plugin.Instance{},
		plugintest.ProcessCase{
			Name:    "collapses repeated lines",
			Request: request(toolTurn(repeated)),
			Want: plugin.Ops{
				RedactSpans: map[string]string{
					strings.Repeat(repeatedLine, 10): repeatedLine + "[repeated 9 more times]\n",
				},
				Annotations: map[string]string{AnnotationBytesSaved: strconv.Itoa(190 - 43)},
			},
		},
		plugintest.ProcessCase{
			Name:    "collapses blank and space runs",
			Request: request(toolTurn(spaced)),
			Want: plugin.Ops{
				RedactSpans: map[string]string{
					"\n\n\n\n\n\n":          "\n\n",
					strings.Repeat(" ", 20): " ",
				},
				Annotations: map[string]string{AnnotationBytesSaved: "23"},
			},
		},
		plugintest.ProcessCase{
			Name:    "reads the output of a tool turn",
			Request: request(plugin.Message{Role: "tool", Text: repeated}),
			Want: plugin.Ops{
				RedactSpans: map[string]string{
					strings.Repeat(repeatedLine, 10): repeatedLine + "[repeated 9 more times]\n",
				},
				Annotations: map[string]string{AnnotationBytesSaved: strconv.Itoa(190 - 43)},
			},
		},
		plugintest.ProcessCase{
			Name:    "leaves small clean output alone",
			Request: request(toolTurn("one\ntwo\nthree\n")),
			Want:    plugin.Ops{},
		},
		plugintest.ProcessCase{
			Name:    "leaves what the user typed alone",
			Request: request(plugin.Message{Role: "user", Text: strings.Repeat(repeatedLine, 10)}),
			Want:    plugin.Ops{},
		},
		plugintest.ProcessCase{
			Name:      "honors cancellation",
			Context:   canceled,
			WantError: context.Canceled.Error(),
		},
	)
}

func TestTrimsLongOutputToHeadAndTail(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 1000; i++ {
		fmt.Fprintf(&b, "line %04d: some log content that differs per line\n", i)
	}
	text := b.String()
	r := request(toolTurn(text))
	ops := process(t, newPlugin(t, nil), r)
	checkSpans(t, ops)

	got := apply(r, ops)[0]
	if !strings.HasPrefix(got, "line 0000:") || !strings.HasSuffix(got, "line 0999: some log content that differs per line\n") {
		t.Fatalf("head or tail lost:\n%.80s ... %.80s", got, got[len(got)-80:])
	}
	if !strings.Contains(got, " bytes omitted ...]\n") {
		t.Fatal("no marker where the middle was cut")
	}
	if want := len(text) - len(got); ops.Annotations[AnnotationBytesSaved] != strconv.Itoa(want) {
		t.Errorf("bytes_saved = %s, want %d", ops.Annotations[AnnotationBytesSaved], want)
	}
	if len(got) > defaultConfig().HeadBytes+defaultConfig().TailBytes+100 {
		t.Errorf("kept %d bytes of %d", len(got), len(text))
	}
	if len(ops.RedactSpans) < 2 {
		t.Errorf("a %d byte middle should need several %d byte spans, got %d", len(text), maxSpanBytes, len(ops.RedactSpans))
	}
}

func TestTrimKeepsUTF8WithoutLineBreaks(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 8000; i++ {
		fmt.Fprintf(&b, "é€%d", i)
	}
	text := b.String()
	r := request(toolTurn(text))
	ops := process(t, newPlugin(t, nil), r)
	checkSpans(t, ops)
	got := apply(r, ops)[0]
	if !utf8.ValidString(got) || len(got) >= len(text) {
		t.Fatalf("got %d valid=%v bytes from %d", len(got), utf8.ValidString(got), len(text))
	}
}

func TestSameOutputSameSpansWhateverTheTurn(t *testing.T) {
	p := newPlugin(t, nil)
	long := strings.Repeat(repeatedLine, 10)
	before := process(t, p, request(plugin.Message{Role: "user", Text: "hi"}, toolTurn(long)))
	after := process(t, p, request(
		plugin.Message{Role: "user", Text: "hi"}, toolTurn(long),
		plugin.Message{Role: "assistant", Text: "ok"}, toolTurn("fine\n"+strings.Repeat("x\n", 8)),
	))
	for key, repl := range before.RedactSpans {
		if after.RedactSpans[key] != repl {
			t.Errorf("span %q changed once later turns were added", key)
		}
	}
	again := process(t, p, request(plugin.Message{Role: "user", Text: "hi"}, toolTurn(long)))
	if !reflect.DeepEqual(before, again) {
		t.Errorf("not deterministic: %#v then %#v", before, again)
	}
}

func TestSpanCountIsBounded(t *testing.T) {
	var msgs []plugin.Message
	for i := 0; i < 2*maxSpans; i++ {
		msgs = append(msgs, toolTurn(strings.Repeat(fmt.Sprintf("unique line number %d\n", i), 10)))
	}
	ops := process(t, newPlugin(t, nil), request(msgs...))
	checkSpans(t, ops)
	if len(ops.RedactSpans) != maxSpans {
		t.Errorf("got %d spans, want the cap %d", len(ops.RedactSpans), maxSpans)
	}
}

// The registered (compiled-in) plugin and the same code served over unix://
// and https:// must answer identically.
func TestSameOpsCompiledAndServed(t *testing.T) {
	in := plugin.Instance{}
	def, ok := plugin.Lookup(ID)
	if !ok {
		t.Fatalf("plugin %q is not registered", ID)
	}
	h, err := def.New(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	req := request(toolTurn("start\n" + strings.Repeat(repeatedLine, 10)))
	compiled, err := h.(plugin.Processor).Process(context.Background(), &req)
	if err != nil {
		t.Fatal(err)
	}
	if len(compiled.RedactSpans) == 0 {
		t.Fatal("compiled plugin returned no spans")
	}
	transports := plugintest.Serve(t, Definition, in)
	if len(transports) != 2 {
		t.Fatalf("got %d transports, want unix and https", len(transports))
	}
	for _, tr := range transports {
		got, err := tr.Client.Process(context.Background(), &req)
		if err != nil {
			t.Fatalf("%s: %v", tr.Name, err)
		}
		if !reflect.DeepEqual(got, compiled) {
			t.Errorf("%s Ops = %#v, want the compiled %#v", tr.Name, got, compiled)
		}
	}
}

func TestNewRejectsInvalidConfiguration(t *testing.T) {
	for name, cfg := range map[string]map[string]any{
		"min_repeat":   {"min_repeat": 1},
		"head+tail":    {"max_tool_bytes": 1000, "head_bytes": 800, "tail_bytes": 200},
		"unknown type": {"head_bytes": "big"},
	} {
		if _, err := New(context.Background(), plugin.Instance{Config: cfg}); err == nil {
			t.Errorf("%s: New() accepted %v", name, cfg)
		}
	}
}

func TestDefinitionIsRegistered(t *testing.T) {
	got, ok := plugin.Lookup(ID)
	if !ok {
		t.Fatalf("plugin %q is not registered", ID)
	}
	if got.Ref != Definition.Ref || got.New == nil {
		t.Fatalf("registered definition = %#v, want ref %q with a runtime factory", got, Definition.Ref)
	}
}

