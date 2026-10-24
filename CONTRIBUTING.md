# Contributing

## Prerequisites

- Go (version in `go.mod`)
- Docker
- Node 22 and pnpm

## Checks

Run these before opening a pull request:

```sh
go build ./... && go vet ./... && go test ./...
pnpm --dir web build
```

## Pull requests

Keep PRs small and focused on one change. Describe what changed and why, and
note how you verified it.
