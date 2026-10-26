.PHONY: build test lint web cli-binaries

build:
	go build ./...

test:
	go test ./...

lint:
	go vet ./...

web:
	pnpm --dir web build
	mkdir -p internal/webui/dist
	cp -R web/out/. internal/webui/dist/

cli-binaries:
	mkdir -p dist/cli
	set -e; for platform in darwin-arm64 darwin-amd64 linux-amd64 linux-arm64 windows-amd64 windows-arm64; do \
		GOOS=$${platform%-*} GOARCH=$${platform##*-} CGO_ENABLED=0 \
		  go build -trimpath -ldflags="-s -w" -o "dist/cli/paddock-$$platform$$(test "$${platform%-*}" = windows && echo .exe)" ./cmd/paddock; \
	done
