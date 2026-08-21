# The guest build template

Every flag in `build-guests.sh` / `build-guests.ps1` is load-bearing. This is why.

```
tinygo build -target=wasip1 -buildmode=c-shared -scheduler=none -o app.wasm ./
```

## `-target=wasip1`

WASI preview1, and it does not change. wazero has no component-model support
and the two upstream issues tracking it are not close (D0, Andi finding 6). A
guest that imports wasip2 or a component-model interface will not load: the host
rejects unknown import modules at link time in `checkModule`, so this is
enforced rather than merely documented.

## `-buildmode=c-shared`

Produces a **reactor**: the module exports `_initialize` instead of `_start`,
and the host instantiates it with `WithStartFunctions("_initialize")`. A guest
that exports `_start` is a command that runs once and exits, which is the wrong
shape for an app.

## `-scheduler=none`

**This flag is worth 24x on a call.** Measured on the reference guest:

| call | default scheduler | `-scheduler=none` |
|---|---|---|
| `noop` | 80.8 us | 3.3 us |
| `hello` (JSON in and out) | 110.0 us | 11.3 us |

TinyGo's default scheduler for wasip1 is asyncify-based, and asyncify
instruments functions with save-and-restore state machinery so a coroutine can
unwind. Guests never need it: a guest is a pure request-and-response function
that holds no sockets, no files and no ambient state (invariant 5), so it has
nothing to suspend.

The flag is also an enforcement point. With `-scheduler=none`, a guest that
starts a goroutine fails to compile. Invariant 5 stops being a convention.

## No `-opt` flag: TinyGo's `-opt=z` default wins outright

The obvious move is to trade size for speed. Measured, it is not a trade at all:
`-opt=z` is smallest AND fastest here, so there is nothing to buy.

| `-opt` | module size | `hello` call | 1M-iteration loop | wazero compile |
|---|---|---|---|---|
| `z` (default) | 930 KB | 9.8 us | 1.207 ms | 47 ms |
| `s` | 1033 KB | 14.8 us | 1.204 ms | 53 ms |
| `1` | 1387 KB | 12.6 us | 1.202 ms | 80 ms |
| `2` | 1360 KB | 12.4 us | 1.191 ms | 77 ms |

Compute is a wash to within 1.5%, because wazero recompiles the wasm anyway and
its own optimizer does the work that matters. The higher levels inline more,
which makes the module bigger, which makes wazero's compile slower and hurts
short calls. Size and speed point the same way, so leave it alone.

## What is deliberately NOT here

- **`-panic=trap`.** It saves nothing measurable and throws away the panic
  message. Guest stderr is routed into the daemon log with app attribution, and
  for AI-written code that message is the whole debugging story.
- **`-gc=leaking`.** Instances are pooled and reused across calls, so a guest
  that never frees would grow until it hit its memory ceiling. Only correct for
  one-shot commands.

## Inside a guest

- **`encoding/json` works.** TinyGo's `reflect` is famously incomplete, and the
  standing advice is to avoid reflect-based JSON in guests. On TinyGo 0.41.1 it
  round-trips structs with string, integer and boolean fields correctly, which
  the host conformance tests exercise on every run. It is not free: it is most
  of why the reference guest is 930 KB rather than something much smaller.
  Verify before trusting it on a nested or interface-typed shape.
- **Export with `//go:wasmexport`**, one export per manifest function, signature
  `func() int32`.
- **`//go:wasmimport` functions cannot be used as values.** That is why the SDK
  spells out every capability verb instead of routing them through one helper.
  It is the better shape anyway: the linker drops the import for every verb the
  app does not call, so a guest links exactly the capabilities it uses.
- **Check what `Output` returns.** A result over the host's size limit is
  refused, and a guest that returns success anyway turns it into a silent empty
  response. `guest.Handle` does this for you.
- **Trust is not yours to set.** Every capability response is a
  `guest.Response{Trust, Data}`, and the host tracks the invocation's taint
  independently: read anything untrusted and everything you write afterwards is
  recorded untrusted, whatever you claim. `guest.InputTrust()` exists so an app
  can refuse before putting text into instruction position, not so it can
  argue. Raising trust needs the `sanitize` capability, a grant, and an audit
  row.
- **No `time.Sleep`, and it will not compile past the host's link check.**
  `poll_oneoff` is not on the WASI allowlist because nothing in the host can
  interrupt it. A guest that needs to wait is a workflow step that needs a
  timer.

## Toolchain

TinyGo shells out to binaryen's `wasm-opt`; the TinyGo release archive does not
include it. Both must be on PATH.

- TinyGo 0.41.1
- binaryen 132
