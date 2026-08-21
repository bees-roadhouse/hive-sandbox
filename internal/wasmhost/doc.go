// Package wasmhost runs WASM guest apps on wazero behind a JSON ABI.
//
// # The guest contract
//
// A guest is a WASI preview1 **reactor**: it exports `_initialize`, never
// `_start`, and it holds no sockets, no files and no ambient state (invariant
// 5). Anything long-lived is a host service the manifest declares as a
// capability.
//
// A guest exposes one exported function per manifest function. Each takes no
// arguments and returns one i32 status:
//
//	0        the call succeeded; the guest wrote its JSON result with output_write
//	nonzero  the call failed; the guest wrote a message with error_write
//
// Input and output do not travel through the wasm signature. They move through
// the `hive_abi` host module, which is the same idiom every other host call
// uses: learn the size, allocate that many bytes yourself, ask the host to copy
// into them. The host never calls back into a guest allocator, so a guest may
// use whatever allocator its toolchain ships. Every copy takes the guest's
// buffer length and never writes past it.
//
// # Host modules
//
// One host module per capability domain (Andi's finding 5). A guest may import
// only the allowlisted WASI preview1 functions plus the `hive_*` modules its
// manifest grants. The check runs against the compiled module's import section,
// so an undeclared capability is a link error rather than a runtime denial, and
// a wasip2 or component-model import cannot load at all.
//
// The WASI allowlist is per FUNCTION rather than per module, and that is not
// tidiness. Allowing the module wholesale hands a guest `poll_oneoff`, which
// wazero implements with a context-free sleep for a duration the guest picks;
// nothing in the host can interrupt it, so `time.Sleep` in a guest hangs the
// call forever. An ordinary retry backoff does this. The allowlist is also
// invariant 5 in code: no sock_* because guests hold no sockets, no path_*
// because guests hold no files.
//
//	hive_abi       always available: the call protocol
//	  abi_version()                      -> i32   ABIVersion
//	  input_size()                       -> i32   bytes of JSON input
//	  input_read(ptr i32, len i32)       -> i32   copy input, returns bytes written
//	  input_trust()                      -> i32   0 trusted, 1 untrusted
//	  output_write(ptr i32, len i32)     -> i32   set the call result; CHECK THIS
//	  error_write(ptr i32, len i32)      -> i32   set the call error
//	  result_read(ptr i32, len i32)      -> i32   copy the last response
//
//	hive_log       CapLog
//	  log(level i32, ptr i32, len i32)
//
//	hive_storage   CapStorage    insert get update delete query
//	hive_kv        CapKV         get set delete
//	hive_blob      CapBlob       read append
//	hive_events    CapEvents     emit
//	hive_sanitize  CapSanitize   sanitize
//
// Every capability function has the shape `(reqPtr i32, reqLen i32) -> i64`.
// The request is JSON. The i64 packs the whole answer:
//
//	bits  0..31  size of the response, in bytes
//	bits 32..39  trust: 0 trusted, 1 untrusted
//	bits 40..47  Status
//
// On StatusOK the result slot holds a `{"trust": ..., "data": ...}` envelope; on
// anything else it holds the error message as plain text. The size comes back
// WITH the status because ABI v1 made the guest ask for it separately, against a
// slot the next host call overwrites, which failed silently whenever two calls
// were reordered.
//
// # Trust
//
// Every capability response is structurally `{trust, data}`. There is no shape
// in which the marker is absent, so a guest cannot drop it by forgetting.
//
// That is the convenient half. The enforcement is that **taint is host-tracked,
// per invocation, and monotonic** (D22, invariant 12). If any response comes
// back untrusted, every request the guest makes afterwards carries Untrusted,
// and so does the guest's own output. The guest is never asked what the
// provenance is, so it cannot launder untrusted content by reading it and
// writing it back, and `apps/hello`'s `launder` export exists to prove that
// against a guest genuinely trying.
//
// It is coarse on purpose. A guest that reads untrusted data and then writes
// something unrelated gets marked, and that false positive is the price of never
// having to trust the guest's account of what it did with the bytes.
//
// Two consequences. `Sanitize` is the only thing that raises trust, and it needs
// a granted capability and writes an audit row, because "this is safe now" is
// exactly the claim a compromised guest would make. And an instance that touched
// untrusted bytes is destroyed rather than pooled: taint is per-invocation but
// guest MEMORY is not, and whatever the guest parsed or buffered is still in
// linear memory when the next call borrows it.
//
// # Cancellation
//
// Every host function that can block takes the call's context.Context and
// returns on cancellation (invariant 7, D9.7). This is not politeness. wazero
// terminates a guest by inserting periodic checks into *guest* code, so a guest
// parked inside a host call is unkillable unless that host call comes back on
// its own. The wrapper in hostmods.go checks the context before and after every
// capability call for exactly that reason, and TestHostFunctionHonorsContext
// is the regression test.
//
// # Termination
//
// wazero has no epoch interruption; that is a Wasmtime concept. Termination is
// RuntimeConfig.WithCloseOnContextDone(true) plus a per-call context deadline,
// with a watchdog calling CloseWithExitCode. The checks cost throughput, so
// Config.TerminateOnContextDone is a knob and BenchmarkTermination measures it.
// A terminated instance is dead, not paused: it never goes back in the pool.
//
// # Warmth
//
// Compiled modules are cached per runtime behind a single-flight, because
// wazero does not dedup concurrent CompileModule calls for the same bytes and
// concurrent instances sharing a CompiledModule have a known data race. Compile
// happens on one goroutine; everything after that is read-only sharing.
//
// Instances are pooled in an LRU bounded by summed wasm memory rather than
// instance count, because memory dominates the footprint. Eviction and kill are
// the same primitive: Module.Close.
//
// A warm instance is a cache of guest memory, so the pool key carries more than
// the module: the owner PRINCIPAL, because handing one principal's leftover
// heap to another is an isolation break; and the CAPABILITY set, because the
// link check used to run only on a pool miss and a revoked grant kept working
// for as long as the instance stayed warm.
//
// The pool bounds IDLE memory only. Live instances are bounded by a Limiter,
// which resolves each caller's limits itself rather than accepting them as a
// parameter (invariant 11). Pinned instances (D9.3) are a second budget: they
// reserve their memory ceiling up front, never enter the LRU, and the caller
// owns their lifetime.
package wasmhost
