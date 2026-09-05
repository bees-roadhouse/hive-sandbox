//! The daemon's API on a unix socket (invariant 13).
//!
//! A harness container runs `--network=none` with this file bind-mounted,
//! because on rootless Podman an `--internal` network has no gateway and
//! cannot reach the host at all. Measured, not assumed.

use std::os::unix::fs::PermissionsExt;
use std::path::{Path, PathBuf};
use std::time::Duration;

use tokio::net::{UnixListener, UnixStream};

/// Group-accessible on purpose. The socket is a harness run's only route to
/// the API and the run's user is not always the daemon's. 0600 locks it out
/// with a permission error that reads like a bug inside the run.
pub const SOCKET_MODE: u32 = 0o660;

/// Bounds the "is anyone still there?" dial. Short because a live daemon on a
/// local socket answers immediately; the only thing a longer wait buys is a
/// slower boot after a crash.
const PROBE_TIMEOUT: Duration = Duration::from_millis(500);

#[derive(Debug, thiserror::Error)]
pub enum SocketError {
    /// Something is still answering on the path. Distinguished from every
    /// other bind failure because it is the one case where deleting the file
    /// would be actively wrong.
    #[error("{0}: another process is listening on the socket")]
    InUse(PathBuf),
    #[error("socket directory {0}: {1}")]
    Directory(PathBuf, #[source] std::io::Error),
    #[error("socket path {0}: {1}")]
    Stat(PathBuf, #[source] std::io::Error),
    #[error("removing stale socket {0}: {1}")]
    Remove(PathBuf, #[source] std::io::Error),
    #[error("listen on {0}: {1}")]
    Listen(PathBuf, #[source] std::io::Error),
    #[error("socket permissions {0}: {1}")]
    Chmod(PathBuf, #[source] std::io::Error),
}

/// A bound listener that takes its file with it when dropped, so the next
/// start does not find a stale socket that only the recovery path saves it
/// from.
pub struct UnixSocket {
    listener: Option<UnixListener>,
    path: PathBuf,
}

impl UnixSocket {
    pub fn path(&self) -> &Path {
        &self.path
    }

    /// Hands the listener to a server. The file is still unlinked when this
    /// guard drops, so keep the guard alive for as long as the server runs.
    pub fn take(&mut self) -> Option<UnixListener> {
        self.listener.take()
    }
}

impl Drop for UnixSocket {
    fn drop(&mut self) {
        drop(self.listener.take());
        let _ = std::fs::remove_file(&self.path);
    }
}

/// Binds the daemon's API to a unix socket, recovering from a stale socket
/// left by a crash but never from a live one.
///
/// Why the probe: a SIGKILLed daemon leaves the socket file behind, and
/// binding over it fails with EADDRINUSE. Unlinking unconditionally would fix
/// that and introduce something worse: a second daemon silently stealing the
/// socket from a healthy first one, after which the harness reaches whichever
/// process bound last. So we dial first: an answer means live, and we refuse;
/// a refusal means the file is a corpse, and we remove it.
///
/// This is inherently a race (the owner could exit between the dial and the
/// bind), which is why the bind error is returned rather than retried. Losing
/// that race produces a clean failure to start, not a silent takeover.
pub async fn unix_listener(path: impl AsRef<Path>) -> Result<UnixSocket, SocketError> {
    let path = path.as_ref().to_path_buf();
    if let Some(dir) = path.parent().filter(|d| !d.as_os_str().is_empty()) {
        std::fs::create_dir_all(dir).map_err(|e| SocketError::Directory(dir.to_path_buf(), e))?;
        // 0750 on a directory we created; an existing one keeps its mode.
        let _ = std::fs::set_permissions(dir, std::fs::Permissions::from_mode(0o750));
    }
    clear_stale_socket(&path).await?;
    let listener = UnixListener::bind(&path).map_err(|e| SocketError::Listen(path.clone(), e))?;
    // Chmod after bind. bind applies the process umask, which on a default
    // 0022 turns 0660 into 0640 and takes away the group write the harness
    // needs. A umask is not a thing this daemon gets to assume.
    if let Err(e) = std::fs::set_permissions(&path, std::fs::Permissions::from_mode(SOCKET_MODE)) {
        drop(listener);
        let _ = std::fs::remove_file(&path);
        return Err(SocketError::Chmod(path, e));
    }
    Ok(UnixSocket {
        listener: Some(listener),
        path,
    })
}

/// Removes a socket file left by a dead daemon, and refuses if the owner is
/// still answering.
async fn clear_stale_socket(path: &Path) -> Result<(), SocketError> {
    match std::fs::symlink_metadata(path) {
        Ok(_) => {}
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(()),
        Err(e) => return Err(SocketError::Stat(path.to_path_buf(), e)),
    }
    if let Ok(Ok(conn)) = tokio::time::timeout(PROBE_TIMEOUT, UnixStream::connect(path)).await {
        drop(conn);
        return Err(SocketError::InUse(path.to_path_buf()));
    }
    std::fs::remove_file(path).map_err(|e| SocketError::Remove(path.to_path_buf(), e))
}
