// Command plugin-compress serves the compress plugin over the SDK HTTP transport.
package main

import (
	"log"

	plugincompress "example.com/plugins/compress"
	"github.com/modelhive-ai/plugin-sdk/pkg/plugin"
)

func main() {
	if err := plugin.Serve(plugincompress.Definition); err != nil {
		log.Fatal(err)
	}
}
