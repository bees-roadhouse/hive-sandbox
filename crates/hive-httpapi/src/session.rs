//! The browser's login: a bearer token exchanged once for the session cookie.

use axum::extract::State;
use axum::response::{IntoResponse, Response};
use hive_httpauth::SESSION_COOKIE;
use http::{HeaderMap, HeaderValue, StatusCode, header};

use crate::AppState;

/// Moves a bearer token into the session cookie.
///
/// A browser cannot put a token on an EventSource, and it should not keep one
/// in script-readable storage. So the web app presents the token once, over
/// the Authorization header, and from then on the cookie carries it: HttpOnly
/// so script never sees it again, SameSite=Strict so no other site can ride
/// it, and Secure unless the deployment said, once and deliberately, that it
/// serves plain HTTP (D26). The header, not the cookie: a request that already
/// carries the cookie has nothing to exchange.
pub(crate) async fn start(State(s): State<AppState>, headers: HeaderMap) -> Response {
    let token = headers
        .get(header::AUTHORIZATION)
        .and_then(|v| v.to_str().ok())
        .and_then(|h| h.strip_prefix("Bearer "))
        .map(str::trim)
        .filter(|t| !t.is_empty());
    let Some(token) = token else {
        return hive_httpauth::unauthorized();
    };
    let Ok(mut conn) = s.store().conn().await else {
        return hive_httpauth::unauthorized();
    };
    if hive_store::resolve_credential(&mut conn, token)
        .await
        .is_err()
    {
        // One 401 for every reason, as everywhere else.
        return hive_httpauth::unauthorized();
    }
    (
        StatusCode::NO_CONTENT,
        [(header::SET_COOKIE, cookie(token, false, s.plain_http))],
    )
        .into_response()
}

/// Clears the cookie. No credential needed: clearing a cookie you do not hold
/// changes nothing, and a logout that could fail for lack of authorization
/// leaves a revoked session in the browser.
pub(crate) async fn end(State(s): State<AppState>) -> Response {
    (
        StatusCode::NO_CONTENT,
        [(header::SET_COOKIE, cookie("", true, s.plain_http))],
    )
        .into_response()
}

/// Secure by default. The flag, not the request, decides: a security property
/// read off the request's scheme or a forwarded header is a property the
/// network gets to choose, and it is silent.
fn cookie(value: &str, clear: bool, plain_http: bool) -> HeaderValue {
    let mut c = format!("{SESSION_COOKIE}={value}; Path=/");
    if clear {
        c.push_str("; Max-Age=0");
    }
    c.push_str("; HttpOnly; SameSite=Strict");
    if !plain_http {
        c.push_str("; Secure");
    }
    HeaderValue::from_str(&c).expect("cookie is ascii")
}
