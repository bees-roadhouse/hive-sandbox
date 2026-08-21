// Command hello is the reference guest: the smallest app that exercises every
// part of the ABI, and the fixture the host's conformance canary runs under
// both the optimizing compiler and the interpreter.
//
// It is deliberately not a demo. Each export is a host behaviour that needs a
// guest on the other end of it to be tested honestly: a normal call, a guest
// error, a runaway loop, a blocking host call, a memory ceiling, and a compute
// body for the benchmarks.
//
//	tinygo build -target=wasip1 -buildmode=c-shared -o hello.wasm ./
package main

import (
	"encoding/json"
	"errors"
	"strconv"

	"github.com/bees-roadhouse/hive-sandbox/guest"
)

// main never runs. -buildmode=c-shared produces a reactor, so the host calls
// _initialize and then individual exports. A guest that does its work in main
// is a command, and a command is the wrong shape for an app.
func main() {}

type greetIn struct {
	Name string `json:"name"`
}

type greetOut struct {
	Message string `json:"message"`
	ABI     int32  `json:"abi"`
}

//go:wasmexport hello
func hello() int32 {
	return guest.Handle(func(in []byte) ([]byte, error) {
		var req greetIn
		if len(in) > 0 {
			if err := json.Unmarshal(in, &req); err != nil {
				return nil, errors.New("hello: input is not JSON: " + err.Error())
			}
		}
		if req.Name == "" {
			req.Name = "world"
		}
		return json.Marshal(greetOut{Message: "hello, " + req.Name, ABI: guest.ABIVersion()})
	})
}

//go:wasmexport fail
func fail() int32 {
	return guest.Handle(func([]byte) ([]byte, error) {
		return nil, errors.New("this guest fails on purpose")
	})
}

//go:wasmexport logline
func logline() int32 {
	return guest.Handle(func(in []byte) ([]byte, error) {
		guest.Log(guest.LevelInfo, "hello guest says: "+string(in))
		return []byte(`{"logged":true}`), nil
	})
}

// storeQuery proves the capability path end to end and, with a host-side data
// layer that blocks, proves invariant 7: a guest parked inside a host call has
// to come back when the call's context ends, because wazero's termination
// checks live in guest code and cannot reach it here.
//
//go:wasmexport store_query
func storeQuery() int32 {
	return guest.Handle(func(in []byte) ([]byte, error) {
		if len(in) == 0 {
			in = []byte(`{"collection":"entries"}`)
		}
		res, err := guest.StorageQuery(in)
		if err != nil {
			return nil, err
		}
		return res.Data, nil
	})
}

// launder is the attack, written as an export so the host's defence is tested
// against a guest genuinely trying rather than against a mock.
//
// It reads (possibly untrusted) data, writes it straight back claiming the
// content is fine, and returns it as its own output. Under D22 every one of
// those steps is recorded untrusted anyway, because the host never asks the
// guest what the provenance is. TestGuestCannotLaunderTrust is the assertion.
//
//go:wasmexport launder
func launder() int32 {
	return guest.Handle(func([]byte) ([]byte, error) {
		read, err := guest.StorageQuery([]byte(`{"collection":"entries"}`))
		if err != nil {
			return nil, err
		}
		// A guest saying "trusted" in a request body. The host does not read it.
		if _, err := guest.StorageInsert([]byte(`{"collection":"entries","trust":"trusted"}`)); err != nil {
			return nil, err
		}
		return read.Data, nil
	})
}

// reportTrust lets a test see what the guest was told, as opposed to what the
// host recorded. The two must agree.
//
//go:wasmexport report_trust
func reportTrust() int32 {
	return guest.Handle(func([]byte) ([]byte, error) {
		res, err := guest.StorageQuery([]byte(`{"collection":"entries"}`))
		if err != nil {
			return nil, err
		}
		return []byte(`{"input":"` + guest.InputTrust().String() +
			`","response":"` + res.Trust.String() + `"}`), nil
	})
}

// spin is the runaway guest. sink keeps the loop from being optimized away.
var sink uint64

//go:wasmexport spin
func spin() int32 {
	for i := uint64(0); ; i++ {
		sink += i
	}
}

// grow walks past the memory ceiling. memory.grow returning -1 is an allocation
// failure inside the guest, not a host crash, so this surfaces as a guest-side
// out-of-memory rather than as anything the daemon has to survive.
//
//go:wasmexport grow
func grow() int32 {
	return guest.Handle(func([]byte) ([]byte, error) {
		var held [][]byte
		for i := 0; i < 1<<20; i++ {
			chunk := make([]byte, 1<<20)
			chunk[0] = byte(i)
			held = append(held, chunk)
		}
		return []byte(`{"held":` + strconv.Itoa(len(held)) + `}`), nil
	})
}

// sum is the benchmark body: a known amount of pure compute, so the cost of
// wazero's termination checks shows up as the difference between two runs of
// the same loop rather than as noise.
//
// It is an xorshift rather than an accumulate on purpose. `total += i` has a
// closed form and LLVM recognises it, so the obvious version compiles to a
// multiply and measures nothing. Each step here depends on the last, which
// leaves the loop in the binary.
//
//go:wasmexport sum
func sum() int32 {
	return guest.Handle(func(in []byte) ([]byte, error) {
		var req struct {
			N uint64 `json:"n"`
		}
		if len(in) > 0 {
			if err := json.Unmarshal(in, &req); err != nil {
				return nil, err
			}
		}
		state := uint64(0x9e3779b97f4a7c15)
		for i := uint64(0); i < req.N; i++ {
			state ^= state << 13
			state ^= state >> 7
			state ^= state << 17
		}
		return []byte(`{"total":` + strconv.FormatUint(state, 10) + `}`), nil
	})
}

// noop is instantiation and call overhead with nothing in the middle: the
// floor the pool config is measured against.
//
//go:wasmexport noop
func noop() int32 {
	return 0
}
