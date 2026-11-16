# Modelhive plugin SDK

Go SDK for writing plugins for the [Modelhive](https://modelhive.ai) AI gateway: request and
response processors, guardrails, compressors, connectors, console pages and workflow nodes.

```sh
go get github.com/modelhive-ai/plugin-sdk@latest
```

```go
import "github.com/modelhive-ai/plugin-sdk/pkg/plugin"
```

Documentation: **[docs.modelhive.ai/plugin-sdk](https://docs.modelhive.ai/plugin-sdk/)** (Italian).

## Contents

| Path | What it is |
|---|---|
| `pkg/plugin` | The plugin API: `Definition`, `Request`, `Ops`, `Serve`, the `Host` services. |
| `pkg/plugin/plugintest` | Test helpers: `Run`, `TestProcessor`, `Serve`, `ValidateOps`. |
| `examples/hello` | Minimal plugin: rewrites the request, exposes an API route and a console page. |
| `examples/compress` | Compresses tool output with `RedactSpans`; deterministic and stateless. |
| `examples/codeguard` | Masks or blocks customer and loyalty-card codes. |
| `examples/python-codeguard` | The same plugin in Python, without the SDK, over the HTTP protocol. |
| `docs/plugin-http-openapi.yaml` | The HTTP protocol between gateway and plugin, for plugins in other languages. |

No external dependencies. Requires the Go version in `go.mod`.

## First plugin

```sh
cd examples/hello
make test       # go vet and go test
make package    # Linux amd64 binary and manifest in dist/io.example.hello
```

Copy `dist/io.example.hello` into the gateway's plugins folder (console: **System → Local
plugins**), press **Reload plugins** on the Plugins page, create an instance, approve its
permissions and enable it.

To start your own, copy `examples/hello` out of this repository, change `module` in `go.mod`,
`metadata.id` in the manifest and `ID` in the Makefile, and drop the `replace` line from
`go.mod`.

## License

[Apache License 2.0](LICENSE).
