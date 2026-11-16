// Command plugin-codeguard serves the codeguard plugin over the SDK HTTP transport.
package main

import (
	"log"

	"example.com/plugins/codeguard"
	"github.com/modelhive-ai/plugin-sdk/pkg/plugin"
)

func main() {
	if err := plugin.Serve(codeguard.Definition); err != nil {
		log.Fatal(err)
	}
}
