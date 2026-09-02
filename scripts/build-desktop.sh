#!/usr/bin/env bash
# Check and build the Linux desktop client (desktop/, its own Go module).
#
#   ./scripts/build-desktop.sh             # headless checks, then the windowed
#                                          # binary, built in a container when
#                                          # the host lacks the GUI libraries
#   ./scripts/build-desktop.sh --image     # rebuild the toolchain image first
#   ./scripts/build-desktop.sh --headless  # vet+test only; no windowed binary
#
# Why a dedicated image: the host gate's image deliberately carries no GUI
# libraries, and `go build` of cmd/hive-desktop links WebKit2GTK through cgo.
# The nested module is also invisible to the host gate's ./..., so these
# checks run NOWHERE else ... that is exactly why they must run here.
set -uo pipefail
cd "$(dirname "$0")/.."

export PODMAN_COMPOSE_WARNING_LOGS=false

IMAGE=localhost/hive-sandbox-desktopgate:latest
HEADLESS=0
REBUILD=0
for arg in "$@"; do
  case "$arg" in
    --headless) HEADLESS=1 ;;
    --image) REBUILD=1 ;;
    *) echo "unknown flag: $arg" >&2; exit 1 ;;
  esac
done

failed=()

run_host() {
  echo "==> desktop: go vet (webview-free packages)"
  ( cd desktop && go vet ./internal/... ./ui ) || failed+=("vet")
  # CI lints these packages and this script did not, so fourteen findings
  # reached a pull request with a green local gate. One gate, same steps.
  if command -v golangci-lint >/dev/null 2>&1; then
    echo "==> desktop: golangci-lint (webview-free packages)"
    ( cd desktop && golangci-lint run ./internal/... ./ui ) || failed+=("lint")
  else
    echo "golangci-lint not on the host; the desktop lint step is SKIPPED here and CI will run it" >&2
  fi

  echo "==> desktop: go test -race (webview-free packages)"
  ( cd desktop && go test -race -count=1 -timeout 120s ./internal/... ) || failed+=("test")
}

build_binary() {
  echo "==> desktop: go build -tags gtk3 ./cmd/hive-desktop"
  mkdir -p desktop/bin
  ( cd desktop && CGO_ENABLED=1 go build -tags gtk3 \
      -ldflags "-s -w -X main.version=$(version)" \
      -o bin/hive-desktop ./cmd/hive-desktop ) || { failed+=("build"); return; }
  echo "DESKTOP BINARY: desktop/bin/hive-desktop"
}

version() { git describe --tags --always --dirty 2>/dev/null || echo dev; }

if ! command -v go >/dev/null 2>&1; then
  echo "go not found on the host; running every check in the toolchain image." >&2
  run_in_image_all=1
else
  run_in_image_all=0
  run_host
fi

if [ "$HEADLESS" = 1 ]; then
  [ ${#failed[@]} -eq 0 ] || { printf 'GATE RED: %s\n' "${failed[*]}"; exit 1; }
  echo "GATE GREEN (headless)"
  exit 0
fi

if command -v pkg-config >/dev/null 2>&1 && pkg-config --exists webkit2gtk-4.1 && command -v go >/dev/null 2>&1; then
  build_binary
else
  if [ "$REBUILD" = 1 ] || ! podman image exists "$IMAGE"; then
    echo "==> building $IMAGE (first use, or --image)"
    podman build -t "$IMAGE" -f docker/desktop-gate/Containerfile . || { failed+=("image"); }
  fi
  if [ ${#failed[@]} -eq 0 ]; then
    mkdir -p .gocache/build .gocache/mod .gocache/gopath
    echo "==> desktop: full build in $IMAGE (host lacks webkit2gtk-4.1 or go)"
    podman run --rm \
      --userns=keep-id --security-opt label=disable \
      -v "$PWD:/src" -v "$PWD/.gocache/build:/gocache/build" \
      -v "$PWD/.gocache/mod:/gocache/mod" -v "$PWD/.gocache/gopath:/gocache/gopath" \
      -e GOCACHE=/gocache/build -e GOMODCACHE=/gocache/mod -e GOPATH=/gocache/gopath \
      -w /src/desktop "$IMAGE" bash -c '
        set -e
        go vet ./internal/... ./ui
        golangci-lint run ./internal/... ./ui
        go test -race -count=1 -timeout 120s ./internal/...
        mkdir -p bin
        CGO_ENABLED=1 go build -tags gtk3 -ldflags "-s -w -X main.version=$(git describe --tags --always --dirty 2>/dev/null || echo dev)" -o bin/hive-desktop ./cmd/hive-desktop
        ./bin/hive-desktop -version' || failed+=("container")
    [ -f desktop/bin/hive-desktop ] && echo "DESKTOP BINARY: desktop/bin/hive-desktop"
  fi
fi

[ ${#failed[@]} -eq 0 ] || { printf 'GATE RED: %s\n' "${failed[*]}"; exit 1; }
echo "GATE GREEN"
