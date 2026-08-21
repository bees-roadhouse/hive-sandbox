// Its own module for the same reason the SDK is: a wasip1-only package cannot
// live inside the host module without breaking `go build ./...`.
module github.com/bees-roadhouse/hive-sandbox/apps/hello

go 1.26

require github.com/bees-roadhouse/hive-sandbox/guest v0.0.0

replace github.com/bees-roadhouse/hive-sandbox/guest => ../../guest
