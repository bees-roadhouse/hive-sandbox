//! What a run is: the runtime, the pinned image, the caps, the network mode.

use std::collections::BTreeMap;
use std::fmt;
use std::sync::LazyLock;
use std::time::Duration;

use regex::Regex;
use serde::{Deserialize, Serialize};

/// Which agent CLI a run drives. One image per runtime, three entrypoints over
/// one base (D12.6).
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash, PartialOrd, Ord, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Runtime {
    Claude,
    Codex,
    OpenCode,
}

impl Runtime {
    /// Every supported runtime, in the order the image build uses.
    pub const ALL: [Runtime; 3] = [Runtime::Claude, Runtime::Codex, Runtime::OpenCode];

    pub fn as_str(self) -> &'static str {
        match self {
            Runtime::Claude => "claude",
            Runtime::Codex => "codex",
            Runtime::OpenCode => "opencode",
        }
    }

    pub fn parse(s: &str) -> Option<Runtime> {
        Runtime::ALL.into_iter().find(|r| r.as_str() == s)
    }
}

impl fmt::Display for Runtime {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

/// What a run may reach. The default is [`NetworkMode::None`], so a spec that
/// forgets to say gets the narrowest option rather than the widest.
///
/// A harness is the maximal trifecta case (D10, D12.10): private data through
/// MCP, untrusted content from files and the web, and egress. Widening this is a
/// declared act.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Hash, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum NetworkMode {
    /// No interfaces at all.
    #[default]
    None,
    /// `None` plus a bind-mounted unix socket for the daemon's own API, so a run
    /// reaches MCP without reaching an IP network.
    ///
    /// A socket rather than a bridge because a Podman `--internal` network does
    /// not route to the host: an internal network gets an on-link route and no
    /// gateway, and `--add-host=host-gateway` resolves to an address with no
    /// route to it. Verified on rootless Podman 6.0.2.
    Daemon,
    /// A per-run internal Podman network shared with an allowlisting egress
    /// proxy, with the CLI pointed at it.
    ///
    /// The harness gets no route out. The proxy sits on the internal network
    /// AND on a normal one, so it is the only thing in the run that can reach
    /// anything, and it reaches only what `egress_allow` lists. Requires a
    /// non-empty allowlist: a spec that asks for proxied egress without one is
    /// an error rather than a silent fall back to open egress.
    Proxied,
}

impl NetworkMode {
    /// The column value, verbatim: the schema's CHECK names these three words.
    pub fn as_str(self) -> &'static str {
        match self {
            NetworkMode::None => "none",
            NetworkMode::Daemon => "daemon",
            NetworkMode::Proxied => "proxied",
        }
    }
}

impl fmt::Display for NetworkMode {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

/// The resource caps a run gets. Every field is required: a harness run is the
/// most expensive thing the platform does (D12.8) and an uncapped one is a feral
/// loop waiting to happen.
#[derive(Clone, Copy, Debug, Default, PartialEq, Serialize, Deserialize)]
pub struct Limits {
    pub memory_bytes: i64,
    pub cpus: f64,
    pub pids_limit: i32,
}

impl Limits {
    /// Deliberately modest. An agent that needs more should say so on the run
    /// rather than have everything sized for the worst case.
    pub fn default_limits() -> Limits {
        Limits {
            memory_bytes: 2 << 30, // 2 GiB
            cpus: 2.0,
            pids_limit: 512,
        }
    }

    fn validate(&self) -> Result<(), SpecError> {
        if self.memory_bytes <= 0 {
            return Err(SpecError("limits: memory_bytes must be positive".into()));
        }
        if self.cpus <= 0.0 {
            return Err(SpecError("limits: cpus must be positive".into()));
        }
        if self.pids_limit <= 0 {
            return Err(SpecError("limits: pids_limit must be positive".into()));
        }
        Ok(())
    }
}

/// A spec the supervisor refuses to run. The message says which field.
#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
#[error("spec: {0}")]
pub struct SpecError(pub String);

/// Matches what scripts/harness-build.sh tags.
pub const DEFAULT_IMAGE_REPOSITORY: &str = "localhost/hive-sandbox-harness";

/// The proxy's port. Fixed: the proxy is only ever addressed from inside the
/// run's own network, so there is nothing to avoid colliding with.
pub const PROXY_PORT: u16 = 3128;

/// Bounds a run id to what can safely name a container, a network and a label.
///
/// A run id reaches `podman run --name` and `podman network create`. Anything
/// outside this set either fails opaquely inside podman or, worse, collides
/// with another run's names after podman normalises it.
static RUN_ID: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$").expect("static regex"));

/// Describes one agent run. It is the whole input: the supervisor reads no
/// ambient configuration and the container inherits no host state.
#[derive(Clone, Debug, Default)]
pub struct RunSpec {
    /// Identifies the run and names the container, the proxy and the run's
    /// network. Caller-assigned so the caller can record it before anything is
    /// spawned. It must be unique: two live runs sharing an id share a network
    /// and a proxy, which means each one gets the union of both allowlists and
    /// either one's teardown kills the other.
    pub run_id: String,

    pub runtime: Option<Runtime>,

    /// Pins the image (D12.5), e.g. "sha256:a3201fd1...". Required: a tag is
    /// not a pin.
    pub image_digest: String,

    /// Defaults to [`DEFAULT_IMAGE_REPOSITORY`] when empty.
    pub image_repository: String,

    /// Recorded on the run alongside the digest. Informational.
    pub cli_version: String,

    /// Passed to the CLI entrypoint.
    ///
    /// A PROMPT DOES NOT GO HERE. Use `prompt_stdin`: argv is world-readable
    /// through /proc on the host, so a message put here is visible to every
    /// process on the box, and it is bounded by ARG_MAX besides. Flags belong in
    /// args; content does not.
    pub args: Vec<String>,

    /// Written to the child's stdin and the pipe is then closed.
    ///
    /// This is where untrusted content goes. A chat message is written by a
    /// person and read by an agent, and neither argv nor the environment is an
    /// acceptable channel for it: both are readable by anything that can see the
    /// process, and both have length limits a message does not respect.
    pub prompt_stdin: Vec<u8>,

    /// Injected at spawn. Credentials arrive this way, from the vault, per run
    /// (D12.7). Nothing is baked into the image. Sorted, so the same spec
    /// produces the same command.
    pub env: BTreeMap<String, String>,

    /// A host directory bind-mounted at /workspace. It is the only writable
    /// thing that outlives the run.
    pub workspace_dir: String,

    /// Resumes a prior conversation (D12.9). Empty starts fresh.
    pub session_id: String,

    pub model: String,

    pub network: NetworkMode,

    /// The host path of the daemon's API socket, bind-mounted when `network` is
    /// `Daemon`.
    pub daemon_socket: String,

    /// The run's allowlist, one `host[:port]` per entry, with `*.example.com`
    /// permitted. Required when `network` is `Proxied`, and meaningless
    /// otherwise. Scoped per run rather than per deployment because D12.10 asks
    /// for the narrowest scope that does the job.
    pub egress_allow: Vec<String>,

    /// Lets the run reach RFC1918, loopback and link-local destinations. Off by
    /// default; this is the SSRF and DNS-rebinding control.
    pub egress_allow_private: bool,

    pub limits: Limits,

    /// The wall clock a run gets. Required and enforced by the supervisor rather
    /// than trusted to the CLI.
    pub deadline: Duration,
}

impl RunSpec {
    /// The digest-pinned reference the run actually uses.
    pub fn image_ref(&self) -> String {
        let repo = if self.image_repository.is_empty() {
            DEFAULT_IMAGE_REPOSITORY
        } else {
            &self.image_repository
        };
        format!("{repo}@{}", self.image_digest)
    }

    /// Derived from the run id so an orphan is traceable back to its run
    /// without consulting the database.
    pub fn container_name(&self) -> String {
        format!("hive-sandbox-run-{}", self.run_id)
    }

    /// The run's egress proxy.
    pub fn proxy_container_name(&self) -> String {
        format!("hive-sandbox-proxy-{}", self.run_id)
    }

    /// The run's private internal network. Derived from the run id rather than
    /// configured: two runs sharing a network could reach each other's proxy,
    /// which would quietly widen both allowlists to the union.
    pub fn egress_network_name(&self) -> String {
        format!("hive-sandbox-egress-{}", self.run_id)
    }

    /// How the harness addresses its proxy, by container name over the run's
    /// internal network.
    pub fn proxy_url(&self) -> String {
        format!("http://{}:{PROXY_PORT}", self.proxy_container_name())
    }

    pub fn runtime(&self) -> Result<Runtime, SpecError> {
        self.runtime
            .ok_or_else(|| SpecError("unknown runtime \"\"".into()))
    }

    pub fn validate(&self) -> Result<(), SpecError> {
        if self.run_id.is_empty() {
            return Err(SpecError("run_id is required".into()));
        }
        if !RUN_ID.is_match(&self.run_id) {
            return Err(SpecError(format!(
                "run_id {:?} must be 1-63 characters of [A-Za-z0-9_.-] starting alphanumeric; it names a container and a network",
                self.run_id
            )));
        }
        self.runtime()?;
        if self.image_digest.is_empty() {
            return Err(SpecError(
                "image_digest is required; a run pins a digest, never a tag".into(),
            ));
        }
        if self.workspace_dir.is_empty() {
            return Err(SpecError("workspace_dir is required".into()));
        }
        if self.deadline.is_zero() {
            return Err(SpecError(
                "deadline is required and must be positive".into(),
            ));
        }
        self.limits.validate()?;
        match self.network {
            NetworkMode::None => {}
            NetworkMode::Daemon => {
                if self.daemon_socket.is_empty() {
                    return Err(SpecError("network daemon requires daemon_socket".into()));
                }
            }
            NetworkMode::Proxied => {
                // Fail closed. The alternative ... quietly running with open
                // egress because the allowlist was not configured ... is the
                // failure mode this whole design exists to prevent.
                if self.egress_allow.is_empty() {
                    return Err(SpecError(
                        "network proxied requires a non-empty egress_allow; use network none for no egress".into(),
                    ));
                }
            }
        }
        Ok(())
    }
}

/// Which pipe an event came from.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum EventStream {
    Stdout,
    Stderr,
}

impl EventStream {
    pub fn as_str(self) -> &'static str {
        match self {
            EventStream::Stdout => "stdout",
            EventStream::Stderr => "stderr",
        }
    }

    pub fn parse(s: &str) -> Option<EventStream> {
        match s {
            "stdout" => Some(EventStream::Stdout),
            "stderr" => Some(EventStream::Stderr),
            _ => None,
        }
    }
}

impl fmt::Display for EventStream {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

/// One line of the CLI's output. Agent CLIs emit newline-delimited JSON on
/// stdout under --output-format stream-json; anything that does not parse is
/// still delivered, as `text`, rather than dropped.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Event {
    /// Starts at 1 and is unique within a run.
    pub seq: i32,
    pub at: chrono::DateTime<chrono::Utc>,
    pub stream: EventStream,
    /// The stream-json "type" field, empty when the line was not JSON.
    pub r#type: String,
    /// The parsed line, `None` when the line was not JSON. Raw JSON bytes.
    pub json: Option<Vec<u8>>,
    /// The raw line, always populated.
    pub text: String,
    /// Set when the line exceeded the line cap and was cut. The supervisor
    /// keeps draining either way; a full pipe deadlocks the child.
    pub truncated: bool,
}

/// How a run ended.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum TerminalState {
    /// The CLI exited zero. It does NOT mean anything was installed (D19.4);
    /// the definition of done is a green build.
    Succeeded,
    /// The CLI exited non-zero, or the supervisor could not process its output.
    Failed,
    /// The run outlived its deadline.
    DeadlineExceeded,
    /// The caller cancelled.
    Cancelled,
    /// The supervisor cannot say whether the work happened. A run recovered
    /// from a lease reclaim lands here rather than re-firing, because agent
    /// runs spend money and are at-most-once (invariant 10, D8).
    Indeterminate,
}

impl TerminalState {
    /// The column value, verbatim.
    pub fn as_str(self) -> &'static str {
        match self {
            TerminalState::Succeeded => "succeeded",
            TerminalState::Failed => "failed",
            TerminalState::DeadlineExceeded => "deadline_exceeded",
            TerminalState::Cancelled => "cancelled",
            TerminalState::Indeterminate => "indeterminate",
        }
    }
}

impl fmt::Display for TerminalState {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

/// What a finished run reports. There is no install or promote field here on
/// purpose: a run finishes at a green build and stops.
#[derive(Clone, Debug, PartialEq)]
pub struct RunResult {
    pub run_id: String,
    pub state: TerminalState,
    /// -1 when the process was killed or never reported one.
    pub exit_code: i32,
    pub started_at: chrono::DateTime<chrono::Utc>,
    pub ended_at: chrono::DateTime<chrono::Utc>,
    /// Events delivered to the callback.
    pub event_count: i32,
    /// Scraped from the CLI's own output when it announces one, so a follow-up
    /// run can resume the conversation (D12.9).
    pub session_id: String,
    /// The last bytes of stderr, for diagnostics. The full stream is delivered
    /// as events; this is what a failure message shows.
    pub stderr_tail: String,
    /// Where the run left its output. Whatever promotes a build reads it from
    /// here, as a separate and human-authorised act.
    pub workspace_dir: String,
}

impl RunResult {
    pub fn duration(&self) -> Duration {
        (self.ended_at - self.started_at)
            .to_std()
            .unwrap_or(Duration::ZERO)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn valid() -> RunSpec {
        RunSpec {
            run_id: "run-1".into(),
            runtime: Some(Runtime::Claude),
            image_digest: "sha256:abc".into(),
            workspace_dir: "/tmp/ws".into(),
            limits: Limits::default_limits(),
            deadline: Duration::from_secs(60),
            ..Default::default()
        }
    }

    #[test]
    fn a_valid_spec_validates() {
        valid().validate().unwrap();
    }

    #[test]
    fn run_ids_are_container_names() {
        for bad in ["", "-leading", "has space", "a/b", &"x".repeat(64)] {
            let mut s = valid();
            s.run_id = bad.to_string();
            assert!(s.validate().is_err(), "{bad:?} validated");
        }
        let mut s = valid();
        s.run_id = "Chat_turn.7-a".into();
        s.validate().unwrap();
    }

    #[test]
    fn proxied_fails_closed_without_an_allowlist() {
        let mut s = valid();
        s.network = NetworkMode::Proxied;
        assert!(s.validate().is_err());
        s.egress_allow = vec!["api.example.com".into()];
        s.validate().unwrap();
    }

    #[test]
    fn daemon_needs_a_socket() {
        let mut s = valid();
        s.network = NetworkMode::Daemon;
        assert!(s.validate().is_err());
        s.daemon_socket = "/run/hive/api.sock".into();
        s.validate().unwrap();
    }

    #[test]
    fn a_tag_is_not_a_pin() {
        let mut s = valid();
        s.image_digest.clear();
        assert!(s.validate().is_err());
    }

    #[test]
    fn the_narrowest_network_is_the_default() {
        assert_eq!(NetworkMode::default(), NetworkMode::None);
    }
}
