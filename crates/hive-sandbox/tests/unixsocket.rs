//! Ported from unixsocket_test.go and stalesocket_unix_test.go.

use std::os::unix::fs::PermissionsExt;

use axum::Router;
use axum::routing::get;
use hive_sandbox::{SOCKET_MODE, SocketError, unix_listener};
use http::StatusCode;

/// One request over the socket, by hand: a client library is not the thing
/// under test here.
async fn get_over_socket(path: &std::path::Path, target: &str) -> u16 {
    use tokio::io::{AsyncReadExt, AsyncWriteExt};
    let mut s = tokio::net::UnixStream::connect(path)
        .await
        .expect("dial socket");
    s.write_all(
        format!("GET {target} HTTP/1.1\r\nHost: unix\r\nConnection: close\r\n\r\n").as_bytes(),
    )
    .await
    .unwrap();
    let mut buf = Vec::new();
    let _ = tokio::time::timeout(std::time::Duration::from_secs(5), s.read_to_end(&mut buf)).await;
    let head = String::from_utf8_lossy(&buf);
    head.split_whitespace()
        .nth(1)
        .and_then(|c| c.parse().ok())
        .unwrap_or(0)
}

/// A harness container runs --network=none with this socket bind-mounted, so
/// this is the only way in for a run (invariant 13).
#[tokio::test]
async fn unix_listener_serves_the_handler() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("api.sock");
    let mut sock = unix_listener(&path).await.expect("unix_listener");
    let app = Router::new().route("/healthz", get(|| async { StatusCode::IM_A_TEAPOT }));
    let listener = sock.take().unwrap();
    tokio::spawn(async move {
        let _ = axum::serve(listener, app).await;
    });
    assert_eq!(get_over_socket(&path, "/healthz").await, 418);
}

/// A crashed daemon leaves the socket file behind. Binding over it fails with
/// EADDRINUSE, so a restart would need a human to delete a file, which on a
/// machine that reboots unattended means the daemon simply does not come back.
#[tokio::test]
async fn unix_listener_replaces_a_stale_socket() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("api.sock");
    seed_stale_socket(&path);
    assert!(path.exists(), "stale socket should still exist");
    let sock = unix_listener(&path)
        .await
        .expect("unix_listener over a stale socket");
    drop(sock);
}

/// The stale-socket recovery must not become a way for a second daemon to
/// steal the socket from a live one. If something is still answering, that is
/// not stale and we refuse rather than unlink it.
#[tokio::test]
async fn unix_listener_refuses_when_another_daemon_is_live() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("api.sock");
    let live = tokio::net::UnixListener::bind(&path).unwrap();
    tokio::spawn(async move {
        while let Ok((conn, _)) = live.accept().await {
            drop(conn);
        }
    });
    match unix_listener(&path).await {
        Ok(_) => panic!("unix_listener stole the socket from a live daemon; want an error"),
        Err(SocketError::InUse(_)) => {}
        Err(e) => panic!("err = {e}, want InUse"),
    }
    assert!(path.exists(), "the live socket was unlinked");
}

/// The socket is the harness's only route to the API, and a run's container
/// user is not always the daemon's. 0600 would lock it out with a permission
/// error that reads like a bug in the run.
#[tokio::test]
async fn unix_listener_is_group_accessible() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("api.sock");
    let _sock = unix_listener(&path).await.expect("unix_listener");
    let mode = std::fs::metadata(&path).unwrap().permissions().mode() & 0o777;
    assert_eq!(mode, SOCKET_MODE, "socket mode = {mode:#o}");
}

/// Dropping the guard must take the file with it, or the next start finds a
/// stale socket that only the recovery path above saves it from.
#[tokio::test]
async fn unix_listener_unlinks_on_close() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("api.sock");
    let sock = unix_listener(&path).await.expect("unix_listener");
    drop(sock);
    assert!(!path.exists(), "socket still present after close");
}

/// Leaves a socket file with nothing behind it: bound, never listened on,
/// closed. That is what a SIGKILL leaves behind, minus the one moving part the
/// earlier arrangement had (a real listener that was closed, which one CI
/// runner once found still answering). Nothing has ever listened here, so a
/// probe that connects to this file is a probe that is wrong.
fn seed_stale_socket(path: &std::path::Path) {
    let sock =
        socket2::Socket::new(socket2::Domain::UNIX, socket2::Type::STREAM, None).expect("socket");
    sock.bind(&socket2::SockAddr::unix(path).unwrap())
        .expect("bind");
    drop(sock);
}
