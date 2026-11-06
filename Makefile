BINARY := apple-mail-mcp
PKG := ./cmd/apple-mail-mcp

.PHONY: build release test lint install clean tools-docs

build:
	go build -o $(BINARY) $(PKG)

release:
	mkdir -p dist && CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags='-s -w' -o dist/$(BINARY)-darwin-arm64 $(PKG) && shasum -a 256 dist/$(BINARY)-darwin-arm64 > dist/SHA256SUMS

test:
	go test ./... -race -cover

lint:
	go vet ./...

install:
	go install ./cmd/apple-mail-mcp

clean:
	rm -f $(BINARY)

tools-docs:
	go run ./cmd/apple-mail-mcp tools-docs

