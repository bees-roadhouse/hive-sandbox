//! The host modules a guest imports: `hive_abi`, `hive_log` and one module per
//! capability domain.

use hive_trust::Level;
use wasmtime::{Caller, Linker, Memory};

use crate::abi::{ABI_VERSION, Caller as AbiCaller, Deps, HostError as AbiError, Request, Status};
use crate::host::State;

/// Everything one guest call needs from the host. It lives on the store for
/// the duration of the call, because an instance is leased exclusively to one
/// call and every host function reaches the store.
pub(crate) struct CallState {
    pub(crate) caller: AbiCaller,
    pub(crate) app: String,
    pub(crate) module_hash: String,
    pub(crate) deps: Deps,
    pub(crate) input: Vec<u8>,
    pub(crate) output: Vec<u8>,
    pub(crate) err_msg: String,
    /// The envelope of the most recent capability call. One host call
    /// overwrites the previous one, which is survivable only because the call
    /// that produced it also handed back its size (D22, `pack_result`).
    pub(crate) result: Vec<u8>,
    /// The invocation's trust, host-tracked and monotonic (D22.2). It starts
    /// at whatever the caller passed in, drops the moment any capability
    /// response comes back untrusted, and is stamped on every request the
    /// guest makes afterwards AND on the guest's own output. The guest is
    /// never asked to participate.
    pub(crate) taint: Level,
    /// The operation that first weakened the taint. Diagnosis, never policy.
    pub(crate) tainted_by: String,
    /// The guest tried to set a result and the host refused it. `Ok` means
    /// either no attempt or a successful one.
    pub(crate) output_rejected: Status,
    pub(crate) max_input: usize,
    pub(crate) max_output: usize,
}

impl CallState {
    /// Folds a level into the taint, monotonically, and records what did it
    /// the first time it actually drops. The first cause is the useful one.
    pub(crate) fn weaken(&mut self, level: Level, cause: &str) {
        let next = Level::weaker(self.taint, level);
        if next != self.taint && self.tainted_by.is_empty() {
            self.tainted_by = cause.to_string();
        }
        self.taint = next;
    }
}

/// Bounds a guest error message: generous for a sentence, small enough that
/// a runaway guest cannot log a gigabyte.
pub(crate) const MAX_ERROR_BYTES: usize = 8 << 10;

const TRUST_BIT_TRUSTED: u64 = 0;
const TRUST_BIT_UNTRUSTED: u64 = 1;
const STATUS_SHIFT: u32 = 40;
const TRUST_SHIFT: u32 = 32;
const SIZE_MASK: u64 = (1 << 32) - 1;
const BYTE_MASK: u64 = 0xff;

/// The i64 every capability function returns:
///
/// ```text
/// bits  0..31  size of the response envelope, in bytes
/// bits 32..39  trust: 0 trusted, 1 untrusted
/// bits 40..47  status
/// ```
///
/// The size travels with the status on purpose. ABI v1 had the guest ask the
/// host how big the last result was, in a separate call, against a slot the
/// next host call overwrote.
pub fn pack_result(status: Status, level: Level, size: usize) -> i64 {
    let bit = if level.is_untrusted() {
        TRUST_BIT_UNTRUSTED
    } else {
        TRUST_BIT_TRUSTED
    };
    let n = (size as u64) & SIZE_MASK;
    let s = status.code() as u64 & BYTE_MASK;
    ((s << STATUS_SHIFT) | (bit << TRUST_SHIFT) | n) as i64
}

/// The guest side of `pack_result`, here so the tests check the two against
/// each other rather than against a comment.
pub fn unpack_result(v: i64) -> (Status, Level, usize) {
    let v = v as u64;
    let status = Status::from_u8(((v >> STATUS_SHIFT) & BYTE_MASK) as u8).unwrap_or(Status::Error);
    let level = if (v >> TRUST_SHIFT) & BYTE_MASK == TRUST_BIT_UNTRUSTED {
        Level::Untrusted
    } else {
        Level::Trusted
    };
    (status, level, (v & SIZE_MASK) as usize)
}

fn abi_len(n: usize) -> i32 {
    n.min(i32::MAX as usize) as i32
}

fn memory_of(caller: &mut Caller<'_, State>) -> Option<Memory> {
    let name = caller.data().memory_name.clone();
    caller.get_export(&name).and_then(|e| e.into_memory())
}

/// Copies `len` bytes at `ptr` out of guest memory. The bytes are copied
/// rather than viewed, so nothing here holds a view across a call that can
/// grow memory.
fn read_guest(
    caller: &mut Caller<'_, State>,
    ptr: i32,
    len: i32,
    max: usize,
) -> Result<Vec<u8>, Status> {
    let (ptr, len) = (ptr as u32 as usize, len as u32 as usize);
    if len > max {
        return Err(Status::Invalid);
    }
    let mem = memory_of(caller).ok_or(Status::Invalid)?;
    let mut buf = vec![0u8; len];
    mem.read(&*caller, ptr, &mut buf)
        .map_err(|_| Status::Invalid)?;
    Ok(buf)
}

/// Copies data into guest memory at `ptr`, never more than the guest says it
/// allocated, and returns how many bytes it wrote.
///
/// The cap is the interesting part. Trusting the host's length would let a
/// stale size on the guest side overrun the guest's own heap: wasm bounds
/// checking protects the HOST from that, not the guest from itself.
fn write_guest(caller: &mut Caller<'_, State>, ptr: i32, room: i32, data: &[u8]) -> i32 {
    let (ptr, room) = (ptr as u32 as usize, room as u32 as usize);
    let n = data.len().min(room);
    if n == 0 {
        return 0;
    }
    let Some(mem) = memory_of(caller) else {
        return 0;
    };
    if mem.write(&mut *caller, ptr, &data[..n]).is_err() {
        return 0;
    }
    abi_len(n)
}

fn state<'a, 'b>(caller: &'a mut Caller<'b, State>) -> Option<&'a mut CallState> {
    caller.data_mut().call.as_mut()
}

/// Registers every host module into one linker. WASI is the caller's.
pub(crate) fn add_host_modules(linker: &mut Linker<State>) -> wasmtime::Result<()> {
    add_abi_module(linker)?;
    add_log_module(linker)?;
    add_capability_modules(linker)?;
    Ok(())
}

/// `hive_abi`: the call protocol every guest uses, always available.
fn add_abi_module(linker: &mut Linker<State>) -> wasmtime::Result<()> {
    linker.func_wrap("hive_abi", "abi_version", |_: Caller<'_, State>| -> i32 {
        ABI_VERSION
    })?;
    linker.func_wrap(
        "hive_abi",
        "input_size",
        |mut caller: Caller<'_, State>| -> i32 {
            state(&mut caller)
                .map(|st| abi_len(st.input.len()))
                .unwrap_or(0)
        },
    )?;
    linker.func_wrap(
        "hive_abi",
        "input_read",
        |mut caller: Caller<'_, State>, ptr: i32, len: i32| -> i32 {
            let Some(input) = state(&mut caller).map(|st| st.input.clone()) else {
                return 0;
            };
            write_guest(&mut caller, ptr, len, &input)
        },
    )?;
    // input_trust exists because a guest may legitimately want to refuse
    // before putting text in instruction position. Convenience, not
    // enforcement: taint is tracked host-side either way.
    linker.func_wrap(
        "hive_abi",
        "input_trust",
        |mut caller: Caller<'_, State>| -> i32 {
            match state(&mut caller) {
                // No state means no idea, and the safe direction for a question
                // about provenance is downward.
                None => TRUST_BIT_UNTRUSTED as i32,
                Some(st) if st.taint.is_untrusted() => TRUST_BIT_UNTRUSTED as i32,
                Some(_) => TRUST_BIT_TRUSTED as i32,
            }
        },
    )?;
    linker.func_wrap(
        "hive_abi",
        "output_write",
        |mut caller: Caller<'_, State>, ptr: i32, len: i32| -> i32 {
            let Some(max) = state(&mut caller).map(|st| st.max_output) else {
                return Status::Error.code();
            };
            match read_guest(&mut caller, ptr, len, max) {
                Ok(data) => {
                    let st = state(&mut caller).expect("state was present a moment ago");
                    st.output = data;
                    st.output_rejected = Status::Ok;
                    Status::Ok.code()
                }
                Err(status) => {
                    // Remember the refusal. Without this the guest's own status
                    // check is the only thing between an oversized result and a
                    // successful EMPTY one, and every AI-written guest is a copy
                    // of the SDK lines that once dropped it.
                    if let Some(st) = state(&mut caller) {
                        st.output_rejected = status;
                    }
                    status.code()
                }
            }
        },
    )?;
    linker.func_wrap(
        "hive_abi",
        "error_write",
        |mut caller: Caller<'_, State>, ptr: i32, len: i32| -> i32 {
            if state(&mut caller).is_none() {
                return Status::Error.code();
            }
            match read_guest(&mut caller, ptr, len, MAX_ERROR_BYTES) {
                Ok(data) => {
                    if let Some(st) = state(&mut caller) {
                        st.err_msg = String::from_utf8_lossy(&data).into_owned();
                    }
                    Status::Ok.code()
                }
                Err(status) => status.code(),
            }
        },
    )?;
    // No result_size: the size comes back from the call that produced the
    // result, and this copies at most the length the guest says it allocated.
    linker.func_wrap(
        "hive_abi",
        "result_read",
        |mut caller: Caller<'_, State>, ptr: i32, len: i32| -> i32 {
            let Some(result) = state(&mut caller).map(|st| std::mem::take(&mut st.result)) else {
                return 0;
            };
            let n = write_guest(&mut caller, ptr, len, &result);
            if let Some(st) = state(&mut caller) {
                st.result = result;
            }
            n
        },
    )?;
    Ok(())
}

/// `hive_log`: guests get no files and no stdout worth the name, so this is
/// how an app says anything about itself. Attribution is the host's, never
/// the guest's: an app cannot log as another app because it never names
/// itself.
fn add_log_module(linker: &mut Linker<State>) -> wasmtime::Result<()> {
    linker.func_wrap(
        "hive_log",
        "log",
        |mut caller: Caller<'_, State>, level: i32, ptr: i32, len: i32| {
            let Ok(msg) = read_guest(&mut caller, ptr, len, MAX_ERROR_BYTES) else {
                return;
            };
            let Some(st) = state(&mut caller) else {
                return;
            };
            let msg = String::from_utf8_lossy(&msg);
            let app = st.app.as_str();
            let module = st.module_hash.as_str();
            let actor = st.caller.cred.actor_id;
            let principal = st.caller.cred.principal_id;
            // slog levels: -4 debug, 0 info, 4 warn, 8 error.
            if level >= 8 {
                tracing::error!(app, module, %actor, %principal, "{msg}");
            } else if level >= 4 {
                tracing::warn!(app, module, %actor, %principal, "{msg}");
            } else if level >= 0 {
                tracing::info!(app, module, %actor, %principal, "{msg}");
            } else {
                tracing::debug!(app, module, %actor, %principal, "{msg}");
            }
        },
    )?;
    Ok(())
}

const DOMAINS: &[(&str, &[&str])] = &[
    (
        "hive_storage",
        &["insert", "get", "update", "delete", "query"],
    ),
    ("hive_kv", &["get", "set", "delete"]),
    ("hive_blob", &["read", "append"]),
    ("hive_events", &["emit"]),
    // sanitize is the only verb whose response can RAISE the invocation's
    // trust, which is why it is its own capability domain and why an
    // ordinary app cannot link it.
    ("hive_sanitize", &["sanitize"]),
];

/// Registers every domain a manifest can grant. Which ones a given guest may
/// reach is decided at link time by `check_module`, not here.
fn add_capability_modules(linker: &mut Linker<State>) -> wasmtime::Result<()> {
    for (module, verbs) in DOMAINS {
        for verb in *verbs {
            let (module, verb) = (*module, *verb);
            linker.func_wrap_async(
                module,
                verb,
                move |caller: Caller<'_, State>, (ptr, len): (i32, i32)| {
                    Box::new(capability_call(caller, module, verb, ptr, len))
                },
            )?;
        }
    }
    Ok(())
}

async fn dispatch(
    deps: &Deps,
    module: &str,
    verb: &str,
    req: Request,
) -> Result<crate::abi::Response, AbiError> {
    match (module, verb) {
        ("hive_storage", "insert") => deps.storage.insert(req).await,
        ("hive_storage", "get") => deps.storage.get(req).await,
        ("hive_storage", "update") => deps.storage.update(req).await,
        ("hive_storage", "delete") => deps.storage.delete(req).await,
        ("hive_storage", "query") => deps.storage.query(req).await,
        ("hive_kv", "get") => deps.kv.get(req).await,
        ("hive_kv", "set") => deps.kv.set(req).await,
        ("hive_kv", "delete") => deps.kv.delete(req).await,
        ("hive_blob", "read") => deps.blob.read(req).await,
        ("hive_blob", "append") => deps.blob.append(req).await,
        ("hive_events", "emit") => deps.events.emit(req).await,
        ("hive_sanitize", "sanitize") => deps.sanitizer.sanitize(req).await,
        _ => Err(AbiError::error(format!(
            "{module}.{verb}: no such host function"
        ))),
    }
}

#[derive(serde::Serialize)]
struct Envelope<'a> {
    trust: Level,
    data: &'a serde_json::value::RawValue,
}

/// The one wrapper every capability verb goes through, where three invariants
/// are enforced once rather than remembered thirteen times.
///
/// **Cancellation (invariant 7).** This is a future. The call's deadline drops
/// it, and a data layer that would have run forever simply stops being
/// polled; nothing here needs a second mechanism.
///
/// **Identity (invariants 1 and 2).** The caller comes from the credential
/// the host resolved. A guest cannot claim to act for someone else because it
/// is never asked.
///
/// **Trust (invariant 12).** The request carries the invocation's current
/// taint, so a write lands with the provenance of everything the guest read
/// before it. The response's trust is folded back in monotonically. The guest
/// participates in neither direction. Sanitize is the one verb allowed to
/// move taint back up, and only because reaching it required a granted
/// capability and an audit row (D22.3).
async fn capability_call(
    mut caller: Caller<'_, State>,
    module: &'static str,
    verb: &'static str,
    ptr: i32,
    len: i32,
) -> i64 {
    let op = format!("{}.{verb}", module.trim_start_matches("hive_"));
    let raises = module == "hive_sanitize";

    let Some(max_input) = state(&mut caller).map(|st| st.max_input) else {
        return pack_result(Status::Error, Level::Untrusted, 0);
    };
    let body = match read_guest(&mut caller, ptr, len, max_input) {
        Ok(b) => b,
        Err(status) => {
            return fail(
                &mut caller,
                status,
                "request body is out of range or too large",
                &op,
            );
        }
    };
    let (deps, req) = {
        let Some(st) = state(&mut caller) else {
            return pack_result(Status::Error, Level::Untrusted, 0);
        };
        (
            st.deps.clone(),
            Request {
                caller: st.caller,
                app: st.app.clone(),
                body,
                trust: st.taint,
                tainted_by: st.tainted_by.clone(),
            },
        )
    };

    let resp = match dispatch(&deps, module, verb, req).await {
        Ok(r) => r,
        Err(e) => return fail(&mut caller, e.status(), &e.message, &op),
    };

    let Some(st) = state(&mut caller) else {
        return pack_result(Status::Error, Level::Untrusted, 0);
    };
    let level = if raises {
        // The sanitizer succeeded, so the invocation starts clean. The only
        // non-monotonic assignment to taint in the codebase, reachable only
        // by an app whose manifest declared `sanitize` and whose Sanitizer
        // authorised and audited the call.
        st.taint = resp.trust;
        st.tainted_by.clear();
        resp.trust
    } else {
        st.weaken(resp.trust, &op);
        // What the guest is told matches what the host recorded.
        st.taint
    };

    let Ok(raw) = serde_json::from_slice::<&serde_json::value::RawValue>(&resp.data) else {
        return fail(
            &mut caller,
            Status::Error,
            "host could not encode the response envelope",
            &op,
        );
    };
    let Ok(out) = serde_json::to_vec(&Envelope {
        trust: level,
        data: raw,
    }) else {
        return fail(
            &mut caller,
            Status::Error,
            "host could not encode the response envelope",
            &op,
        );
    };
    let Some(st) = state(&mut caller) else {
        return pack_result(Status::Error, Level::Untrusted, 0);
    };
    if out.len() > st.max_output {
        return fail(
            &mut caller,
            Status::Error,
            "response exceeds the ABI size limit",
            &op,
        );
    }
    let n = out.len();
    st.result = out;
    pack_result(Status::Ok, level, n)
}

/// A failure taints the invocation, and this is not over-caution. The message
/// reaches the guest through the same result slot as data, and the host does
/// not control what a data layer puts in an error string. Failures are rare
/// and the cost is one cold instantiation.
fn fail(caller: &mut Caller<'_, State>, status: Status, msg: &str, op: &str) -> i64 {
    if let Some(st) = state(caller) {
        st.result = msg.as_bytes().to_vec();
        st.weaken(Level::Untrusted, &format!("{op} failed"));
    }
    pack_result(status, Level::Untrusted, msg.len())
}
