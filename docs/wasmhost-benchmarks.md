# wazero, measured

D0 flagged three numbers as unverified and unpublished, and said they should
shape the pool config rather than a guess: instantiation latency, per-instance
bookkeeping overhead, and the real cost of `WithCloseOnContextDone` on our call
pattern. Here they are, plus two that turned out to matter more than any of
them.

Reproduce:

```
go test ./internal/wasmhost/ -run '^$' -bench . -benchmem
```

Machine: AMD Ryzen 7 9800X3D, Windows 11, Go 1.26.7, wazero 1.12.0,
TinyGo 0.41.1, binaryen 132. Guest: `apps/hello`, 930 KB, WASI preview1
reactor. Every number is the median of two runs at `-benchtime 2s`.

## The three

### 1. Instantiation latency: 76-91 us

| | time | allocations |
|---|---|---|
| `New` (runtime + WASI + 6 host modules) | 405 us | 383 KB, 1619 allocs |
| `CompileModule` (930 KB guest, cold) | 47 ms | 12 MB, 20377 allocs |
| `InstantiateModule` from a compiled module | **83 us** | 154 KB, 314 allocs |
| warm call (pooled instance) | **10.1 us** | 12.5 KB, 12 allocs |
| cold call (instantiate every time) | **150 us** | 167 KB, 334 allocs |

**A pool hit is 15x a pool miss.** That settles whether the warmth pool earns
its complexity: it does, decisively. It also means a pool budget too small for
the working set is a 15x latency regression rather than a rounding error, which
is worth an alert once there are metrics to alert on.

Compilation at 47 ms is 570x an instantiation, which is what the single-flight
and the on-disk cache exist for. A restart with a cold cache pays it once per
distinct module; a restart with a warm one does not pay it at all.

### 2. Per-instance bookkeeping: 10-20 KB against 131 KB of linear memory

| | bytes per instance |
|---|---|
| wasm linear memory | 131,072 (2 pages) |
| everything else wazero allocates | 10,000-20,000 |

**Memory dominates, somewhere between 6:1 and 13:1.** Bounding the pool by
summed wasm memory rather than by instance count is measuring the right thing,
and Andi's call holds.

> **Correction.** The first version of this section said "~10 KB" and "13 to 1"
> as if they were one measurement. They were not: `BenchmarkInstanceFootprint`
> subtracted `instanceOverheadBytes` from its own result, because `memBytes()`
> already adds that constant, so it reported `realOverhead - 16384` and went
> negative. The figures above are the honest range across runs and machines; the
> spread is heap-accounting noise, and reporting one run as if it were the answer
> is what produced the error in the first place. The benchmark now reports the
> ratio itself rather than leaving it to be derived by hand in a document.
>
> Consequence: `instanceOverheadBytes` was raised from 16 KB to **32 KB**, above
> the top of the measured range. Over-counting costs a few idle instances;
> under-counting silently overcommits the box.

Two things worth knowing beyond the ratio:

- **A guest uses 131 KB, not the 16 MB its ceiling allows.** The cap is a
  ceiling, not a reservation, so budgeting against the cap would undercount
  capacity by 128x. The pool measures `Memory().Size()` per instance on release,
  after growth, which is the only number that is true.
- The 10 KB of bookkeeping is counted anyway (`instanceOverheadBytes`), so a
  512 MB budget means 512 MB. At these sizes it holds roughly 3,500 idle
  instances.

### 3. `WithCloseOnContextDone`: 2x to 18x, so it is per-module

wazero's own floor, on a hand-written module with no toolchain in the way:

| | per call |
|---|---|
| termination off | 18.3 ns, 1 alloc |
| termination on | 235 ns, 4 allocs |

On the real guest:

| call | off | on | ratio |
|---|---|---|---|
| `noop` | 2.95 us | 2.98 us | **1.0x** |
| `hello` (JSON in, JSON out, 2 ABI crossings) | 9.98 us | 44.8 us | **4.5x** |
| 2M-iteration compute loop | 2.41 ms | 42.2 ms | **17.4x** |

wazero describes this as "a bit of extra cost." On a tight loop it is 17x,
because the check lands on the loop back-edge and the body is six instructions.

> **Correction, and it changed the code.** The `noop` row used to read 2.23 us
> against 4.57 us, and Augie was right that the 2.3 us gap was not wazero: it was
> a **watchdog goroutine spawned on every call**, which the runtime floor row
> exposes as the control (wazero's own per-call cost is 250 ns, not 2,300).
>
> The watchdog is now a `context.AfterFunc` callback, which registers and spawns
> nothing unless the deadline actually fires. `noop` is a dead heat, so the
> per-call cost of enabling termination is now genuinely zero.
>
> The 4.5x on `hello` survived that fix unchanged, which is the useful part: it
> is not per-call overhead at all, it is the checks instrumenting guest code, and
> `hello` runs a lot of guest code (TinyGo's JSON decode). The runtime floor
> measures a module whose body is one instruction, so it can only ever see setup
> cost. **Per-module termination is still the right design and is now justified
> by a number that measures the thing it names.**

**So the answer to "measure before enabling it globally" is: do not enable it
globally, and do not disable it globally either.** It is now `Module.Termination`,
resolved per module, defaulting ON:

- **Default ON reverses wazero's default deliberately.** Without the checks
  there is nothing for `CloseWithExitCode` to act on, so a runaway guest cannot
  be killed at all. This platform hot-loads AI-written code. "Unkillable"
  should not be something a module gets by failing to ask.
- **`TerminationOff` is for audited first-party apps** that came through the
  born-green gate. It lines up with D10.9's trust tiers: `builtin` as
  configured, `local` and `imported` keep their checks and pay the 4x.

The one thing that is *not* optional either way: **every blocking host function
takes a context and returns on cancellation** (invariant 7). The checks live in
guest code, so termination cannot reach a guest parked inside a host call. With
checks off, host functions honoring their context is the *only* thing that ends
such a call. `TestHostFunctionHonorsContext` is the regression test and it runs
under both engines.

## The two nobody asked about, which mattered more

### 4. TinyGo's default scheduler costs 24x per call

| call | default scheduler | `-scheduler=none` |
|---|---|---|
| `noop` | 80.8 us | **3.3 us** |
| `hello` | 110.0 us | **11.3 us** |

An 80 us prologue on a call that does nothing. wazero's own floor for the same
shape is 18 ns, so this is entirely the guest toolchain: TinyGo's wasip1
scheduler is asyncify-based, and asyncify instruments functions with
save-and-restore machinery so a coroutine can unwind.

Guests never need it. A guest holds no sockets, no files and no ambient state
(invariant 5), so it has nothing to suspend. `-scheduler=none` is in the build
template, and it also makes a guest that starts a goroutine fail to compile,
which turns invariant 5 from a convention into a build error.

Without this the whole picture is different: at 80 us per call, instantiation
(83 us) and a warm call would have cost the same, and the pool would have looked
pointless. It was worth chasing rather than accepting.

### 5. The interpreter is 60x slower, so the canary is a canary

| engine | 200k-iteration loop |
|---|---|
| optimizing compiler | 249 us |
| interpreter | 14.9 ms |

Compiler-versus-interpreter divergence is a recurring wazero issue class, so
every behavioural test runs under both (`forEachEngine`, and every test named
`TestConformance*`). CI gets it from `go test ./...`. But 60x means the
interpreter is a correctness instrument only. Nobody should reach for it to save
memory in production.

## What these numbers set

| setting | value | why |
|---|---|---|
| `PoolMemoryBudget` | 512 MB | ~3,200 idle instances at 163 KB each |
| `ReservedMemoryBudget` | a quarter of the pool | pinned memory is unevictable, so it needs its own ceiling (D9.3) |
| `IdleTTL` | 5 min | instantiation is 83 us, so re-warming is cheap; holding memory is not |
| `MemoryTiers` | 256 / 1024 / 4096 pages | actual use is 2 pages, so the smallest tier already has 128x headroom |
| `instanceOverheadBytes` | 32 KB | above the top of the measured 10-20 KB range; see the correction in 2 |
| `DefaultTermination` | on | see 3, and it now costs nothing per call |
| guest build | `-scheduler=none`, no `-opt` | see 4, and `scripts/guest-build.md` |

Two of these came from numbers that were wrong the first time, and both errors
had the same shape: a figure derived by hand in prose rather than reported by
the code that measured it. The benchmarks now emit the ratio and the overhead
directly.

## Caveats

One machine, one OS, one guest. The ratios should travel; the absolute numbers
are Windows on a 9800X3D, and Linux is where this actually runs. Re-run on the
deployment host before treating the pool budget as tuned rather than as a
starting point.
