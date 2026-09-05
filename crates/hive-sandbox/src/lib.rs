//! The platform daemon as a library, so its pieces are testable: the unix
//! socket the harness reaches the API through, and the blob driver chosen
//! from configuration. `main.rs` is flags and wiring over this.

pub mod blobdriver;
pub mod unixsocket;

pub use blobdriver::{BlobConfig, blob_driver};
pub use unixsocket::{SOCKET_MODE, SocketError, UnixSocket, unix_listener};
