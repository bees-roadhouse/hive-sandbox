// The guest SDK is its own Go module on purpose.
//
// It only compiles for GOOS=wasip1 (//go:wasmimport is rejected everywhere
// else), so keeping it inside the host module would break `go build ./...` on
// the host platform. A nested module is invisible to the parent's ./... and
// costs nothing else.
module github.com/bees-roadhouse/hive-sandbox/guest

go 1.26
