// Package guest is the SDK a WASM app links against to talk to hive-sandbox.
//
// The whole ABI is here. An app writes one exported function per manifest
// function and wraps its body in Handle:
//
//	//go:wasmexport greet
//	func greet() int32 {
//		return guest.Handle(func(in []byte) ([]byte, error) {
//			return []byte(`{"ok":true}`), nil
//		})
//	}
//
// Build as a WASI preview1 reactor. Never wasip2, never the component model:
// wazero does not support them, and the host rejects those imports at link
// time rather than failing mysteriously later.
//
//	tinygo build -target=wasip1 -buildmode=c-shared -o app.wasm ./
package guest

import (
	"errors"
	"runtime"
	"unsafe"
)

// ---------------------------------------------------------------------------
// hive_abi: the call protocol.
//
// Nothing crosses the wasm signature. Every transfer is "ask for the size,
// allocate it yourself, ask the host to copy into it", so the host never calls
// back into a guest allocator and an app may use whatever allocator its
// toolchain ships.
// ---------------------------------------------------------------------------

//go:wasmimport hive_abi abi_version
func abiVersion() int32

//go:wasmimport hive_abi input_size
func inputSize() int32

//go:wasmimport hive_abi input_read
func inputRead(ptr unsafe.Pointer)

//go:wasmimport hive_abi output_write
func outputWrite(ptr unsafe.Pointer, size int32) int32

//go:wasmimport hive_abi error_write
func errorWrite(ptr unsafe.Pointer, size int32) int32

//go:wasmimport hive_abi result_size
func resultSize() int32

//go:wasmimport hive_abi result_read
func resultRead(ptr unsafe.Pointer)

// ABIVersion is the host's ABI revision. Check it in an init if an app cares.
func ABIVersion() int32 { return abiVersion() }

// Input returns the JSON the host passed to this call.
func Input() []byte {
	n := inputSize()
	if n <= 0 {
		return nil
	}
	buf := make([]byte, n)
	inputRead(unsafe.Pointer(&buf[0]))
	runtime.KeepAlive(buf)
	return buf
}

// Output sets this call's JSON result. The last call wins.
func Output(b []byte) {
	if len(b) == 0 {
		return
	}
	outputWrite(unsafe.Pointer(&b[0]), int32(len(b)))
	runtime.KeepAlive(b)
}

// Fail records an error message for this call. Handle does this for you.
func Fail(msg string) {
	b := []byte(msg)
	if len(b) == 0 {
		return
	}
	errorWrite(unsafe.Pointer(&b[0]), int32(len(b)))
	runtime.KeepAlive(b)
}

// Handle is the body of every exported guest function: read the input, run the
// app's code, hand back either output or an error, return the status the host
// expects.
func Handle(fn func(input []byte) ([]byte, error)) int32 {
	out, err := fn(Input())
	if err != nil {
		Fail(err.Error())
		return 1
	}
	Output(out)
	return 0
}

// ---------------------------------------------------------------------------
// hive_log
// ---------------------------------------------------------------------------

// Log levels match log/slog, so a guest's lines sort with the daemon's.
const (
	LevelDebug int32 = -4
	LevelInfo  int32 = 0
	LevelWarn  int32 = 4
	LevelError int32 = 8
)

//go:wasmimport hive_log log
func hostLog(level int32, ptr unsafe.Pointer, size int32)

// Log writes one line into the daemon log, attributed to this app by the host.
// An app cannot log as another app because it never gets to name itself.
func Log(level int32, msg string) {
	b := []byte(msg)
	if len(b) == 0 {
		return
	}
	hostLog(level, unsafe.Pointer(&b[0]), int32(len(b)))
	runtime.KeepAlive(b)
}

// ---------------------------------------------------------------------------
// Capability domains. Every verb has the same shape: JSON in, Status plus JSON
// out. Importing one of these without the matching manifest capability makes
// the app fail to load, so an app links only what it declared.
// ---------------------------------------------------------------------------

// Status is what a capability call returns.
type Status int32

const (
	StatusOK            Status = 0
	StatusError         Status = 1
	StatusDenied        Status = 2
	StatusNotFound      Status = 3
	StatusInvalid       Status = 4
	StatusUnimplemented Status = 5
	StatusCanceled      Status = 6
)

func (s Status) String() string {
	switch s {
	case StatusOK:
		return "ok"
	case StatusError:
		return "error"
	case StatusDenied:
		return "denied"
	case StatusNotFound:
		return "not_found"
	case StatusInvalid:
		return "invalid"
	case StatusUnimplemented:
		return "unimplemented"
	case StatusCanceled:
		return "canceled"
	default:
		return "status"
	}
}

// HostError is a failed capability call. Status carries the reason; Message is
// the host's text.
type HostError struct {
	Op      string
	Status  Status
	Message string
}

func (e *HostError) Error() string { return e.Op + ": " + e.Status.String() + ": " + e.Message }

// Denied reports whether a call failed because the caller may not do it.
// Absence of a grant lands here and is deliberately indistinguishable from an
// explicit deny.
func Denied(err error) bool {
	var he *HostError
	return errors.As(err, &he) && he.Status == StatusDenied
}

//go:wasmimport hive_storage insert
func storageInsert(ptr unsafe.Pointer, size int32) int32

//go:wasmimport hive_storage get
func storageGet(ptr unsafe.Pointer, size int32) int32

//go:wasmimport hive_storage update
func storageUpdate(ptr unsafe.Pointer, size int32) int32

//go:wasmimport hive_storage delete
func storageDelete(ptr unsafe.Pointer, size int32) int32

//go:wasmimport hive_storage query
func storageQuery(ptr unsafe.Pointer, size int32) int32

//go:wasmimport hive_kv get
func kvGet(ptr unsafe.Pointer, size int32) int32

//go:wasmimport hive_kv set
func kvSet(ptr unsafe.Pointer, size int32) int32

//go:wasmimport hive_kv delete
func kvDelete(ptr unsafe.Pointer, size int32) int32

//go:wasmimport hive_blob read
func blobRead(ptr unsafe.Pointer, size int32) int32

//go:wasmimport hive_blob append
func blobAppend(ptr unsafe.Pointer, size int32) int32

//go:wasmimport hive_events emit
func eventsEmit(ptr unsafe.Pointer, size int32) int32

// Each verb is spelled out rather than routed through one helper taking a
// function value: a //go:wasmimport function cannot be used as a value. That
// turns out to be the better shape anyway, because the linker now drops the
// import for every verb an app does not call, so a guest links exactly the
// capabilities it uses and the host's link-time check sees the truth.

// Storage verbs. One call is one transaction. The host resolves who is asking
// from the credential, so a request body carries data and never identity.

func StorageInsert(req []byte) ([]byte, error) {
	req = orEmpty(req)
	s := Status(storageInsert(unsafe.Pointer(&req[0]), int32(len(req))))
	runtime.KeepAlive(req)
	return finish("storage.insert", s)
}

func StorageGet(req []byte) ([]byte, error) {
	req = orEmpty(req)
	s := Status(storageGet(unsafe.Pointer(&req[0]), int32(len(req))))
	runtime.KeepAlive(req)
	return finish("storage.get", s)
}

func StorageUpdate(req []byte) ([]byte, error) {
	req = orEmpty(req)
	s := Status(storageUpdate(unsafe.Pointer(&req[0]), int32(len(req))))
	runtime.KeepAlive(req)
	return finish("storage.update", s)
}

func StorageDelete(req []byte) ([]byte, error) {
	req = orEmpty(req)
	s := Status(storageDelete(unsafe.Pointer(&req[0]), int32(len(req))))
	runtime.KeepAlive(req)
	return finish("storage.delete", s)
}

func StorageQuery(req []byte) ([]byte, error) {
	req = orEmpty(req)
	s := Status(storageQuery(unsafe.Pointer(&req[0]), int32(len(req))))
	runtime.KeepAlive(req)
	return finish("storage.query", s)
}

// KV is a per-install best-effort cache: TTL'd, flushable, never truth.

func KVGet(req []byte) ([]byte, error) {
	req = orEmpty(req)
	s := Status(kvGet(unsafe.Pointer(&req[0]), int32(len(req))))
	runtime.KeepAlive(req)
	return finish("kv.get", s)
}

func KVSet(req []byte) ([]byte, error) {
	req = orEmpty(req)
	s := Status(kvSet(unsafe.Pointer(&req[0]), int32(len(req))))
	runtime.KeepAlive(req)
	return finish("kv.set", s)
}

func KVDelete(req []byte) ([]byte, error) {
	req = orEmpty(req)
	s := Status(kvDelete(unsafe.Pointer(&req[0]), int32(len(req))))
	runtime.KeepAlive(req)
	return finish("kv.delete", s)
}

// Blob is windowed access to content-addressed bytes.

func BlobRead(req []byte) ([]byte, error) {
	req = orEmpty(req)
	s := Status(blobRead(unsafe.Pointer(&req[0]), int32(len(req))))
	runtime.KeepAlive(req)
	return finish("blob.read", s)
}

func BlobAppend(req []byte) ([]byte, error) {
	req = orEmpty(req)
	s := Status(blobAppend(unsafe.Pointer(&req[0]), int32(len(req))))
	runtime.KeepAlive(req)
	return finish("blob.append", s)
}

// EventsEmit appends to the platform event log.
func EventsEmit(req []byte) ([]byte, error) {
	req = orEmpty(req)
	s := Status(eventsEmit(unsafe.Pointer(&req[0]), int32(len(req))))
	runtime.KeepAlive(req)
	return finish("events.emit", s)
}

func orEmpty(req []byte) []byte {
	if len(req) == 0 {
		return []byte("{}")
	}
	return req
}

// finish reads the result slot straight away, because the next host call
// overwrites it.
func finish(op string, status Status) ([]byte, error) {
	body := readResult()
	if status != StatusOK {
		return nil, &HostError{Op: op, Status: status, Message: string(body)}
	}
	return body, nil
}

func readResult() []byte {
	n := resultSize()
	if n <= 0 {
		return nil
	}
	buf := make([]byte, n)
	resultRead(unsafe.Pointer(&buf[0]))
	runtime.KeepAlive(buf)
	return buf
}
