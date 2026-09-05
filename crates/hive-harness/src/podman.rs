//! The Podman launcher (D7, D12.6).
//!
//! It shells out to the podman CLI rather than linking anything. The CLI is the
//! same contract on the Linux host and on a dev box where podman talks to a
//! machine VM, and it keeps the daemon free of native dependencies. The cost is
//! parsing a little text in `terminate`.

use std::collections::HashSet;
use std::sync::LazyLock;

use async_trait::async_trait;
use tokio::process::Command;

use crate::spec::{NetworkMode, RunSpec, SpecError};
use crate::supervisor::{Launcher, RunError};

/// Container-side paths. Fixed rather than configurable: a run should not be
/// able to influence where its own workspace is mounted.
pub(crate) const CONTAINER_WORKSPACE: &str = "/workspace";
pub(crate) const CONTAINER_HOME: &str = "/home/harness";
pub(crate) const CONTAINER_DAEMON_SOCK: &str = "/run/hive-sandbox/api.sock";
const CONTAINER_UID: u32 = 1001;
const CONTAINER_GID: u32 = 1001;
const CONTAINER_TMP_SIZE: &str = "512m";
const CONTAINER_HOME_TMP_SIZE: &str = "256m";

/// Set by the launcher and refused from a spec.
///
/// The isolation depends on several of them. HOME must stay on the tmpfs so an
/// injected credential cannot outlive the run; the proxy variables are how a
/// proxied run reaches anything at all; the socket path is the daemon's API.
/// Credentials arrive through `spec.env` by design, which makes that map
/// attacker-adjacent.
static RESERVED_ENV: LazyLock<HashSet<&'static str>> = LazyLock::new(|| {
    HashSet::from([
        "HOME",
        "NPM_CONFIG_CACHE",
        "HIVE_SANDBOX_RUN_ID",
        "HIVE_SANDBOX_MODEL",
        "HIVE_SANDBOX_SESSION_ID",
        "HIVE_SANDBOX_API_SOCKET",
        "HTTP_PROXY",
        "HTTPS_PROXY",
        "http_proxy",
        "https_proxy",
        "NO_PROXY",
        "no_proxy",
    ])
});

/// Builds the argument list for one run.
///
/// Exported and pure so the isolation properties can be asserted in a unit test
/// rather than only observed by running a container. Every deny here is a
/// default that a spec cannot widen.
pub fn podman_run_args(spec: &RunSpec, extra: &[String]) -> Result<Vec<String>, SpecError> {
    spec.validate()?;
    let s = |v: &str| v.to_string();

    let mut args: Vec<String> = vec![
        s("run"),
        // The container is removed on exit; terminate is the belt to this
        // braces, for the paths where the client dies first.
        s("--rm"),
        s("--name"),
        spec.container_name(),
        s("--workdir"),
        s(CONTAINER_WORKSPACE),
        // Rootless: map the invoking host user onto the image's uid so the
        // bind-mounted workspace is writable without chowning it on the host.
        s("--userns"),
        format!("keep-id:uid={CONTAINER_UID},gid={CONTAINER_GID}"),
        s("--user"),
        format!("{CONTAINER_UID}:{CONTAINER_GID}"),
        // The agent gets a writable workspace and scratch space, nothing else.
        s("--read-only"),
        s("--cap-drop"),
        s("ALL"),
        s("--security-opt"),
        s("no-new-privileges"),
        s("--tmpfs"),
        format!("/tmp:rw,nosuid,nodev,size={CONTAINER_TMP_SIZE}"),
        // Home is a tmpfs so an injected credential, a session file or a CLI
        // cache cannot outlive the run. This is why the image installs the
        // CLIs under /opt rather than under home.
        s("--tmpfs"),
        format!("{CONTAINER_HOME}:rw,nosuid,nodev,size={CONTAINER_HOME_TMP_SIZE}"),
        s("--memory"),
        format!("{}b", spec.limits.memory_bytes),
        s("--cpus"),
        format_cpus(spec.limits.cpus),
        s("--pids-limit"),
        spec.limits.pids_limit.to_string(),
        s("--label"),
        format!("org.beesroadhouse.hive-sandbox.run-id={}", spec.run_id),
        s("--label"),
        format!("org.beesroadhouse.hive-sandbox.runtime={}", spec.runtime()?),
        s("--volume"),
        format!("{}:{CONTAINER_WORKSPACE}:rw", spec.workspace_dir),
    ];

    match spec.network {
        NetworkMode::None => {
            args.extend([s("--network"), s("none")]);
        }
        NetworkMode::Daemon => {
            // Still no IP network. The daemon's API arrives as a unix socket,
            // which is the only shape that reaches the host without opening a
            // route: a Podman internal network has no gateway to it.
            args.extend([
                s("--network"),
                s("none"),
                s("--volume"),
                format!("{}:{CONTAINER_DAEMON_SOCK}:rw", spec.daemon_socket),
                s("--env"),
                format!("HIVE_SANDBOX_API_SOCKET={CONTAINER_DAEMON_SOCK}"),
            ]);
        }
        NetworkMode::Proxied => {
            // The run's own internal network reaches the proxy and nothing
            // else; the proxy owns the allowlist. Direct egress has no route at
            // all, so a CLI that ignores the proxy variables fails rather than
            // escaping. That is the part that makes this enforcement and not a
            // suggestion.
            let proxy = spec.proxy_url();
            args.extend([
                s("--network"),
                spec.egress_network_name(),
                s("--env"),
                format!("HTTP_PROXY={proxy}"),
                s("--env"),
                format!("HTTPS_PROXY={proxy}"),
                s("--env"),
                format!("http_proxy={proxy}"),
                s("--env"),
                format!("https_proxy={proxy}"),
                s("--env"),
                s("NO_PROXY=localhost,127.0.0.1"),
                s("--env"),
                s("no_proxy=localhost,127.0.0.1"),
            ]);
            if !spec.daemon_socket.is_empty() {
                args.extend([
                    s("--volume"),
                    format!("{}:{CONTAINER_DAEMON_SOCK}:rw", spec.daemon_socket),
                    s("--env"),
                    format!("HIVE_SANDBOX_API_SOCKET={CONTAINER_DAEMON_SOCK}"),
                ]);
            }
        }
    }

    // HOME must match the tmpfs, and the CLIs need a writable cache that is
    // not the read-only rootfs.
    args.extend([
        s("--env"),
        format!("HOME={CONTAINER_HOME}"),
        s("--env"),
        s("NPM_CONFIG_CACHE=/tmp/npm-cache"),
        s("--env"),
        format!("HIVE_SANDBOX_RUN_ID={}", spec.run_id),
    ]);
    if !spec.model.is_empty() {
        args.extend([s("--env"), format!("HIVE_SANDBOX_MODEL={}", spec.model)]);
    }
    if !spec.session_id.is_empty() {
        args.extend([
            s("--env"),
            format!("HIVE_SANDBOX_SESSION_ID={}", spec.session_id),
        ]);
    }

    // A BTreeMap, so the same spec produces the same command, which is what
    // makes the recorded invocation comparable across runs.
    for (key, value) in &spec.env {
        if key.is_empty() || key.contains('=') || key.contains('\0') {
            return Err(SpecError(format!("invalid env key {key:?}")));
        }
        // Podman takes the LAST --env for a key, so a spec key colliding with
        // one the launcher sets would win. HOME is the one that matters:
        // `HOME=/workspace` aims a credential that is supposed to die with the
        // run's tmpfs at the one directory that outlives it. Refuse rather than
        // reorder, so a caller learns its variable was ignored instead of
        // silently losing it.
        if RESERVED_ENV.contains(key.as_str()) {
            return Err(SpecError(format!(
                "{key:?} is set by the launcher and cannot be overridden"
            )));
        }
        args.extend([s("--env"), format!("{key}={value}")]);
    }

    args.extend(extra.iter().cloned());
    args.push(spec.image_ref());
    args.extend(spec.args.iter().cloned());
    Ok(args)
}

/// Renders a CPU count the way `strconv.FormatFloat(f, 'f', -1, 64)` did: the
/// shortest decimal that round-trips, with no trailing zeros.
fn format_cpus(cpus: f64) -> String {
    if cpus.fract() == 0.0 {
        format!("{}", cpus as i64)
    } else {
        format!("{cpus}")
    }
}

/// Runs the harness image under rootless Podman.
#[derive(Clone, Debug, Default)]
pub struct PodmanLauncher {
    /// Defaults to "podman".
    pub binary: String,
    /// Appended to every `podman run` before the image reference. For operator
    /// escape hatches, not for anything a run controls.
    pub extra_args: Vec<String>,
    /// The digest-pinned proxy image, required for proxied runs. Build it with
    /// scripts/egress-build.sh and read the pin with [`crate::EgressPin::load`].
    pub egress_image: String,
    /// The Podman network the proxy reaches the internet over. Defaults to
    /// "podman", the default bridge.
    pub egress_uplink: String,
    /// The resolvers the proxy uses. Defaults to [`crate::DEFAULT_EGRESS_DNS`].
    ///
    /// It needs explicit ones. A container attached to an --internal network
    /// has its resolv.conf pointed at that network's aardvark-dns, and aardvark
    /// on an internal network has no upstream to forward to, so every external
    /// name returns NXDOMAIN even though the uplink interface routes fine.
    pub egress_dns: Vec<String>,
}

impl PodmanLauncher {
    pub(crate) fn binary(&self) -> &str {
        if self.binary.is_empty() {
            "podman"
        } else {
            &self.binary
        }
    }

    pub(crate) fn egress_uplink(&self) -> &str {
        if self.egress_uplink.is_empty() {
            "podman"
        } else {
            &self.egress_uplink
        }
    }

    pub(crate) fn egress_dns(&self) -> Vec<String> {
        if self.egress_dns.is_empty() {
            crate::egress::DEFAULT_EGRESS_DNS
                .iter()
                .map(|s| s.to_string())
                .collect()
        } else {
            self.egress_dns.clone()
        }
    }

    pub(crate) fn command(&self, args: &[String]) -> Command {
        let mut cmd = Command::new(self.binary());
        cmd.args(args);
        cmd
    }
}

pub(crate) fn is_no_such_container(output: &str) -> bool {
    let lowered = output.to_ascii_lowercase();
    lowered.contains("no such container") || lowered.contains("no container with name")
}

#[async_trait]
impl Launcher for PodmanLauncher {
    /// Builds the `podman run` invocation for a spec.
    async fn command(&self, spec: &RunSpec) -> Result<Command, RunError> {
        let args = podman_run_args(spec, &self.extra_args)?;
        // The network and the proxy have to exist before the harness starts,
        // and terminate tears both down on every exit path.
        if spec.network == NetworkMode::Proxied {
            self.start_egress(spec).await?;
        }
        // Spawning a subprocess from a spec is what this crate is for. The args
        // come from podman_run_args over a validated spec and are passed as a
        // vector, so there is no shell to inject into.
        Ok(self.command(&args))
    }

    /// Removes the run's container.
    ///
    /// Killing the `podman run` client does not stop the container it started,
    /// so without this a cancelled or timed-out run leaves a live agent burning
    /// tokens against a workspace nobody is watching.
    async fn terminate(&self, spec: &RunSpec) -> Result<(), RunError> {
        let name = spec.container_name();
        let out = self
            .command(&[
                "rm".into(),
                "--force".into(),
                "--time".into(),
                "5".into(),
                name.clone(),
            ])
            .output()
            .await;
        let mut first_err: Option<RunError> = None;
        match out {
            Ok(o) if o.status.success() => {}
            Ok(o) => {
                let text = String::from_utf8_lossy(&o.stderr).to_string()
                    + &String::from_utf8_lossy(&o.stdout);
                // Already gone is the common case: `--rm` usually got there first.
                if !is_no_such_container(&text) {
                    first_err = Some(RunError::Launcher(format!(
                        "podman rm {name}: {}: {}",
                        o.status,
                        text.trim()
                    )));
                }
            }
            Err(e) => first_err = Some(RunError::Launcher(format!("podman rm {name}: {e}"))),
        }

        // The proxy and the network outlive the harness container otherwise, and
        // a leaked internal network per run adds up fast. A spec that was
        // proxied on the way in still is.
        if spec.network == NetworkMode::Proxied
            && let Err(e) = self.stop_egress(spec).await
            && first_err.is_none()
        {
            first_err = Some(e);
        }
        match first_err {
            Some(e) => Err(e),
            None => Ok(()),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::spec::{Limits, Runtime};
    use std::time::Duration;

    fn spec() -> RunSpec {
        RunSpec {
            run_id: "run-args".into(),
            runtime: Some(Runtime::Claude),
            image_digest: format!("sha256:{}", "0123456789abcdef".repeat(4)),
            workspace_dir: "/tmp/ws".into(),
            limits: Limits::default_limits(),
            deadline: Duration::from_secs(30),
            ..Default::default()
        }
    }

    fn has_pair(args: &[String], flag: &str, value: &str) -> bool {
        args.windows(2).any(|w| w[0] == flag && w[1] == value)
    }

    fn values_of<'a>(args: &'a [String], flag: &str) -> Vec<&'a str> {
        args.windows(2)
            .filter(|w| w[0] == flag)
            .map(|w| w[1].as_str())
            .collect()
    }

    /// Each of these is a deny a spec cannot widen.
    #[test]
    fn isolates_by_default() {
        let args = podman_run_args(&spec(), &[]).unwrap();
        for (flag, value) in [
            ("--network", "none"),
            ("--cap-drop", "ALL"),
            ("--security-opt", "no-new-privileges"),
            ("--user", "1001:1001"),
            ("--userns", "keep-id:uid=1001,gid=1001"),
            ("--pids-limit", "512"),
            ("--memory", "2147483648b"),
            ("--cpus", "2"),
        ] {
            assert!(
                has_pair(&args, flag, value),
                "missing {flag} {value} in {args:?}"
            );
        }
        assert!(
            args.iter().any(|a| a == "--read-only"),
            "the rootfs must not be writable"
        );
        assert!(args.iter().any(|a| a == "--rm"));
        // Home is a tmpfs so an injected credential cannot outlive the run.
        assert!(
            values_of(&args, "--tmpfs")
                .iter()
                .any(|v| v.starts_with("/home/harness:")),
            "home is not a tmpfs; a run would leave state behind"
        );
        // The image is pinned by digest, and the reference is the last thing
        // before the CLI's own arguments.
        assert!(
            args.last().unwrap().contains("@sha256:"),
            "image reference is not digest-pinned"
        );
    }

    #[test]
    fn daemon_keeps_the_container_off_the_network() {
        let mut s = spec();
        s.network = NetworkMode::Daemon;
        s.daemon_socket = "/run/hive-sandbox/api.sock".into();
        let args = podman_run_args(&s, &[]).unwrap();
        // The daemon arrives as a socket, not a route.
        assert!(has_pair(&args, "--network", "none"), "{args:?}");
        assert!(
            values_of(&args, "--volume")
                .iter()
                .any(|m| m.starts_with("/run/hive-sandbox/api.sock:")),
            "daemon socket not bind-mounted"
        );
    }

    #[test]
    fn proxied_routes_through_the_runs_own_proxy_only() {
        let mut s = spec();
        s.network = NetworkMode::Proxied;
        s.egress_allow = vec!["api.anthropic.com".into()];
        let args = podman_run_args(&s, &[]).unwrap();
        // A network named after the run. Two runs sharing one would quietly
        // widen both allowlists to the union.
        assert!(
            has_pair(&args, "--network", &s.egress_network_name()),
            "{args:?}"
        );
        assert!(s.egress_network_name().contains(&s.run_id));
        let envs = values_of(&args, "--env");
        let proxy = s.proxy_url();
        // Both cases: Go's net/http reads the upper-case forms, curl and most
        // CLIs read the lower-case ones, and an agent shells out to both.
        for want in [
            format!("HTTPS_PROXY={proxy}"),
            format!("HTTP_PROXY={proxy}"),
            format!("https_proxy={proxy}"),
            format!("http_proxy={proxy}"),
        ] {
            assert!(envs.contains(&want.as_str()), "missing {want} in {envs:?}");
        }
    }

    /// Fail closed. Falling back to open egress because the allowlist was not
    /// configured is the failure this design exists to prevent.
    #[test]
    fn proxied_without_an_allowlist_is_refused() {
        let mut s = spec();
        s.network = NetworkMode::Proxied;
        assert!(podman_run_args(&s, &[]).is_err());
    }

    /// Deterministic ordering is what makes a recorded invocation comparable
    /// between two runs of the same spec.
    #[test]
    fn env_is_sorted_and_validated() {
        let mut s = spec();
        s.env.insert("ZED".into(), "3".into());
        s.env.insert("ALPHA".into(), "1".into());
        s.env.insert("MIDDLE".into(), "2".into());
        let args = podman_run_args(&s, &[]).unwrap();
        let caller: Vec<&str> = values_of(&args, "--env")
            .into_iter()
            .filter(|e| {
                e.starts_with("ALPHA=") || e.starts_with("MIDDLE=") || e.starts_with("ZED=")
            })
            .collect();
        assert_eq!(caller, ["ALPHA=1", "MIDDLE=2", "ZED=3"]);

        let mut s = spec();
        s.env.insert("BAD=KEY".into(), "x".into());
        assert!(
            podman_run_args(&s, &[]).is_err(),
            "an env key containing = would inject a second variable"
        );
    }

    /// Podman takes the last --env for a key, and spec env was appended after
    /// the launcher's own, so a spec could redirect HOME off the tmpfs and onto
    /// the one directory that outlives the run.
    #[test]
    fn spec_env_cannot_override_the_launcher_env() {
        for key in [
            "HOME", // the one that matters: aims a credential at the workspace
            "HTTP_PROXY",
            "https_proxy",
            "NO_PROXY",                // how a proxied run reaches anything
            "HIVE_SANDBOX_API_SOCKET", // the daemon's API
            "HIVE_SANDBOX_RUN_ID",
        ] {
            let mut s = spec();
            s.env.insert(key.into(), "/workspace".into());
            assert!(podman_run_args(&s, &[]).is_err(), "a spec overrode {key}");
        }
        // An ordinary variable still works ... this is a reserved list, not a ban.
        let mut s = spec();
        s.env
            .insert("ANTHROPIC_API_KEY".into(), "from-the-vault".into());
        let args = podman_run_args(&s, &[]).unwrap();
        assert!(values_of(&args, "--env").contains(&"ANTHROPIC_API_KEY=from-the-vault"));
    }

    /// The launcher's own HOME must be the last word, whatever else is in the spec.
    #[test]
    fn home_stays_on_the_tmpfs() {
        let mut s = spec();
        s.env.insert("ANTHROPIC_API_KEY".into(), "k".into());
        let args = podman_run_args(&s, &[]).unwrap();
        let last_home = values_of(&args, "--env")
            .into_iter()
            .filter(|e| e.starts_with("HOME="))
            .last();
        assert_eq!(last_home, Some("HOME=/home/harness"));
    }

    #[test]
    fn cpus_render_like_the_go_formatter() {
        assert_eq!(format_cpus(2.0), "2");
        assert_eq!(format_cpus(0.5), "0.5");
        assert_eq!(format_cpus(1.25), "1.25");
    }
}
