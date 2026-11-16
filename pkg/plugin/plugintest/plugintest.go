// Package plugintest provides reusable conformance helpers for plugin authors.
package plugintest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/modelhive-ai/plugin-sdk/pkg/plugin"
)

// ProcessCase describes one call to Processor.Process.
type ProcessCase struct {
	Name string

	// Context defaults to context.Background when nil.
	Context context.Context
	Request plugin.Request

	Want      plugin.Ops
	WantError string
}

// TestDefinition constructs a Definition's runtime handler and runs the
// common processor checks. It is intended to be called from a partner's
// TestConformance function.
func TestDefinition(
	t *testing.T,
	d plugin.Definition,
	in plugin.Instance,
	cases ...ProcessCase,
) {
	t.Helper()
	if d.Ref == "" {
		t.Fatal("plugintest: definition has an empty Ref")
	}
	if d.New == nil {
		t.Fatal("plugintest: definition has no runtime factory")
	}
	h, err := d.New(context.Background(), in)
	if err != nil {
		t.Fatalf("plugintest: factory returned an error: %v", err)
	}
	p, ok := h.(plugin.Processor)
	if !ok {
		t.Fatalf("plugintest: handler %T does not implement plugin.Processor", h)
	}
	TestProcessor(t, p, cases...)
}

// TestProcessor runs processing cases and checks transport compatibility and
// generic Ops invariants. Plugin-specific semantics remain explicit in Want.
func TestProcessor(t *testing.T, p plugin.Processor, cases ...ProcessCase) {
	t.Helper()
	if isNilProcessor(p) {
		t.Fatal("plugintest: nil plugin")
	}
	if len(cases) == 0 {
		t.Fatal("plugintest: at least one transform case is required")
	}

	for i := range cases {
		tc := cases[i]
		name := tc.Name
		if name == "" {
			name = fmt.Sprintf("case_%d", i+1)
		}
		t.Run(name, func(t *testing.T) {
			ctx := tc.Context
			if ctx == nil {
				ctx = context.Background()
			}

			req := cloneRequest(tc.Request)
			before := cloneRequest(req)
			got, err := process(p, ctx, &req)
			if !reflect.DeepEqual(req, before) {
				t.Errorf("Process() mutated its request: got %#v, want %#v; return edits in plugin.Ops", req, before)
			}
			if tc.WantError != "" {
				if err == nil {
					t.Fatalf("Process() error = nil, want an error containing %q", tc.WantError)
				}
				if !strings.Contains(err.Error(), tc.WantError) {
					t.Fatalf("Process() error = %q, want an error containing %q", err, tc.WantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("Process() unexpected error: %v", err)
			}
			if err := ValidateOps(got); err != nil {
				t.Fatalf("Process() returned invalid operations: %v", err)
			}
			if _, err := json.Marshal(got); err != nil {
				t.Fatalf("Process() operations are not JSON-compatible: %v", err)
			}
			if !reflect.DeepEqual(got, tc.Want) {
				t.Errorf("Process() operations = %#v, want %#v", got, tc.Want)
			}
		})
	}
}

// ValidateOps checks invariants that apply to every pipeline plugin.
func ValidateOps(ops plugin.Ops) error {
	if ops.Block != nil && ops.Respond != nil {
		return errors.New("block and respond are mutually exclusive")
	}
	for i, f := range ops.Findings {
		if f.Type == "" {
			return fmt.Errorf("finding %d has no type", i)
		}
	}
	for index := range ops.ReplaceText {
		if index < 0 {
			return fmt.Errorf("replacement message index must be non-negative: %d", index)
		}
	}
	return nil
}

func process(p plugin.Processor, ctx context.Context, req *plugin.Request) (ops plugin.Ops, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("plugin panicked: %v", recovered)
		}
	}()
	return p.Process(ctx, req)
}

func cloneRequest(in plugin.Request) plugin.Request {
	out := in
	out.Messages = append([]plugin.Message(nil), in.Messages...)
	out.Caller.Groups = append([]string(nil), in.Caller.Groups...)
	return out
}

func isNilProcessor(p plugin.Processor) bool {
	if p == nil {
		return true
	}
	v := reflect.ValueOf(p)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}
