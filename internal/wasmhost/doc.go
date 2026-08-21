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
// uses: ask for a size, allocate that many bytes yourself, ask the host to copy
// into them. The host never calls back into a guest allocator, so a guest may
// use whatever allocator its toolchain ships.
//
// # Host modules
//
// One host module per capability domain (Andi's finding 5). A guest may import
// only `wasi_snapshot_preview1` plus the `hive_*` modules its manifest grants.
// The check runs against the compiled module's import section before the first
// instantiation, so an undeclared capability is a link error rather than a
// runtime denial, and a wasip2 or component-model import cannot load at all.
//
//	hive_abi       always available: the call protocol
//	  abi_version()                      -> i32   ABIVersion
//	  input_size()                       -> i32   bytes of JSON input
//	  input_read(ptr i32)                        copy input to ptr
//	  output_write(ptr i32, len i32)     -> i32   set the call result
//	  error_write(ptr i32, len i32)      -> i32   set the call error
//	  result_size()                      -> i32   bytes from the last host call
//	  result_read(ptr i32)                       copy that result to ptr
//
//	hive_log       CapLog
//	  log(level i32, ptr i32, len i32)
//
//	hive_storage   CapStorage    insert get update delete query
//	hive_kv        CapKV         get set delete
//	hive_blob      CapBlob       read append
//	hive_events    CapEvents     emit
//
// Every capability function has the shape `(reqPtr i32, reqLen i32) -> i32`.
// The request is JSON. The i32 is a Status. On StatusOK the JSON response sits
// in the result slot; on anything else the result slot holds the error message
// as plain text. One host call overwrites the previous result slot, so read it
// before making another call.
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
package wasmhost
