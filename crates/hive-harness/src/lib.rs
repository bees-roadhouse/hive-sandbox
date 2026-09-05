//! Hosted agent runs (D12).
//!
//! The daemon does not shell out to whatever `claude` happens to be on PATH. It
//! runs a digest-pinned image under rootless Podman with a per-run workspace, a
//! memory and CPU cap, a wall-clock deadline, and no network beyond what the run
//! declared. Every run records the digest it used, so "why did this build behave
//! differently last Tuesday" has an answer.
//!
//! # What this crate does not do
//!
//! It never installs, registers or promotes anything. D19.4 separates BUILDING
//! from INSTALLING: producing a build is unprivileged and the loop may do it all
//! day, while making a build live is a distinct act requiring a human principal.
//! So a run finishes at a green build and stops. There is deliberately no path
//! from [`RunResult`] to a running app, and adding one is a design change rather
//! than a feature.
//!
//! # Layering
//!
//! [`Supervisor`] owns the risky part: pipe draining, deadlines, termination and
//! the terminal state. [`Launcher`] is the seam underneath it, so that logic runs
//! against a real container in production and a cheap local process in tests
//! rather than being mocked out of the tests that matter.

mod egress;
mod pins;
mod podman;
mod record;
mod spec;
mod supervisor;

pub use egress::{DEFAULT_EGRESS_DNS, EgressLauncherError};
pub use pins::{
    DEFAULT_EGRESS_PIN_PATH, DEFAULT_EGRESS_REPOSITORY, DEFAULT_PINS_PATH, EgressPin, ImagePin,
    ImagePins, PinError,
};
pub use podman::{PodmanLauncher, podman_run_args};
pub use record::{MemoryStore, RunRecord, RunStore, StoreError, StoredRun};
pub use spec::{
    DEFAULT_IMAGE_REPOSITORY, Event, EventStream, Limits, NetworkMode, PROXY_PORT, RunResult,
    RunSpec, Runtime, SpecError, TerminalState,
};
pub use supervisor::{
    DEFAULT_DRAIN_GRACE, DEFAULT_MAX_LINE_BYTES, DEFAULT_MAX_STDERR_TAIL_BYTES,
    DEFAULT_TERMINATE_GRACE, EventFn, Launcher, RunError, Supervisor, SupervisorConfig,
};
