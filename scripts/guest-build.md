# The guest build template

A guest is a Rust `cdylib` for `wasm32-wasip1`, built by `scripts/build-guests.sh`
(or `.ps1`) with the profile in the app's `Cargo.toml`. Every setting is
load-bearing. This is why.

```toml
[lib]
crate-type = ["cdylib"]

[profile.release]
opt-level = "z"
lto = true
codegen-units = 1
panic = "abort"
strip = true
```

## `wasm32-wasip1`

WASI preview 1, and it does not change. A guest that imports wasip2 or a
component-model interface will not load: the host rejects unknown import
modules at link time in `check_module`, so this is enforced rather than merely
documented. The host also allows only a short list of WASI functions
(`hive_wasmhost::ALLOWED_WASI`): clocks, randomness, stdout and stderr, the
process-shape calls a runtime makes at startup. No filesystem, no sockets, no
`poll_oneoff`. A guest that reaches for `std::fs` or `std::thread::sleep`
fails at link time rather than at run time.

## `cdylib`, which is a reactor

A reactor exports `_initialize` instead of `_start`; the host calls it once
after instantiation and then individual exports. A crate that exports `_start`
is a command that runs once and exits, which is the wrong shape for an app, and
`crates/hive-registry` refuses a module without `_initialize` at install time.

rustc links a `wasm32-wasip1` cdylib with `--no-entry` and no reactor crt, so
nothing exports `_initialize` on its own. **The SDK exports it**: linking
`hive-guest` gives the module its `_initialize`, an empty one, because Rust's
std on wasi initialises lazily and a guest holds no state to set up. A guest
that does not link the SDK has to export it itself.

Export one function per manifest function, signature `extern "C" fn() -> i32`,
body wrapped in `hive_guest::handle`.

## `panic = "abort"`

A panic becomes a trap. The message still reaches stderr, which the host routes
into the daemon log with app attribution, before the trap fires. Unwinding
needs a runtime a guest has no use for, and the host treats a trap and a
guest-reported error the same way: the call fails, the instance is discarded.

## `opt-level = "z"`, `lto`, one codegen unit

Size first. The module is compiled by wasmtime anyway and its own optimizer
does the work that matters, so a bigger module buys little speed and costs
compile time on the first call. The reference guest is about 90 KB with
serde_json in it, against 930 KB for the TinyGo build it replaced.

## Inside a guest

- **serde_json works** and is the expected way to read the input and write the
  output.
- **Check what `output` returns.** A result over the host's size limit is
  refused, and a guest that returns success anyway turns it into a silent empty
  response. `hive_guest::handle` does this for you.
- **Trust is not yours to set.** Every capability response is a
  `hive_guest::Response { trust, data }`, and the host tracks the invocation's
  taint independently: read anything untrusted and everything you write
  afterwards is recorded untrusted, whatever you claim. `input_trust_level`
  exists so an app can refuse before putting text into instruction position,
  not so it can argue. Raising trust needs the `sanitize` capability, a grant,
  and an audit row.
- **Allocation failure is a guest failure.** `memory.grow` returning -1 is an
  allocation error inside the guest; use `try_reserve` where running out is a
  real possibility so it is reported rather than aborted on.
- **`unsafe` is allowed here and only here.** The SDK calls the host's imports,
  which are `extern "C"`; the host workspace forbids `unsafe` entirely, and the
  guest crates are separate workspaces for exactly that reason.

## The frozen TinyGo fixture

`crates/hive-wasmhost/testdata/hello-tinygo.wasm` is the last TinyGo build of
the reference guest, kept as an ABI conformance fixture (D31). It is never
rebuilt; the host suite runs a handful of tests against it so a change that only
works for guests built the way we build them is caught.
