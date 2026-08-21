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
	"encoding/json"
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
func inputRead(ptr unsafe.Pointer, size int32) int32

//go:wasmimport hive_abi input_trust
func inputTrust() int32

//go:wasmimport hive_abi output_write
func outputWrite(ptr unsafe.Pointer, size int32) int32

//go:wasmimport hive_abi error_write
func errorWrite(ptr unsafe.Pointer, size int32) int32

//go:wasmimport hive_abi result_read
func resultRead(ptr unsafe.Pointer, size int32) int32

// ABIVersion is the host's ABI revision. Check it in an init if an app cares.
func ABIVersion() int32 { return abiVersion() }

// Input returns the JSON the host passed to this call.
func Input() []byte {
	n := inputSize()
	if n <= 0 {
		return nil
	}
	buf := make([]byte, n)
	got := inputRead(unsafe.Pointer(&buf[0]), n)
	runtime.KeepAlive(buf)
	if got < 0 || got > n {
		return nil
	}
	return buf[:got]
}

// InputTrust reports whether this invocation's input is trusted.
//
// It is for a guest that wants to refuse: an app about to put text into
// instruction position should look first. It is NOT how trust is enforced. The
// host tracks taint whatever the guest does, so ignoring this cannot launder
// anything ... it only means the app made a decision blind.
func InputTrust() Trust {
	if inputTrust() == int32(Untrusted) {
		return Untrusted
	}
	return Trusted
}

// Output sets this call's JSON result and reports whether the host took it.
// The last successful call wins.
//
// Check the status. A result over the host's size limit is refused, and a guest
// that returns success anyway turns an oversized response into a silent empty
// one. Handle does this for you; if you are writing an export by hand, do not
// drop it. (The host also refuses to report success behind your back, but the
// error a guest raises itself is a far better one than the host's.)
func Output(b []byte) Status {
	if len(b) == 0 {
		return StatusOK
	}
	s := Status(outputWrite(unsafe.Pointer(&b[0]), int32(len(b))))
	runtime.KeepAlive(b)
	return s
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
//
// These twelve lines are the ones every AI-written guest will copy, so they
// check everything they can. In particular Output's status is not discarded:
// dropping it turned a result over the host's size limit into a successful
// empty response, which is the worst shape a failure can take.
func Handle(fn func(input []byte) ([]byte, error)) int32 {
	out, err := fn(Input())
	if err != nil {
		Fail(err.Error())
		return 1
	}
	if s := Output(out); s != StatusOK {
		Fail("host refused the result: " + s.String())
		return 1
	}
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

// Trust is where content came from.
//
// A guest cannot set it, cannot clear it, and cannot avoid receiving it: every
// capability response carries one, and the host tracks the invocation's taint
// independently of anything the guest does with it. Reading untrusted data
// means everything this invocation writes is recorded untrusted, whether or not
// the guest ever looks at this value.
type Trust int32

const (
	Trusted   Trust = 0
	Untrusted Trust = 1
)

func (t Trust) String() string {
	if t == Untrusted {
		return "untrusted"
	}
	return "trusted"
}

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

// Response is what a capability call returns.
//
// Trust and Data arrive together and there is no way to obtain one without the
// other, which is the point (D22.1). A guest that wants the bytes has the
// provenance in the same value.
type Response struct {
	Trust Trust
	Data  []byte
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
func storageInsert(ptr unsafe.Pointer, size int32) uint64

//go:wasmimport hive_storage get
func storageGet(ptr unsafe.Pointer, size int32) uint64

//go:wasmimport hive_storage update
func storageUpdate(ptr unsafe.Pointer, size int32) uint64

//go:wasmimport hive_storage delete
func storageDelete(ptr unsafe.Pointer, size int32) uint64

//go:wasmimport hive_storage query
func storageQuery(ptr unsafe.Pointer, size int32) uint64

//go:wasmimport hive_kv get
func kvGet(ptr unsafe.Pointer, size int32) uint64

//go:wasmimport hive_kv set
func kvSet(ptr unsafe.Pointer, size int32) uint64

//go:wasmimport hive_kv delete
func kvDelete(ptr unsafe.Pointer, size int32) uint64

//go:wasmimport hive_blob read
func blobRead(ptr unsafe.Pointer, size int32) uint64

//go:wasmimport hive_blob append
func blobAppend(ptr unsafe.Pointer, size int32) uint64

//go:wasmimport hive_events emit
func eventsEmit(ptr unsafe.Pointer, size int32) uint64

// Each verb is spelled out rather than routed through one helper taking a
// function value: a //go:wasmimport function cannot be used as a value. That
// turns out to be the better shape anyway, because the linker now drops the
// import for every verb an app does not call, so a guest links exactly the
// capabilities it uses and the host's link-time check sees the truth.

// Storage verbs. One call is one transaction. The host resolves who is asking
// from the credential, so a request body carries data and never identity.
//
// # "blob" is reserved in a document body
//
// The host maintains blob references for you: it walks each document it is
// handed and writes, moves or releases them on the same transaction as the
// write. It recognises a descriptor by the key alone, at any depth and inside
// arrays:
//
//	{"cover": {"blob": "<64 hex>", "size": 20481, "mime": "image/jpeg"}}
//
// A "blob" key whose value is not a 64-character hex digest FAILS THE WRITE,
// naming the JSON path. It is not ignored, because ignoring it would let one
// mistyped character in a digest quietly turn a descriptor into an ordinary
// string field ... and the bytes it named would then be collected out from
// under a live document.
//
// Use any other key for a checksum or a content address you are only recording.
// "sha256", "digest" and "checksum" are ordinary fields; only "blob" is claimed.
//
// A document may only name blobs its own principal already holds a reference
// to. Naming somebody else's returns the same not-found as naming a digest that
// was never stored. See docs/blob.md.

func StorageInsert(req []byte) (Response, error) {
	req = orEmpty(req)
	packed := storageInsert(unsafe.Pointer(&req[0]), int32(len(req)))
	runtime.KeepAlive(req)
	return finish("storage.insert", packed)
}

func StorageGet(req []byte) (Response, error) {
	req = orEmpty(req)
	packed := storageGet(unsafe.Pointer(&req[0]), int32(len(req)))
	runtime.KeepAlive(req)
	return finish("storage.get", packed)
}

func StorageUpdate(req []byte) (Response, error) {
	req = orEmpty(req)
	packed := storageUpdate(unsafe.Pointer(&req[0]), int32(len(req)))
	runtime.KeepAlive(req)
	return finish("storage.update", packed)
}

func StorageDelete(req []byte) (Response, error) {
	req = orEmpty(req)
	packed := storageDelete(unsafe.Pointer(&req[0]), int32(len(req)))
	runtime.KeepAlive(req)
	return finish("storage.delete", packed)
}

func StorageQuery(req []byte) (Response, error) {
	req = orEmpty(req)
	packed := storageQuery(unsafe.Pointer(&req[0]), int32(len(req)))
	runtime.KeepAlive(req)
	return finish("storage.query", packed)
}

// KV is a per-install best-effort cache: TTL'd, flushable, never truth.

func KVGet(req []byte) (Response, error) {
	req = orEmpty(req)
	packed := kvGet(unsafe.Pointer(&req[0]), int32(len(req)))
	runtime.KeepAlive(req)
	return finish("kv.get", packed)
}

func KVSet(req []byte) (Response, error) {
	req = orEmpty(req)
	packed := kvSet(unsafe.Pointer(&req[0]), int32(len(req)))
	runtime.KeepAlive(req)
	return finish("kv.set", packed)
}

func KVDelete(req []byte) (Response, error) {
	req = orEmpty(req)
	packed := kvDelete(unsafe.Pointer(&req[0]), int32(len(req)))
	runtime.KeepAlive(req)
	return finish("kv.delete", packed)
}

// Blob is windowed access to content-addressed bytes.

func BlobRead(req []byte) (Response, error) {
	req = orEmpty(req)
	packed := blobRead(unsafe.Pointer(&req[0]), int32(len(req)))
	runtime.KeepAlive(req)
	return finish("blob.read", packed)
}

func BlobAppend(req []byte) (Response, error) {
	req = orEmpty(req)
	packed := blobAppend(unsafe.Pointer(&req[0]), int32(len(req)))
	runtime.KeepAlive(req)
	return finish("blob.append", packed)
}

// EventsEmit appends to the platform event log.
func EventsEmit(req []byte) (Response, error) {
	req = orEmpty(req)
	packed := eventsEmit(unsafe.Pointer(&req[0]), int32(len(req)))
	runtime.KeepAlive(req)
	return finish("events.emit", packed)
}

//go:wasmimport hive_sanitize sanitize
func hostSanitize(ptr unsafe.Pointer, size int32) uint64

// Sanitize is the only path from untrusted to trusted, and the only thing that
// can clear this invocation's taint.
//
// It is not a function most apps should link. Declaring `sanitize` in a
// manifest requires a grant, every call writes an audit row, and the host
// resolves both rather than believing anything the guest says. If you are
// reaching for this to make a warning go away, the answer is no: the taint is
// telling the truth about where your data came from.
func Sanitize(req []byte) (Response, error) {
	req = orEmpty(req)
	packed := hostSanitize(unsafe.Pointer(&req[0]), int32(len(req)))
	runtime.KeepAlive(req)
	return finish("sanitize", packed)
}

func orEmpty(req []byte) []byte {
	if len(req) == 0 {
		return []byte("{}")
	}
	return req
}

// Layout of the i64 a capability call returns:
//
//	bits  0..31  size of the response, in bytes
//	bits 32..39  trust: 0 trusted, 1 untrusted
//	bits 40..47  status
//
// The size comes back WITH the status, which is the fix for ABI v1's worst
// footgun. Asking the host how big the last result was, in a separate call,
// against a slot the next host call overwrites, failed silently the moment two
// calls got reordered.
const (
	statusShift = 40
	trustShift  = 32
	sizeMask    = uint64(1)<<32 - 1
	byteMask    = uint64(0xff)
)

// finish unpacks the header and reads the envelope straight away, because the
// next host call overwrites the slot.
func finish(op string, packed uint64) (Response, error) {
	status := Status(uint8(packed >> statusShift & byteMask))
	level := Trusted
	if packed>>trustShift&byteMask == uint64(Untrusted) {
		level = Untrusted
	}
	size := int32(packed & sizeMask)

	body := readResult(size)
	if status != StatusOK {
		// A failure carries the host's message as plain text, not an envelope.
		return Response{Trust: Untrusted}, &HostError{Op: op, Status: status, Message: string(body)}
	}

	var env struct {
		Trust string          `json:"trust"`
		Data  json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return Response{Trust: Untrusted}, &HostError{
			Op: op, Status: StatusError, Message: "malformed response envelope: " + err.Error(),
		}
	}
	// The header wins over the envelope field. Both come from the host and
	// agree, but the header cannot be reshaped by anything downstream, and when
	// two sources of a security property disagree the less malleable one is the
	// right answer.
	return Response{Trust: level, Data: env.Data}, nil
}

func readResult(size int32) []byte {
	if size <= 0 {
		return nil
	}
	buf := make([]byte, size)
	got := resultRead(unsafe.Pointer(&buf[0]), size)
	runtime.KeepAlive(buf)
	if got < 0 || got > size {
		return nil
	}
	return buf[:got]
}
