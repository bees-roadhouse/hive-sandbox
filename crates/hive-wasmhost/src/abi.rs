//! The guest ABI's types: what a capability verb receives and returns, who a
//! call runs as, and the closed set of capabilities a manifest may grant.
//!
//! This module has no wasmtime in it, so the data layer can implement the
//! capability traits without linking a runtime. The runtime is in the rest of
//! the crate.

use std::fmt;
use std::sync::Arc;

use async_trait::async_trait;
use hive_identity::{Credential, IncompleteCredential};
use hive_trust::Level;
use serde::{Deserialize, Serialize};
use uuid::Uuid;

/// The guest ABI revision. A guest may read it through `hive_abi.abi_version`
/// and refuse to run against a host it does not know.
///
/// Version 2 made every capability response `{trust, data}` and packed
/// (status, trust, size) into the one i64 a capability call returns (D22).
pub const ABI_VERSION: i32 = 2;

/// Who a call runs as. The credential is the platform-wide pair (invariant 2);
/// `install_id` binds the call to one install of one app in one scope, which is
/// what the data layer resolves the app's schema and grants from.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash)]
pub struct Caller {
    pub cred: Credential,
    pub install_id: Uuid,
}

/// A caller the host cannot enforce anything about.
#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
pub enum CallerError {
    #[error(transparent)]
    Credential(#[from] IncompleteCredential),
    #[error("caller: install id is empty")]
    NoInstall,
}

impl Caller {
    pub fn new(cred: Credential, install_id: Uuid) -> Self {
        Self { cred, install_id }
    }

    /// Rejects a caller the host cannot enforce anything about. Absence of
    /// scope is deny, never bypass (invariant 1), so this runs before a guest is
    /// instantiated rather than before a row is written.
    pub fn validate(&self) -> Result<(), CallerError> {
        self.cred.validate()?;
        if self.install_id.is_nil() {
            return Err(CallerError::NoInstall);
        }
        Ok(())
    }
}

/// One host module a manifest may grant. Domains, not functions: a guest that
/// may write storage may use every storage verb, and which rows it may touch is
/// the data layer's decision, not the ABI's.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Capability {
    Log,
    Storage,
    Kv,
    Blob,
    Events,
    /// The only capability that can raise trust, which is why it is a
    /// capability at all rather than something a guest asserts.
    Sanitize,
}

impl Capability {
    pub const ALL: [Capability; 6] = [
        Capability::Log,
        Capability::Storage,
        Capability::Kv,
        Capability::Blob,
        Capability::Events,
        Capability::Sanitize,
    ];

    pub fn as_str(self) -> &'static str {
        match self {
            Capability::Log => "log",
            Capability::Storage => "storage",
            Capability::Kv => "kv",
            Capability::Blob => "blob",
            Capability::Events => "events",
            Capability::Sanitize => "sanitize",
        }
    }

    /// Reads a manifest's capability name. `None` is a capability the host does
    /// not recognise; it grants nothing, which is the right reading of an
    /// unknown word in a permission list.
    pub fn parse(s: &str) -> Option<Self> {
        match s {
            "log" => Some(Capability::Log),
            "storage" => Some(Capability::Storage),
            "kv" => Some(Capability::Kv),
            "blob" => Some(Capability::Blob),
            "events" => Some(Capability::Events),
            "sanitize" => Some(Capability::Sanitize),
            _ => None,
        }
    }

    /// The host module a guest imports to reach this capability.
    pub fn module_name(self) -> &'static str {
        match self {
            Capability::Log => "hive_log",
            Capability::Storage => "hive_storage",
            Capability::Kv => "hive_kv",
            Capability::Blob => "hive_blob",
            Capability::Events => "hive_events",
            Capability::Sanitize => "hive_sanitize",
        }
    }

    pub fn for_module(name: &str) -> Option<Self> {
        Capability::ALL
            .into_iter()
            .find(|c| c.module_name() == name)
    }

    /// The position of each capability in a fingerprint. Capabilities are a
    /// closed set, so a bitmask is a faithful key and an allocation-free one.
    fn bit(self) -> u32 {
        match self {
            Capability::Log => 1 << 0,
            Capability::Storage => 1 << 1,
            Capability::Kv => 1 << 2,
            Capability::Blob => 1 << 3,
            Capability::Events => 1 << 4,
            Capability::Sanitize => 1 << 5,
        }
    }
}

impl fmt::Display for Capability {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

/// The manifest's capability section, resolved. The empty set grants nothing,
/// which is the deny-on-absence default (invariant 1) expressed in the type's
/// default rather than in a check somebody has to remember to write.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Hash)]
pub struct CapabilitySet(u32);

impl CapabilitySet {
    pub fn new(caps: &[Capability]) -> Self {
        let mut s = CapabilitySet(0);
        for c in caps {
            s.0 |= c.bit();
        }
        s
    }

    /// From a manifest's names. A name the host does not recognise contributes
    /// nothing, and that is correct rather than lossy: the link check only ever
    /// consults known capabilities, so two sets with the same known bits
    /// authorize identically.
    pub fn from_names<S: AsRef<str>>(names: &[S]) -> Self {
        let mut s = CapabilitySet(0);
        for n in names {
            if let Some(c) = Capability::parse(n.as_ref()) {
                s.0 |= c.bit();
            }
        }
        s
    }

    pub fn has(self, c: Capability) -> bool {
        self.0 & c.bit() != 0
    }

    pub fn with(mut self, c: Capability) -> Self {
        self.0 |= c.bit();
        self
    }

    /// Fingerprints the set for use as a map key. The instance key and the
    /// link-check memo are both on the hot path.
    pub fn bits(self) -> u32 {
        self.0
    }

    pub fn is_empty(self) -> bool {
        self.0 == 0
    }

    pub fn iter(self) -> impl Iterator<Item = Capability> {
        Capability::ALL.into_iter().filter(move |c| self.has(*c))
    }
}

impl fmt::Display for CapabilitySet {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        let names: Vec<&str> = self.iter().map(|c| c.as_str()).collect();
        f.write_str(&names.join(","))
    }
}

/// The status half of what a capability host function returns.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash)]
#[repr(u8)]
pub enum Status {
    Ok = 0,
    /// An unclassified host-side failure.
    Error = 1,
    /// The caller may not do this. Absence of a grant lands here, and it is
    /// deliberately indistinguishable from an explicit deny.
    Denied = 2,
    /// The target does not exist, or the caller cannot see that it exists.
    NotFound = 3,
    /// The request itself was malformed.
    Invalid = 4,
    /// The host has no implementation wired yet.
    Unimplemented = 5,
    /// The call's deadline ended mid-flight.
    Canceled = 6,
}

impl Status {
    pub fn as_str(self) -> &'static str {
        match self {
            Status::Ok => "ok",
            Status::Error => "error",
            Status::Denied => "denied",
            Status::NotFound => "not_found",
            Status::Invalid => "invalid",
            Status::Unimplemented => "unimplemented",
            Status::Canceled => "canceled",
        }
    }

    pub fn from_u8(v: u8) -> Option<Status> {
        Some(match v {
            0 => Status::Ok,
            1 => Status::Error,
            2 => Status::Denied,
            3 => Status::NotFound,
            4 => Status::Invalid,
            5 => Status::Unimplemented,
            6 => Status::Canceled,
            _ => return None,
        })
    }

    pub fn code(self) -> i32 {
        self as u8 as i32
    }
}

impl fmt::Display for Status {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

/// A failed capability call, carrying the status the guest sees.
///
/// A data layer picks the status rather than the ABI layer guessing from error
/// text. The Go tree's `Errorf(status, ...)` is `HostError::new(status, ...)`;
/// the sentinel mappings `StatusOf` performed live in the `From` impls below.
#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
#[error("{message}")]
pub struct HostError {
    pub status: Status,
    pub message: String,
}

impl HostError {
    pub fn new(status: Status, message: impl Into<String>) -> Self {
        Self {
            status,
            message: message.into(),
        }
    }

    pub fn error(message: impl Into<String>) -> Self {
        Self::new(Status::Error, message)
    }

    pub fn denied(message: impl Into<String>) -> Self {
        Self::new(Status::Denied, message)
    }

    pub fn not_found(message: impl Into<String>) -> Self {
        Self::new(Status::NotFound, message)
    }

    pub fn invalid(message: impl Into<String>) -> Self {
        Self::new(Status::Invalid, message)
    }

    /// What the stub data layer returns. It exists so the host can be exercised
    /// end to end before the store is wired.
    pub fn unimplemented(op: &str) -> Self {
        Self::new(
            Status::Unimplemented,
            format!("{op}: host function not implemented"),
        )
    }

    pub fn canceled(message: impl Into<String>) -> Self {
        Self::new(Status::Canceled, message)
    }

    pub fn status(&self) -> Status {
        self.status
    }
}

/// An incomplete credential is a denial, never a hint about which half.
impl From<IncompleteCredential> for HostError {
    fn from(e: IncompleteCredential) -> Self {
        HostError::denied(e.to_string())
    }
}

impl From<CallerError> for HostError {
    fn from(e: CallerError) -> Self {
        HostError::denied(e.to_string())
    }
}

impl From<serde_json::Error> for HostError {
    fn from(e: serde_json::Error) -> Self {
        HostError::error(e.to_string())
    }
}

/// What a capability verb receives.
///
/// Identity and trust are both filled in by the host and are not readable from
/// `body`. A guest is never asked who it is or whether its data is trustworthy,
/// which is what makes both unforgeable (invariants 1, 2 and 12).
#[derive(Clone, Debug)]
pub struct Request {
    pub caller: Caller,
    pub app: String,

    /// The guest's JSON. It is data. Nothing in it is ever treated as an
    /// assertion about identity, ownership or provenance.
    pub body: Vec<u8>,

    /// The invocation's taint at the moment of the call, and it is what any row
    /// this request writes must be recorded as. It is not a hint. A data layer
    /// that writes 'trusted' when this says Untrusted breaks invariant 9 at the
    /// last possible moment, after every other layer got it right.
    ///
    /// The reverse rule matters just as much and is easier to get wrong: on a
    /// READ, the trust of the Response is a property of the row or ref actually
    /// read, and must never be computed, defaulted, or inherited from this
    /// field. "The caller asked for trusted data, so return Trusted" reads as
    /// reasonable and is a laundering machine.
    pub trust: Level,

    /// Names the operation that first weakened this invocation's taint, or is
    /// empty if nothing did. Purely diagnostic; nothing may branch on it.
    pub tainted_by: String,
}

impl Request {
    /// The body parsed as `T`. A body that is not what the verb expects is the
    /// guest's mistake and lands as `Status::Invalid`; the decoder's own
    /// message is deliberately NOT echoed, because every error string is copied
    /// into the guest's result slot and the guest is not always the party that
    /// supplied the bytes.
    pub fn parse<T: for<'de> Deserialize<'de>>(&self, what: &str) -> Result<T, HostError> {
        serde_json::from_slice(&self.body)
            .map_err(|_| HostError::invalid(format!("{what}: body is not an object")))
    }
}

/// What a capability verb returns, and it is a struct rather than bare bytes
/// for one reason: there is no shape in which the trust marker is absent
/// (D22.1). A data layer cannot forget to say where the bytes came from,
/// because it cannot return them without saying.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Response {
    pub trust: Level,
    /// Raw JSON.
    pub data: Vec<u8>,
}

impl Response {
    /// A response for content that originated inside the platform.
    pub fn trusted(data: impl Into<Vec<u8>>) -> Self {
        Response {
            trust: Level::Trusted,
            data: data.into(),
        }
    }

    /// A response for content a browser, fetch or feed produced. It taints the
    /// rest of the invocation, whatever the guest does with it.
    pub fn untrusted(data: impl Into<Vec<u8>>) -> Self {
        Response {
            trust: Level::Untrusted,
            data: data.into(),
        }
    }

    pub fn with_trust(trust: Level, data: impl Into<Vec<u8>>) -> Self {
        Response {
            trust,
            data: data.into(),
        }
    }
}

// Every method below is a future that completes on cancellation (invariant 7,
// in the Rust tree's words): the host runs it under the call's deadline and
// drops it when that passes, and a data layer must not hold anything a dropped
// future cannot release. wasmtime's epoch interruption is what makes a guest
// parked in a host call killable at all.

/// The host-mediated data layer. Guests never see SQL. One call is one
/// transaction. The single enforcement point for owner and grants lives behind
/// this trait, not in any caller of it (invariant 1).
#[async_trait]
pub trait Storage: Send + Sync {
    async fn insert(&self, req: Request) -> Result<Response, HostError>;
    async fn get(&self, req: Request) -> Result<Response, HostError>;
    async fn update(&self, req: Request) -> Result<Response, HostError>;
    async fn delete(&self, req: Request) -> Result<Response, HostError>;
    async fn query(&self, req: Request) -> Result<Response, HostError>;
}

/// The per-install best-effort cache: TTL'd, flushable, never truth.
#[async_trait]
pub trait Kv: Send + Sync {
    async fn get(&self, req: Request) -> Result<Response, HostError>;
    async fn set(&self, req: Request) -> Result<Response, HostError>;
    async fn delete(&self, req: Request) -> Result<Response, HostError>;
}

/// Windowed access to content-addressed bytes. Reads resolve through the
/// caller's refs, never the global hash space (D17.6), and every append writes
/// a ref (invariant 8) ... both of which are this trait's job, not the ABI's.
///
/// Trust on a blob read comes from the REF the caller reached it through, never
/// from the blob row, because two refs to the same bytes may disagree (D17.1).
#[async_trait]
pub trait Blob: Send + Sync {
    async fn read(&self, req: Request) -> Result<Response, HostError>;
    async fn append(&self, req: Request) -> Result<Response, HostError>;
}

/// Appends to the events table. The table is the transport; NOTIFY is a wakeup
/// bell (invariant 4).
#[async_trait]
pub trait Events: Send + Sync {
    async fn emit(&self, req: Request) -> Result<Response, HostError>;
}

/// The one path from untrusted to trusted, and the only thing in the platform
/// that raises trust (D22.3).
///
/// It is a capability rather than an assertion because "this is safe now" is
/// precisely the claim a compromised or confused guest would make. So: the
/// manifest must declare `sanitize`, which needs a grant; the implementation
/// resolves that grant itself from the credential rather than accepting a
/// decision as a parameter (invariant 11); and it writes an audit row, because
/// an obligation that belongs to a call site is optional for anyone who reaches
/// for another one (D21).
///
/// On success the invocation's taint resets to Trusted.
#[async_trait]
pub trait Sanitizer: Send + Sync {
    async fn sanitize(&self, req: Request) -> Result<Response, HostError>;
}

/// Everything the host functions need from the rest of the daemon. The default
/// is every stub, so the runtime stands up and runs guests before the store
/// exists; a stub answers `Status::Unimplemented` and never a crash.
#[derive(Clone)]
pub struct Deps {
    pub storage: Arc<dyn Storage>,
    pub kv: Arc<dyn Kv>,
    pub blob: Arc<dyn Blob>,
    pub events: Arc<dyn Events>,
    pub sanitizer: Arc<dyn Sanitizer>,
}

impl Default for Deps {
    fn default() -> Self {
        Deps {
            storage: Arc::new(Stub),
            kv: Arc::new(Stub),
            blob: Arc::new(Stub),
            events: Arc::new(Stub),
            sanitizer: Arc::new(Stub),
        }
    }
}

impl Deps {
    pub fn with_storage(mut self, s: Arc<dyn Storage>) -> Self {
        self.storage = s;
        self
    }
    pub fn with_kv(mut self, s: Arc<dyn Kv>) -> Self {
        self.kv = s;
        self
    }
    pub fn with_blob(mut self, s: Arc<dyn Blob>) -> Self {
        self.blob = s;
        self
    }
    pub fn with_events(mut self, s: Arc<dyn Events>) -> Self {
        self.events = s;
        self
    }
    pub fn with_sanitizer(mut self, s: Arc<dyn Sanitizer>) -> Self {
        self.sanitizer = s;
        self
    }
}

/// The stub behind every unwired dependency. It names the operation in the
/// error so a guest author reading a log can tell "not built yet" from
/// "denied". The sanitizer stub in particular refuses rather than no-ops: an
/// unwired sanitizer that returned Trusted would be a trust bypass sitting in
/// the default configuration, which is the worst possible place for one.
pub struct Stub;

#[async_trait]
impl Storage for Stub {
    async fn insert(&self, _: Request) -> Result<Response, HostError> {
        Err(HostError::unimplemented("storage.insert"))
    }
    async fn get(&self, _: Request) -> Result<Response, HostError> {
        Err(HostError::unimplemented("storage.get"))
    }
    async fn update(&self, _: Request) -> Result<Response, HostError> {
        Err(HostError::unimplemented("storage.update"))
    }
    async fn delete(&self, _: Request) -> Result<Response, HostError> {
        Err(HostError::unimplemented("storage.delete"))
    }
    async fn query(&self, _: Request) -> Result<Response, HostError> {
        Err(HostError::unimplemented("storage.query"))
    }
}

#[async_trait]
impl Kv for Stub {
    async fn get(&self, _: Request) -> Result<Response, HostError> {
        Err(HostError::unimplemented("kv.get"))
    }
    async fn set(&self, _: Request) -> Result<Response, HostError> {
        Err(HostError::unimplemented("kv.set"))
    }
    async fn delete(&self, _: Request) -> Result<Response, HostError> {
        Err(HostError::unimplemented("kv.delete"))
    }
}

#[async_trait]
impl Blob for Stub {
    async fn read(&self, _: Request) -> Result<Response, HostError> {
        Err(HostError::unimplemented("blob.read"))
    }
    async fn append(&self, _: Request) -> Result<Response, HostError> {
        Err(HostError::unimplemented("blob.append"))
    }
}

#[async_trait]
impl Events for Stub {
    async fn emit(&self, _: Request) -> Result<Response, HostError> {
        Err(HostError::unimplemented("events.emit"))
    }
}

#[async_trait]
impl Sanitizer for Stub {
    async fn sanitize(&self, _: Request) -> Result<Response, HostError> {
        Err(HostError::unimplemented("sanitize"))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_empty_set_grants_nothing() {
        let s = CapabilitySet::default();
        for c in Capability::ALL {
            assert!(!s.has(c));
        }
        assert_eq!(s.to_string(), "");
    }

    #[test]
    fn unknown_names_contribute_nothing_to_the_fingerprint() {
        let a = CapabilitySet::from_names(&["storage", "egress", "log"]);
        let b = CapabilitySet::new(&[Capability::Log, Capability::Storage]);
        assert_eq!(a.bits(), b.bits());
        assert_eq!(a.to_string(), "log,storage");
    }

    #[test]
    fn statuses_round_trip_through_a_byte() {
        for s in [
            Status::Ok,
            Status::Error,
            Status::Denied,
            Status::NotFound,
            Status::Invalid,
            Status::Unimplemented,
            Status::Canceled,
        ] {
            assert_eq!(Status::from_u8(s as u8), Some(s));
        }
        assert_eq!(Status::from_u8(200), None);
    }

    #[test]
    fn an_incomplete_credential_is_a_denial() {
        let e: HostError = IncompleteCredential.into();
        assert_eq!(e.status(), Status::Denied);
    }
}
