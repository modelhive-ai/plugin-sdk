// Command plugin-hello serves the hello plugin over the SDK HTTP transport.
package main

import (
	"log"

	pluginhello "example.com/plugins/hello"
	"github.com/modelhive-ai/plugin-sdk/pkg/plugin"
)

func main() {
	if err := plugin.Serve(pluginhello.Definition); err != nil {
		log.Fatal(err)
	}
}
