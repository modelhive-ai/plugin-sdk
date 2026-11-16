// Package plugin is the Modelhive plugin SDK: the one contract every plugin
// is written against, whether Modelhive ships it, a partner writes it or a
// customer does.
//
// A plugin is a manifest (its permissions, config schema and capabilities)
// plus a Definition. The Definition may provide:
//
//   - a Connector: the lifecycle of an integration instance (validate, test,
//     reconcile the Modelhive objects it owns, rotate, deprovision). Identity
//     providers, log sinks and managed apps are connectors.
//   - a New func building the per-instance Handler the gateway calls at
//     runtime. A Handler implements one or more of Processor (shape or stop a
//     request), OutputGuard (check streamed output), Observer (see finished
//     exchanges) and IdentityProvider (map a federated login).
//
// The same Definition runs two ways, with no change to its code:
//
//	func init() { plugin.Register(myplugin.Definition) } // compiled into the gateway
//	func main() { plugin.Serve(myplugin.Definition) }   // its own process, over HTTP
//
// Compiled in, calls are direct. Served, the gateway reaches the plugin over
// unix:// or https:// and Resources become authenticated callbacks, limited
// to what the manifest's permissions grant. A plugin cannot tell the
// difference, which is what lets a partner plugin be promoted into the
// binary later.
//
// This package is public API: changes follow semantic versioning.
package plugin

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// ErrNotImplemented is returned by an operation a plugin does not provide.
var ErrNotImplemented = errors.New("plugin: not implemented")

// Definition is everything the gateway needs from a plugin's code.
type Definition struct {
	// Ref matches the manifest's spec.connector.ref.
	Ref string
	// Connector runs the instance lifecycle. Nil means the plugin owns no
	// Modelhive objects, and lifecycle operations are no-ops.
	Connector Connector
	// New builds the runtime handler of one instance. It is called when an
	// instance is enabled and again when its config or secrets change; the
	// previous handler is then closed if it implements io.Closer. Nil means
	// the plugin has no runtime part.
	New func(ctx context.Context, in Instance) (Handler, error)
}

// Handler is the runtime object of one instance. The gateway type-asserts it
// against Processor, OutputGuard, Observer and IdentityProvider, and calls
// only the interfaces the manifest declares capabilities for.
type Handler any

var (
	mu   sync.RWMutex
	defs = map[string]Definition{}
)

// Register makes a compiled-in plugin known to the gateway. It is meant for
// init functions and panics on an empty or duplicate Ref, which is a build
// mistake rather than a runtime condition.
func Register(d Definition) {
	if d.Ref == "" {
		panic("plugin: Register with empty Ref")
	}
	mu.Lock()
	defer mu.Unlock()
	if _, dup := defs[d.Ref]; dup {
		panic(fmt.Sprintf("plugin: Register called twice for %q", d.Ref))
	}
	defs[d.Ref] = d
}

// Lookup returns the compiled-in plugin registered under ref.
func Lookup(ref string) (Definition, bool) {
	mu.RLock()
	defer mu.RUnlock()
	d, ok := defs[ref]
	return d, ok
}

// Registered returns the refs of every compiled-in plugin, sorted.
func Registered() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(defs))
	for ref := range defs {
		out = append(out, ref)
	}
	sort.Strings(out)
	return out
}

// Instance is a plugin's read-only view of one configured instance.
type Instance struct {
	ID            string
	PluginID      string
	PluginVersion string
	Name          string
	// Config holds non-secret values, already validated against the schema.
	Config map[string]any
	// Secrets returns one clear secret on demand; it is never a bulk map so a
	// plugin cannot leak secrets it does not use.
	Secrets  func(name string) (string, error)
	Revision int64
	// Resources reaches the Modelhive objects the manifest permits. It is nil
	// for pipeline plugins running in-process, which own no resources; Serve
	// fills it with a gateway callback client when SUNDELION_PLUGIN_GATEWAY_URL
	// is set. Never serialized.
	Resources Resources `json:"-"`
	// Host holds the gateway services the manifest asks for (storage, vault,
	// models, guard, groups, plugins); see host.go. Filled in-process by the
	// gateway and out of process by Serve. Never serialized.
	Host Host `json:"-"`
}
