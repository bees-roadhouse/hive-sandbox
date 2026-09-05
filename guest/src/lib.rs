//! The SDK a WASM app links against to talk to hive-sandbox.
//!
//! The whole ABI is here. An app writes one exported function per manifest
//! function and wraps its body in [`handle`]:
//!
//! ```ignore
//! #[unsafe(no_mangle)]
//! pub extern "C" fn greet() -> i32 {
//!     hive_guest::handle(|input| Ok(br#"{"ok":true}"#.to_vec()))
//! }
//! ```
//!
//! Build as a WASI preview1 reactor: a `cdylib` for `wasm32-wasip1`, which
//! exports `_initialize` rather than `_start`. Never wasip2, never the
//! component model: the host rejects those imports at link time rather than
//! failing mysteriously later.
//!
//! This crate is the one place in a guest that needs `unsafe`: every import is
//! an `extern "C"` the host provides, and calling one is unsafe by definition.
//! The host crates forbid `unsafe` entirely; a guest is not the host.

use std::fmt;

// ---------------------------------------------------------------------------
// hive_abi: the call protocol.
//
// Nothing crosses the wasm signature. Every transfer is "ask for the size,
// allocate it yourself, ask the host to copy into it", so the host never calls
// back into a guest allocator and an app may use whatever allocator its
// toolchain ships.
// ---------------------------------------------------------------------------

#[link(wasm_import_module = "hive_abi")]
unsafe extern "C" {
    fn abi_version() -> i32;
    fn input_size() -> i32;
    fn input_read(ptr: *mut u8, size: i32) -> i32;
    fn input_trust() -> i32;
    fn output_write(ptr: *const u8, size: i32) -> i32;
    fn error_write(ptr: *const u8, size: i32) -> i32;
    fn result_read(ptr: *mut u8, size: i32) -> i32;
}

#[link(wasm_import_module = "hive_log")]
unsafe extern "C" {
    #[link_name = "log"]
    fn host_log(level: i32, ptr: *const u8, size: i32);
}

#[link(wasm_import_module = "hive_storage")]
unsafe extern "C" {
    #[link_name = "insert"]
    fn raw_storage_insert(ptr: *const u8, size: i32) -> u64;
    #[link_name = "get"]
    fn raw_storage_get(ptr: *const u8, size: i32) -> u64;
    #[link_name = "update"]
    fn raw_storage_update(ptr: *const u8, size: i32) -> u64;
    #[link_name = "delete"]
    fn raw_storage_delete(ptr: *const u8, size: i32) -> u64;
    #[link_name = "query"]
    fn raw_storage_query(ptr: *const u8, size: i32) -> u64;
}

#[link(wasm_import_module = "hive_kv")]
unsafe extern "C" {
    #[link_name = "get"]
    fn raw_kv_get(ptr: *const u8, size: i32) -> u64;
    #[link_name = "set"]
    fn raw_kv_set(ptr: *const u8, size: i32) -> u64;
    #[link_name = "delete"]
    fn raw_kv_delete(ptr: *const u8, size: i32) -> u64;
}

#[link(wasm_import_module = "hive_blob")]
unsafe extern "C" {
    #[link_name = "read"]
    fn raw_blob_read(ptr: *const u8, size: i32) -> u64;
    #[link_name = "append"]
    fn raw_blob_append(ptr: *const u8, size: i32) -> u64;
}

#[link(wasm_import_module = "hive_events")]
unsafe extern "C" {
    #[link_name = "emit"]
    fn raw_events_emit(ptr: *const u8, size: i32) -> u64;
}

#[link(wasm_import_module = "hive_sanitize")]
unsafe extern "C" {
    #[link_name = "sanitize"]
    fn raw_sanitize(ptr: *const u8, size: i32) -> u64;
}

/// The reactor entrypoint the host calls once after instantiation.
///
/// rustc links a `wasm32-wasip1` cdylib with `--no-entry` and no reactor crt,
/// so nothing exports `_initialize` unless the guest does; a module without it
/// is refused at install as a command rather than a reactor. Every guest that
/// links this SDK gets one. There is nothing to run in it: Rust's std on wasi
/// initialises lazily, and a guest holds no state to set up (invariant 5).
#[unsafe(no_mangle)]
pub extern "C" fn _initialize() {}

/// The host's ABI revision. Check it in an init if an app cares.
pub fn abi_ver() -> i32 {
    unsafe { abi_version() }
}

/// The JSON the host passed to this call.
pub fn input() -> Vec<u8> {
    let n = unsafe { input_size() };
    if n <= 0 {
        return Vec::new();
    }
    let mut buf = vec![0u8; n as usize];
    let got = unsafe { input_read(buf.as_mut_ptr(), n) };
    if got < 0 || got > n {
        return Vec::new();
    }
    buf.truncate(got as usize);
    buf
}

/// Whether this invocation's input is trusted.
///
/// It is for a guest that wants to refuse: an app about to put text into
/// instruction position should look first. It is NOT how trust is enforced.
/// The host tracks taint whatever the guest does, so ignoring this cannot
/// launder anything ... it only means the app made a decision blind.
pub fn input_trust_level() -> Trust {
    if unsafe { input_trust() } == Trust::Untrusted as i32 {
        Trust::Untrusted
    } else {
        Trust::Trusted
    }
}

/// Sets this call's JSON result and reports whether the host took it. The last
/// successful call wins.
///
/// Check the status. A result over the host's size limit is refused, and a
/// guest that returns success anyway turns an oversized response into a silent
/// empty one. [`handle`] does this for you.
pub fn output(b: &[u8]) -> Status {
    if b.is_empty() {
        return Status::Ok;
    }
    Status::from(unsafe { output_write(b.as_ptr(), b.len() as i32) })
}

/// Records an error message for this call. [`handle`] does this for you.
pub fn fail(msg: &str) {
    if msg.is_empty() {
        return;
    }
    unsafe { error_write(msg.as_ptr(), msg.len() as i32) };
}

/// The body of every exported guest function: read the input, run the app's
/// code, hand back either output or an error, return the status the host
/// expects.
///
/// These lines are the ones every AI-written guest will copy, so they check
/// everything they can. In particular `output`'s status is not discarded:
/// dropping it turned a result over the host's size limit into a successful
/// empty response, which is the worst shape a failure can take.
pub fn handle(f: impl FnOnce(Vec<u8>) -> Result<Vec<u8>, String>) -> i32 {
    match f(input()) {
        Err(e) => {
            fail(&e);
            1
        }
        Ok(out) => {
            let s = output(&out);
            if s != Status::Ok {
                fail(&format!("host refused the result: {s}"));
                return 1;
            }
            0
        }
    }
}

// ---------------------------------------------------------------------------
// hive_log
// ---------------------------------------------------------------------------

/// Log levels match the daemon's, so a guest's lines sort with the daemon's.
pub const LEVEL_DEBUG: i32 = -4;
pub const LEVEL_INFO: i32 = 0;
pub const LEVEL_WARN: i32 = 4;
pub const LEVEL_ERROR: i32 = 8;

/// Writes one line into the daemon log, attributed to this app by the host. An
/// app cannot log as another app because it never gets to name itself.
pub fn log(level: i32, msg: &str) {
    if msg.is_empty() {
        return;
    }
    unsafe { host_log(level, msg.as_ptr(), msg.len() as i32) };
}

// ---------------------------------------------------------------------------
// Capability domains. Every verb has the same shape: JSON in, Status plus JSON
// out. Importing one of these without the matching manifest capability makes
// the app fail to load, so an app links only what it declared ... the linker
// drops the import for every verb an app does not call.
// ---------------------------------------------------------------------------

/// Where content came from.
///
/// A guest cannot set it, cannot clear it, and cannot avoid receiving it: every
/// capability response carries one, and the host tracks the invocation's taint
/// independently of anything the guest does with it.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
#[repr(i32)]
pub enum Trust {
    Trusted = 0,
    Untrusted = 1,
}

impl Trust {
    pub fn as_str(self) -> &'static str {
        match self {
            Trust::Trusted => "trusted",
            Trust::Untrusted => "untrusted",
        }
    }
}

impl fmt::Display for Trust {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

/// What a capability call returns.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Status {
    Ok,
    Error,
    Denied,
    NotFound,
    Invalid,
    Unimplemented,
    Canceled,
    Unknown(i32),
}

impl From<i32> for Status {
    fn from(v: i32) -> Status {
        match v {
            0 => Status::Ok,
            1 => Status::Error,
            2 => Status::Denied,
            3 => Status::NotFound,
            4 => Status::Invalid,
            5 => Status::Unimplemented,
            6 => Status::Canceled,
            other => Status::Unknown(other),
        }
    }
}

impl fmt::Display for Status {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(match self {
            Status::Ok => "ok",
            Status::Error => "error",
            Status::Denied => "denied",
            Status::NotFound => "not_found",
            Status::Invalid => "invalid",
            Status::Unimplemented => "unimplemented",
            Status::Canceled => "canceled",
            Status::Unknown(_) => "status",
        })
    }
}

/// What a capability call returns. Trust and data arrive together and there is
/// no way to obtain one without the other, which is the point (D22.1).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Response {
    pub trust: Trust,
    pub data: Vec<u8>,
}

/// A failed capability call. `status` carries the reason; `message` is the
/// host's text.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct HostError {
    pub op: &'static str,
    pub status: Status,
    pub message: String,
}

impl HostError {
    /// Whether the call failed because the caller may not do it. Absence of a
    /// grant lands here and is deliberately indistinguishable from an explicit
    /// deny.
    pub fn denied(&self) -> bool {
        self.status == Status::Denied
    }
}

impl fmt::Display for HostError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{}: {}: {}", self.op, self.status, self.message)
    }
}

impl std::error::Error for HostError {}

impl From<HostError> for String {
    fn from(e: HostError) -> String {
        e.to_string()
    }
}

macro_rules! verb {
    ($(#[$doc:meta])* $name:ident, $import:ident, $op:literal) => {
        $(#[$doc])*
        pub fn $name(req: &[u8]) -> Result<Response, HostError> {
            let req = or_empty(req);
            let packed = unsafe { $import(req.as_ptr(), req.len() as i32) };
            finish($op, packed)
        }
    };
}

// Storage verbs. One call is one transaction. The host resolves who is asking
// from the credential, so a request body carries data and never identity.
//
// # "blob" is reserved in a document body
//
// The host maintains blob references for you: it walks each document it is
// handed and writes, moves or releases them on the same transaction as the
// write. It recognises a descriptor by the key alone, at any depth and inside
// arrays: `{"cover": {"blob": "<64 hex>", "size": 20481, "mime": "image/jpeg"}}`.
// A "blob" key whose value is not a 64-character hex digest FAILS THE WRITE,
// naming the JSON path. Use any other key for a checksum you are only
// recording. A document may only name blobs its own principal already holds a
// reference to; naming somebody else's returns the same not-found as naming a
// digest that was never stored.
verb!(storage_insert, raw_storage_insert, "storage.insert");
verb!(storage_get, raw_storage_get, "storage.get");
verb!(storage_update, raw_storage_update, "storage.update");
verb!(storage_delete, raw_storage_delete, "storage.delete");
verb!(storage_query, raw_storage_query, "storage.query");
// KV is a per-install best-effort cache: TTL'd, flushable, never truth.
verb!(kv_get, raw_kv_get, "kv.get");
verb!(kv_set, raw_kv_set, "kv.set");
verb!(kv_delete, raw_kv_delete, "kv.delete");
// Blob is windowed access to content-addressed bytes.
verb!(blob_read, raw_blob_read, "blob.read");
verb!(blob_append, raw_blob_append, "blob.append");
// Appends to the platform event log.
verb!(events_emit, raw_events_emit, "events.emit");
verb!(
    /// The only path from untrusted to trusted, and the only thing that can
    /// clear this invocation's taint.
    ///
    /// It is not a function most apps should link. Declaring `sanitize` in a
    /// manifest requires a grant, every call writes an audit row, and the host
    /// resolves both rather than believing anything the guest says. If you are
    /// reaching for this to make a warning go away, the answer is no: the
    /// taint is telling the truth about where your data came from.
    sanitize, raw_sanitize, "sanitize");

fn or_empty(req: &[u8]) -> &[u8] {
    if req.is_empty() { b"{}" } else { req }
}

// Layout of the u64 a capability call returns:
//
//   bits  0..31  size of the response, in bytes
//   bits 32..39  trust: 0 trusted, 1 untrusted
//   bits 40..47  status
//
// The size comes back WITH the status, which is the fix for ABI v1's worst
// footgun. Asking the host how big the last result was, in a separate call,
// against a slot the next host call overwrites, failed silently the moment two
// calls got reordered.
const STATUS_SHIFT: u64 = 40;
const TRUST_SHIFT: u64 = 32;
const SIZE_MASK: u64 = (1 << 32) - 1;
const BYTE_MASK: u64 = 0xff;

/// Unpacks the header and reads the envelope straight away, because the next
/// host call overwrites the slot.
fn finish(op: &'static str, packed: u64) -> Result<Response, HostError> {
    let status = Status::from(((packed >> STATUS_SHIFT) & BYTE_MASK) as i32);
    let level = if (packed >> TRUST_SHIFT) & BYTE_MASK == Trust::Untrusted as u64 {
        Trust::Untrusted
    } else {
        Trust::Trusted
    };
    let size = (packed & SIZE_MASK) as i32;
    let body = read_result(size);
    if status != Status::Ok {
        // A failure carries the host's message as plain text, not an envelope.
        return Err(HostError {
            op,
            status,
            message: String::from_utf8_lossy(&body).into_owned(),
        });
    }
    #[derive(serde::Deserialize)]
    struct Envelope<'a> {
        #[serde(default)]
        #[allow(dead_code)]
        trust: &'a str,
        #[serde(borrow)]
        data: &'a serde_json::value::RawValue,
    }
    let env: Envelope = serde_json::from_slice(&body).map_err(|e| HostError {
        op,
        status: Status::Error,
        message: format!("malformed response envelope: {e}"),
    })?;
    // The header wins over the envelope field. Both come from the host and
    // agree, but the header cannot be reshaped by anything downstream, and
    // when two sources of a security property disagree the less malleable one
    // is the right answer.
    Ok(Response {
        trust: level,
        data: env.data.get().as_bytes().to_vec(),
    })
}

fn read_result(size: i32) -> Vec<u8> {
    if size <= 0 {
        return Vec::new();
    }
    let mut buf = vec![0u8; size as usize];
    let got = unsafe { result_read(buf.as_mut_ptr(), size) };
    if got < 0 || got > size {
        return Vec::new();
    }
    buf.truncate(got as usize);
    buf
}
