# Builds the plugin folder to copy into the gateway's plugins folder.
ID := io.example.compress
BIN := plugin-compress
OUT := dist/$(ID)

.PHONY: test build build-arm64 build-windows package clean

test:
	go vet ./... && go test ./...

build:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o $(OUT)/bin/$(BIN) ./cmd/$(BIN)

build-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o $(OUT)/bin/$(BIN) ./cmd/$(BIN)

build-windows:
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o $(OUT)/bin/$(BIN).exe ./cmd/$(BIN)

package: test build
	cp manifest.yaml $(OUT)/
	tar -C dist -czf $(ID).tar.gz $(ID)
	@echo "Copy $(OUT) into the gateway's plugins folder, then Reload plugins."

clean:
	rm -rf dist *.tar.gz
