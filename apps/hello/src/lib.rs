//! The reference guest: the smallest app that exercises every part of the ABI,
//! and the fixture the host's conformance tests run.
//!
//! It is deliberately not a demo. Each export is a host behaviour that needs a
//! guest on the other end of it to be tested honestly: a normal call, a guest
//! error, a runaway loop, a blocking host call, a memory ceiling, and a compute
//! body for the benchmarks.
//!
//! Built as a reactor: a `cdylib` for `wasm32-wasip1`, with the `_initialize`
//! the SDK exports; the host calls that once, then individual exports. There
//! is no `main`.

use std::hint::black_box;

use hive_guest::handle;
use serde::{Deserialize, Serialize};

#[derive(Deserialize, Default)]
#[serde(default)]
struct GreetIn {
    name: String,
}

#[derive(Serialize)]
struct GreetOut {
    message: String,
    abi: i32,
}

#[unsafe(no_mangle)]
pub extern "C" fn hello() -> i32 {
    handle(|input| {
        let mut req = GreetIn::default();
        if !input.is_empty() {
            req = serde_json::from_slice(&input)
                .map_err(|e| format!("hello: input is not JSON: {e}"))?;
        }
        if req.name.is_empty() {
            req.name = "world".into();
        }
        serde_json::to_vec(&GreetOut {
            message: format!("hello, {}", req.name),
            abi: hive_guest::abi_ver(),
        })
        .map_err(|e| e.to_string())
    })
}

#[unsafe(no_mangle)]
pub extern "C" fn fail() -> i32 {
    handle(|_| Err("this guest fails on purpose".into()))
}

#[unsafe(no_mangle)]
pub extern "C" fn logline() -> i32 {
    handle(|input| {
        hive_guest::log(
            hive_guest::LEVEL_INFO,
            &format!("hello guest says: {}", String::from_utf8_lossy(&input)),
        );
        Ok(br#"{"logged":true}"#.to_vec())
    })
}

/// Proves the capability path end to end and, with a host-side data layer
/// that blocks, proves invariant 7: a guest parked inside a host call has to
/// come back when the call's deadline ends.
#[unsafe(no_mangle)]
pub extern "C" fn store_query() -> i32 {
    handle(|input| {
        let req = if input.is_empty() {
            br#"{"collection":"entries"}"#.to_vec()
        } else {
            input
        };
        Ok(hive_guest::storage_query(&req)?.data)
    })
}

/// The attack, written as an export so the host's defence is tested against a
/// guest genuinely trying rather than against a mock.
///
/// It reads (possibly untrusted) data, writes it straight back claiming the
/// content is fine, and returns it as its own output. Under D22 every one of
/// those steps is recorded untrusted anyway, because the host never asks the
/// guest what the provenance is.
#[unsafe(no_mangle)]
pub extern "C" fn launder() -> i32 {
    handle(|_| {
        let read = hive_guest::storage_query(br#"{"collection":"entries"}"#)?;
        // A guest saying "trusted" in a request body. The host does not read it.
        hive_guest::storage_insert(br#"{"collection":"entries","trust":"trusted"}"#)?;
        Ok(read.data)
    })
}

/// Lets a test see what the guest was told, as opposed to what the host
/// recorded. The two must agree.
#[unsafe(no_mangle)]
pub extern "C" fn report_trust() -> i32 {
    handle(|_| {
        let res = hive_guest::storage_query(br#"{"collection":"entries"}"#)?;
        Ok(format!(
            r#"{{"input":"{}","response":"{}"}}"#,
            hive_guest::input_trust_level(),
            res.trust
        )
        .into_bytes())
    })
}

/// The runaway guest. black_box keeps the loop from being optimized away.
#[unsafe(no_mangle)]
pub extern "C" fn spin() -> i32 {
    let mut sink: u64 = 0;
    let mut i: u64 = 0;
    loop {
        sink = sink.wrapping_add(black_box(i));
        i = i.wrapping_add(1);
        black_box(sink);
    }
}

/// Walks past the memory ceiling. memory.grow returning -1 is an allocation
/// failure inside the guest, not a host crash, so this surfaces as a guest-side
/// out-of-memory rather than as anything the daemon has to survive.
#[unsafe(no_mangle)]
pub extern "C" fn grow() -> i32 {
    handle(|_| {
        let mut held: Vec<Vec<u8>> = Vec::new();
        for i in 0..(1u32 << 20) {
            let mut chunk: Vec<u8> = Vec::new();
            // try_reserve rather than a bare allocation: the failure is the
            // point, and reporting it beats aborting on it.
            chunk
                .try_reserve_exact(1 << 20)
                .map_err(|_| format!("out of memory after {} MiB", held.len()))?;
            chunk.resize(1 << 20, 0);
            chunk[0] = i as u8;
            held.push(chunk);
        }
        Ok(format!(r#"{{"held":{}}}"#, held.len()).into_bytes())
    })
}

#[derive(Deserialize, Default)]
#[serde(default)]
struct SumIn {
    n: u64,
}

/// The benchmark body: a known amount of pure compute. An xorshift rather than
/// an accumulate on purpose: `total += i` has a closed form the optimizer
/// recognises, and each step here depends on the last.
#[unsafe(no_mangle)]
pub extern "C" fn sum() -> i32 {
    handle(|input| {
        let mut req = SumIn::default();
        if !input.is_empty() {
            req = serde_json::from_slice(&input).map_err(|e| e.to_string())?;
        }
        let mut state: u64 = 0x9e37_79b9_7f4a_7c15;
        for _ in 0..req.n {
            state ^= state << 13;
            state ^= state >> 7;
            state ^= state << 17;
        }
        Ok(format!(r#"{{"total":{state}}}"#).into_bytes())
    })
}

/// Instantiation and call overhead with nothing in the middle: the floor the
/// pool config is measured against.
#[unsafe(no_mangle)]
pub extern "C" fn noop() -> i32 {
    0
}
