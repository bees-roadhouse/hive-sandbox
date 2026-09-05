//! The proxied network mode's plumbing on the launcher side: the per-run
//! internal network, the proxy sidecar, and their teardown.

use std::time::{Duration, Instant};

use crate::podman::{PodmanLauncher, is_no_such_container};
use crate::spec::RunSpec;
use crate::supervisor::RunError;

/// What the proxy resolves through when nothing says otherwise.
pub const DEFAULT_EGRESS_DNS: [&str; 2] = ["1.1.1.1", "9.9.9.9"];

/// Bounds waiting for the proxy to listen. It is a small binary with no I/O to
/// do at startup; anything past this is a failure.
const PROXY_READY_TIMEOUT: Duration = Duration::from_secs(30);

/// What the proxy's own startup log prints. Reading the container's log is the
/// only readiness signal available: the proxy sits on an internal network the
/// host cannot reach, and the image is distroless so there is no shell to exec a
/// probe into.
const PROXY_LISTENING_MARKER: &str = "role=egress-proxy";

/// Whether the proxy's log says it is listening. Both spellings, because a
/// tracing subscriber renders a string field quoted (`role="egress-proxy"`)
/// unless the writer asks for Display, and a marker that depends on which the
/// daemon happened to use is a readiness check that silently times out.
fn proxy_is_listening(log: &str) -> bool {
    log.contains(PROXY_LISTENING_MARKER) || log.contains("role=\"egress-proxy\"")
}

#[derive(Debug, thiserror::Error)]
pub enum EgressLauncherError {
    #[error(
        "harness: network proxied needs PodmanLauncher.egress_image; build it with scripts/egress-build.sh"
    )]
    NoImage,
}

fn is_no_such_network(output: &str) -> bool {
    let lowered = output.to_ascii_lowercase();
    lowered.contains("no such network") || lowered.contains("network not found")
}

impl PodmanLauncher {
    /// Creates the run's private network and its proxy, and waits until the
    /// proxy is listening. Both are named after the run, so a leftover is
    /// traceable and `terminate` can find them without holding state.
    pub(crate) async fn start_egress(&self, spec: &RunSpec) -> Result<(), RunError> {
        if self.egress_image.is_empty() {
            return Err(RunError::Egress(EgressLauncherError::NoImage));
        }

        // Clear anything a crashed run left behind under this id. Run ids are
        // unique, so a pre-existing network or proxy here is a leftover rather
        // than a peer, and reusing one silently would let a run inherit
        // whatever allowlist the previous one was started with.
        let _ = self.stop_egress(spec).await;

        let network = spec.egress_network_name();
        // --internal is what removes the default route. Without it the harness
        // could reach the internet directly and the proxy would be decoration.
        let out = self
            .command(&[
                "network".into(),
                "create".into(),
                "--internal".into(),
                network.clone(),
            ])
            .output()
            .await
            .map_err(|e| RunError::Launcher(format!("create network {network}: {e}")))?;
        if !out.status.success() {
            return Err(RunError::Launcher(format!(
                "create network {network}: {}: {}",
                out.status,
                String::from_utf8_lossy(&out.stderr).trim()
            )));
        }

        let s = |v: &str| v.to_string();
        let mut args: Vec<String> = vec![
            s("run"),
            s("--detach"),
            s("--rm"),
            s("--name"),
            spec.proxy_container_name(),
            // On the run's internal network so the harness can reach it by
            // name, AND on a normal one so it can reach the hosts it allows.
            // The proxy is the only thing in the run with a route out, which is
            // the whole shape.
            s("--network"),
            network.clone(),
            s("--network"),
            self.egress_uplink().to_string(),
            s("--read-only"),
            s("--cap-drop"),
            s("ALL"),
            s("--security-opt"),
            s("no-new-privileges"),
            s("--memory"),
            s("128m"),
            s("--cpus"),
            s("0.5"),
            s("--pids-limit"),
            s("64"),
            s("--label"),
            format!("org.beesroadhouse.hive-sandbox.run-id={}", spec.run_id),
            s("--label"),
            s("org.beesroadhouse.hive-sandbox.component=egress-proxy"),
            s("--env"),
            format!("HIVE_SANDBOX_RUN_ID={}", spec.run_id),
            // One variable rather than N arguments, and the proxy parses it.
            s("--env"),
            format!("HIVE_SANDBOX_EGRESS_ALLOW={}", spec.egress_allow.join(",")),
            self.egress_image.clone(),
        ];
        // Passed to the proxy, not to podman: --dns would only reconfigure
        // resolv.conf, which aardvark already owns.
        for server in self.egress_dns() {
            args.push(s("--egress-dns"));
            args.push(server);
        }
        if spec.egress_allow_private {
            // A flag rather than only the variable, because this one widens the
            // SSRF guard and should be visible in `podman inspect`.
            args.push(s("--egress-allow-private"));
        }
        let out = self
            .command(&args)
            .output()
            .await
            .map_err(|e| RunError::Launcher(format!("start egress proxy: {e}")))?;
        if !out.status.success() {
            return Err(RunError::Launcher(format!(
                "start egress proxy: {}: {}",
                out.status,
                String::from_utf8_lossy(&out.stderr).trim()
            )));
        }
        self.wait_for_proxy(spec).await
    }

    /// Blocks until the proxy logs that it is listening.
    async fn wait_for_proxy(&self, spec: &RunSpec) -> Result<(), RunError> {
        let name = spec.proxy_container_name();
        let deadline = Instant::now() + PROXY_READY_TIMEOUT;
        while Instant::now() < deadline {
            let logs = self.command(&["logs".into(), name.clone()]).output().await;
            if let Ok(o) = &logs
                && o.status.success()
            {
                let text = String::from_utf8_lossy(&o.stdout).to_string()
                    + &String::from_utf8_lossy(&o.stderr);
                if proxy_is_listening(&text) {
                    return Ok(());
                }
            }
            // A proxy that exited is not going to start listening. Say why now
            // rather than after the full timeout.
            if !self.container_running(&name).await {
                let text = logs
                    .map(|o| {
                        String::from_utf8_lossy(&o.stdout).to_string()
                            + &String::from_utf8_lossy(&o.stderr)
                    })
                    .unwrap_or_default();
                return Err(RunError::Launcher(format!(
                    "egress proxy exited before listening: {}",
                    text.trim()
                )));
            }
            tokio::time::sleep(Duration::from_millis(100)).await;
        }
        Err(RunError::Launcher(format!(
            "egress proxy did not listen within {PROXY_READY_TIMEOUT:?}"
        )))
    }

    async fn container_running(&self, name: &str) -> bool {
        let out = self
            .command(&[
                "inspect".into(),
                "--type".into(),
                "container".into(),
                "--format".into(),
                "{{.State.Running}}".into(),
                name.to_string(),
            ])
            .output()
            .await;
        match out {
            Ok(o) if o.status.success() => String::from_utf8_lossy(&o.stdout).trim() == "true",
            _ => false,
        }
    }

    /// Removes the proxy and the run's network. Best effort and safe to call
    /// when neither exists.
    pub(crate) async fn stop_egress(&self, spec: &RunSpec) -> Result<(), RunError> {
        let mut first_err: Option<RunError> = None;
        let proxy = spec.proxy_container_name();
        match self
            .command(&[
                "rm".into(),
                "--force".into(),
                "--time".into(),
                "5".into(),
                proxy,
            ])
            .output()
            .await
        {
            Ok(o) if o.status.success() => {}
            Ok(o) => {
                let text = String::from_utf8_lossy(&o.stderr).to_string();
                if !is_no_such_container(&text) {
                    first_err = Some(RunError::Launcher(format!(
                        "remove egress proxy: {}: {}",
                        o.status,
                        text.trim()
                    )));
                }
            }
            Err(e) => first_err = Some(RunError::Launcher(format!("remove egress proxy: {e}"))),
        }
        // The network cannot go until everything on it has, so this runs after
        // both containers are removed.
        let network = spec.egress_network_name();
        match self
            .command(&["network".into(), "rm".into(), "--force".into(), network])
            .output()
            .await
        {
            Ok(o) if o.status.success() => {}
            Ok(o) => {
                let text = String::from_utf8_lossy(&o.stderr).to_string();
                if !is_no_such_network(&text) && first_err.is_none() {
                    first_err = Some(RunError::Launcher(format!(
                        "remove egress network: {}: {}",
                        o.status,
                        text.trim()
                    )));
                }
            }
            Err(e) => {
                if first_err.is_none() {
                    first_err = Some(RunError::Launcher(format!("remove egress network: {e}")));
                }
            }
        }
        match first_err {
            Some(e) => Err(e),
            None => Ok(()),
        }
    }
}
