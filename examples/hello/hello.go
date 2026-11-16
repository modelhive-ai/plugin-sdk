// Package pluginhello is a minimal request transformation plugin.
//
// Importing this package registers the plugin for in-process use. The command
// in cmd/plugin-hello serves the same implementation out of process.
package pluginhello

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/modelhive-ai/plugin-sdk/pkg/plugin"
)

const (
	// ID is the identifier used for in-process registration.
	ID = "hello"

	defaultMessage = "Hello from the Modelhive plugin SDK."
)

// Config configures the system message appended by the plugin.
type Config struct {
	Message string `json:"message"`
}

// Plugin appends a configured system message to every request.
type Plugin struct {
	message string
}

// New builds a hello plugin for one configured instance.
func New(_ context.Context, in plugin.Instance) (plugin.Handler, error) {
	cfg := Config{Message: defaultMessage}
	if in.Config != nil {
		raw, err := json.Marshal(in.Config)
		if err != nil {
			return nil, fmt.Errorf("encode hello config: %w", err)
		}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, fmt.Errorf("decode hello config: %w", err)
		}
	}
	if cfg.Message == "" {
		return nil, errors.New("hello config: message must not be empty")
	}
	return &Plugin{message: cfg.Message}, nil
}

// Process returns operations rather than modifying the request body. This
// keeps the implementation identical for in-process and HTTP execution.
func (p *Plugin) Process(ctx context.Context, _ *plugin.Request) (plugin.Ops, error) {
	if err := ctx.Err(); err != nil {
		return plugin.Ops{}, err
	}
	return plugin.Ops{
		AppendSystem: []string{p.message},
		Annotations: map[string]string{
			"hello": "applied",
		},
	}, nil
}

// ServeHTTP implements plugin.APIHandler. The gateway mounts this handler at
// /v1/plugins/hello, so paths here are relative to that prefix.
func (p *Plugin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/ui/index.html":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(helloAppHTML))
	case r.Method == http.MethodGet && r.URL.Path == "/ui/app.js":
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		_, _ = w.Write([]byte(helloAppJS))
	case r.Method == http.MethodGet && r.URL.Path == "/hello":
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"message": p.message})
	default:
		http.NotFound(w, r)
	}
}

// Definition is registered in-process and served by the standalone command.
var Definition = plugin.Definition{
	Ref: ID,
	New: New,
}

func init() {
	plugin.Register(Definition)
}

var _ plugin.Processor = (*Plugin)(nil)
var _ plugin.APIHandler = (*Plugin)(nil)

const helloAppHTML = `<!doctype html>
<html lang="en">
<head>
	<meta charset="utf-8">
	<meta name="viewport" content="width=device-width,initial-scale=1">
	<link rel="stylesheet" href="/console/styles.css">
	<link rel="stylesheet" href="/console/ui.css">
	<title>Hello plugin</title>
</head>
<body>
	<main class="page-shell">
		<section class="card">
			<div class="card-head"><div><h1>Hello plugin</h1><p id="hello-message">Loading…</p></div></div>
			<div class="actions"><button id="hello-load" class="primary" type="button">Call plugin API</button></div>
		</section>
	</main>
	<script src="/console/sundelion-plugin.js"></script>
	<script src="app.js"></script>
</body>
</html>`

const helloAppJS = `(() => {
	const message = document.querySelector("#hello-message");
	const load = async () => {
		try {
			const response = await Sundelion.api.get("/hello");
			const data = response.json || {};
			message.textContent = data.message || "No message returned.";
			await Sundelion.ui.toast("Hello plugin API responded", "success");
		} catch (error) {
			message.textContent = error.message || "Plugin API request failed.";
			await Sundelion.ui.toast(message.textContent, "error");
		}
	};
	document.querySelector("#hello-load").addEventListener("click", load);
	Sundelion.ready().then(load);
})();`
