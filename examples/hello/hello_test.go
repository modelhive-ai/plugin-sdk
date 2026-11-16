package pluginhello

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/modelhive-ai/plugin-sdk/pkg/plugin"
	"github.com/modelhive-ai/plugin-sdk/pkg/plugin/plugintest"
)

func TestConformance(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	plugintest.Run(t, Definition, plugin.Instance{
		Config: map[string]any{"message": "Be welcoming."},
	},
		plugintest.ProcessCase{
			Name:    "appends configured message",
			Request: plugin.Request{},
			Want: plugin.Ops{
				AppendSystem: []string{"Be welcoming."},
				Annotations:  map[string]string{"hello": "applied"},
			},
		},
		plugintest.ProcessCase{
			Name:      "honors cancellation",
			Context:   canceled,
			WantError: context.Canceled.Error(),
		},
	)
}

// The registered (compiled-in) plugin and the same code served over unix://
// and https:// must answer identically.
func TestSameOpsCompiledAndServed(t *testing.T) {
	in := plugin.Instance{Config: map[string]any{"message": "Be welcoming."}}
	def, ok := plugin.Lookup(ID)
	if !ok {
		t.Fatalf("plugin %q is not registered", ID)
	}
	h, err := def.New(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	req := &plugin.Request{
		ID: "req-1", Format: "anthropic", Model: "claude-test",
		Messages: []plugin.Message{{Role: "user", Text: "hi"}},
		Caller:   plugin.Caller{KeyID: "k1", Groups: []string{"eng"}},
	}
	compiled, err := h.(plugin.Processor).Process(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	want := plugin.Ops{AppendSystem: []string{"Be welcoming."}, Annotations: map[string]string{"hello": "applied"}}
	if !reflect.DeepEqual(compiled, want) {
		t.Fatalf("compiled Ops = %#v, want %#v", compiled, want)
	}

	transports := plugintest.Serve(t, Definition, in)
	if len(transports) != 2 {
		t.Fatalf("got %d transports, want unix and https", len(transports))
	}
	for _, tr := range transports {
		got, err := tr.Client.Process(context.Background(), req)
		if err != nil {
			t.Fatalf("%s: %v", tr.Name, err)
		}
		if !reflect.DeepEqual(got, compiled) {
			t.Errorf("%s Ops = %#v, want the compiled %#v", tr.Name, got, compiled)
		}
	}
}

func TestNewRejectsInvalidConfiguration(t *testing.T) {
	if _, err := New(context.Background(), plugin.Instance{Config: map[string]any{"message": ""}}); err == nil {
		t.Fatal("New() accepted an empty message")
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

func TestPluginAPIHandler(t *testing.T) {
	handler, err := New(context.Background(), plugin.Instance{Config: map[string]any{"message": "Hello console."}})
	if err != nil {
		t.Fatal(err)
	}
	api := handler.(plugin.APIHandler)
	for _, test := range []struct {
		path string
		want int
	}{
		{path: "/ui/index.html", want: http.StatusOK},
		{path: "/ui/app.js", want: http.StatusOK},
		{path: "/hello", want: http.StatusOK},
		{path: "/missing", want: http.StatusNotFound},
	} {
		recorder := httptest.NewRecorder()
		api.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, test.path, nil))
		if recorder.Code != test.want {
			t.Errorf("GET %s = %d, want %d", test.path, recorder.Code, test.want)
		}
	}
}
