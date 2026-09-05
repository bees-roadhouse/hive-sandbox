//! The browser client: the Solid.js build under `web/dist`, embedded so a
//! daemon binary is the whole deployment, served at `/` and `/ui/` under a
//! Content-Security-Policy that keeps a rendered message inert.
//!
//! The page renders everything a person typed or an agent said as text nodes,
//! never markup. The policy below is what makes that a property rather than a
//! habit: no inline script, no inline style, no remote anything, so a message
//! body that somehow became markup would still have nowhere to send anything.
//!
//! `web/dist` is a build output that is committed, because the crate embeds it
//! at compile time and a checkout with no Node must still build the daemon. CI
//! rebuilds it from `web/` and fails on a diff, which keeps the committed bytes
//! honest.

use axum::Router;
use axum::body::Body;
use axum::extract::Path;
use axum::response::{IntoResponse, Response};
use axum::routing::get;
use http::{HeaderValue, StatusCode, header};
use rust_embed::RustEmbed;

#[derive(RustEmbed)]
#[folder = "../../web/dist"]
struct Files;

/// What the page may load: itself and nothing else.
pub const POLICY: &str = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; \
img-src 'self' data:; font-src 'self'; form-action 'none'; frame-ancestors 'none'; base-uri 'none'";

/// Exactly two patterns, so an API route can never be shadowed by a file: the
/// root, and the asset prefix. Merge it into the API router.
pub fn router<S: Clone + Send + Sync + 'static>() -> Router<S> {
    Router::new()
        .route("/", get(index))
        .route("/ui/{*path}", get(asset))
}

async fn index() -> Response {
    serve("index.html")
}

async fn asset(Path(path): Path<String>) -> Response {
    // The file server canonicalises nothing: the path is looked up verbatim in
    // the embedded set, so `..` is just a name that matches no file.
    serve(&path)
}

fn serve(name: &str) -> Response {
    let Some(file) = Files::get(name) else {
        return StatusCode::NOT_FOUND.into_response();
    };
    let mime = mime_guess::from_path(name).first_or_octet_stream();
    let mut resp = Response::builder()
        .status(StatusCode::OK)
        .header(header::CONTENT_TYPE, mime.as_ref())
        .header(
            header::CONTENT_SECURITY_POLICY,
            HeaderValue::from_static(POLICY),
        )
        .header(header::X_CONTENT_TYPE_OPTIONS, "nosniff")
        .header(header::REFERRER_POLICY, "no-referrer")
        // The page carries no data; the API does. Nothing here is worth
        // caching across a deploy, and a stale app.js against a new API is a
        // support call.
        .header(header::CACHE_CONTROL, "no-cache");
    if let Ok(v) = HeaderValue::from_str(&format!("\"{}\"", hex(&file.metadata.sha256_hash()))) {
        resp = resp.header(header::ETAG, v);
    }
    resp.body(Body::from(file.data.into_owned()))
        .unwrap_or_else(|_| StatusCode::INTERNAL_SERVER_ERROR.into_response())
}

fn hex(b: &[u8]) -> String {
    b.iter().map(|x| format!("{x:02x}")).collect()
}

/// The embedded page, for a test that wants to read it without a server.
pub fn page(name: &str) -> Option<Vec<u8>> {
    Files::get(name).map(|f| f.data.into_owned())
}
