package registry

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/bees-roadhouse/hive-sandbox/internal/wasmhost"
)

// testExports produces the evidence these tests check manifests against.
//
// It goes through the real seam ... encode a module, compile it on a real host,
// read back what the host says it exports ... and there is deliberately no
// shorter path. wasmhost.Exports has an unexported hash precisely so a caller
// cannot pair one module's name list with another module's address, and a
// test-only constructor exported from wasmhost to save these few milliseconds
// would have handed that back to every caller in the process. The lesson is
// blob's: `Sealed` stopped meaning anything the moment a caller could write one
// down.
//
// The cost is real ... this compiles a (tiny) module per call rather than
// building a slice. It is the price of the assertions below being about
// something.
func testExports(t *testing.T, names ...string) wasmhost.Exports {
	t.Helper()

	wasm := moduleExporting(names...)
	hash := wasmhost.HashModule(wasm)

	h, err := wasmhost.New(context.Background(), wasmhost.Config{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, wasmhost.Deps{})
	if err != nil {
		t.Fatalf("wasmhost.New: %v", err)
	}
	t.Cleanup(func() { _ = h.Close(context.Background()) })

	exports, err := h.ModuleExports(context.Background(), wasmhost.Module{
		Hash: hash, App: "fixture", Version: "0.0.1", MemoryPages: 1,
	}, wasmhost.BytesSource(wasm))
	if err != nil {
		t.Fatalf("ModuleExports: %v", err)
	}
	return exports
}

// moduleExporting hand-encodes the smallest valid wasm module that exports a
// memory and the named functions, each a no-op.
//
// Hand-encoded rather than built with TinyGo because the interesting inputs
// here are export NAMES ... "declares add_entry, exports add_entrie" ... and
// checking a rename into the build pipeline is a minute per case. The encoder
// is boring on purpose: one type, N functions of it, one memory, the exports,
// N empty bodies.
func moduleExporting(names ...string) []byte {
	const (
		secType   = 1
		secFunc   = 3
		secMemory = 5
		secExport = 7
		secCode   = 10

		exportFunc   = 0x00
		exportMemory = 0x02
	)

	mod := []byte{0x00, 'a', 's', 'm', 0x01, 0x00, 0x00, 0x00}

	// One type: () -> ().
	mod = append(mod, section(secType, concat(uleb(1), []byte{0x60, 0x00, 0x00}))...)

	// N functions, all of type 0.
	funcs := uleb(uint32(len(names)))
	for range names {
		funcs = append(funcs, 0x00)
	}
	mod = append(mod, section(secFunc, funcs)...)

	// One memory, minimum one page. checkModule refuses a module without one,
	// because the ABI is copies in and out of guest memory.
	mod = append(mod, section(secMemory, concat(uleb(1), []byte{0x00, 0x01}))...)

	exports := uleb(uint32(len(names) + 1))
	exports = append(exports, name("memory")...)
	exports = append(exports, exportMemory, 0x00)
	for i, n := range names {
		exports = append(exports, name(n)...)
		exports = append(exports, exportFunc)
		exports = append(exports, uleb(uint32(i))...)
	}
	mod = append(mod, section(secExport, exports)...)

	// Bodies: no locals, immediate `end`.
	code := uleb(uint32(len(names)))
	for range names {
		body := []byte{0x00, 0x0b}
		code = append(code, uleb(uint32(len(body)))...)
		code = append(code, body...)
	}
	mod = append(mod, section(secCode, code)...)

	return mod
}

func section(id byte, body []byte) []byte {
	return concat([]byte{id}, uleb(uint32(len(body))), body)
}

func name(s string) []byte { return concat(uleb(uint32(len(s))), []byte(s)) }

func uleb(v uint32) []byte {
	var out []byte
	for {
		b := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			b |= 0x80
		}
		out = append(out, b)
		if v == 0 {
			return out
		}
	}
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// The encoder is a fixture, and a fixture that quietly produces something other
// than what it says would make every test above pass for the wrong reason: an
// export list that is missing a name and an encoder that never wrote it look
// identical from the assertion's side.
func TestTheFixtureEncoderProducesWhatItClaims(t *testing.T) {
	exports := testExports(t, reactorInit, "add_entry", "search")

	for _, want := range []string{reactorInit, "add_entry", "search"} {
		if !exports.Has(want) {
			t.Errorf("the encoded module does not export %q; names = %v", want, exports.Names())
		}
	}
	if exports.Has("never_encoded") {
		t.Error("the fixture reports an export it was never given")
	}
	if exports.ModuleHash() == "" {
		t.Error("evidence with no module hash is not evidence")
	}
}

// Exports must be bound to the bytes it was read from, or the registry's whole
// premise ... a claim meeting its evidence ... is a pair of parameters a caller
// chose independently.
func TestExportsCarryTheHashOfTheirOwnModule(t *testing.T) {
	one := testExports(t, reactorInit, "add_entry")
	two := testExports(t, reactorInit, "add_entry", "search")

	if one.ModuleHash() == two.ModuleHash() {
		t.Fatal("two different modules hashed the same; the fixture is not varying the bytes")
	}
	if got := wasmhost.HashModule(moduleExporting(reactorInit, "add_entry")); got != one.ModuleHash() {
		t.Errorf("ModuleHash = %q, want the address of the bytes it was compiled from %q",
			one.ModuleHash(), got)
	}
}
