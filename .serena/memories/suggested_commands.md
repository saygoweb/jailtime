# Suggested Commands

## Build
```sh
go build ./cmd/jailtimed
go build ./cmd/jailtime
```

## Test
```sh
# Run all tests
go test ./...

# Run tests in a specific package with verbose output
go test -v ./internal/control/...

# Run a single test
go test ./internal/engine/... -run TestJailRuntimeConfigTest
go test ./internal/watch/...  -run TestPollBackendSubdirGlob
```

No Makefile — all build/test is via `go` toolchain directly.

## No separate lint/format step configured; use standard Go tools:
```sh
go vet ./...
gofmt -l .
```
