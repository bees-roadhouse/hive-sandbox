//! Request-to-credential resolution and the ONE unauthorized response shape
//! every surface shares.
//!
//! It exists because `/events` grew the first real authenticator and the REST
//! surface needs the same three things from it: token resolution, an extractor
//! that fails closed, and a 401 that is byte-identical no matter why it fired.

use axum::extract::FromRequestParts;
use axum::response::{IntoResponse, Response};
use hive_identity::Credential;
use hive_store::{Store, StoreError};
use http::header::{AUTHORIZATION, CONTENT_TYPE, COOKIE};
use http::request::Parts;
use http::{HeaderMap, StatusCode};

/// Where a browser carries its credential.
///
/// EventSource cannot set an Authorization header, so a browser needs
/// somewhere else to put the token. A cookie is that place and a query
/// parameter is not: a bearer token in a URL lands in the reverse proxy's
/// access log, in browser history, and in a Referer header on the next
/// navigation.
pub const SESSION_COOKIE: &str = "hive_session";

/// Written by every 401 this crate produces. A constant rather than a format
/// string on purpose: one more `{}` in a message somewhere is all it takes to
/// reintroduce the oracle.
pub const UNAUTHORIZED_BODY: &str = "{\"error\":\"unauthorized\"}\n";

/// The single 401 shape shared by every surface.
pub fn unauthorized() -> Response {
    (
        StatusCode::UNAUTHORIZED,
        [(CONTENT_TYPE, "application/json")],
        UNAUTHORIZED_BODY,
    )
        .into_response()
}

/// Extracts the bearer credential from a request: Authorization header,
/// session cookie, then `access_token` query parameter, in that order.
///
/// One function rather than inline lookups, because two callers need the same
/// answer: the authenticator resolves it, and `/whoami` re-reads the row
/// behind it. The query parameter is last and is kept only for non-browser
/// callers that cannot set a header ... curl against a stream, mostly.
pub fn token(headers: &HeaderMap, query: Option<&str>) -> Option<String> {
    if let Some(h) = headers.get(AUTHORIZATION).and_then(|v| v.to_str().ok())
        && let Some(after) = h.strip_prefix("Bearer ")
    {
        let t = after.trim();
        if !t.is_empty() {
            return Some(t.to_string());
        }
    }
    if let Some(c) = cookie(headers, SESSION_COOKIE)
        && !c.is_empty()
    {
        return Some(c);
    }
    query
        .and_then(|q| query_param(q, "access_token"))
        .filter(|t| !t.is_empty())
}

/// One cookie's value out of the Cookie header(s).
pub fn cookie(headers: &HeaderMap, name: &str) -> Option<String> {
    for v in headers.get_all(COOKIE) {
        let Ok(s) = v.to_str() else { continue };
        for pair in s.split(';') {
            let pair = pair.trim();
            if let Some((k, val)) = pair.split_once('=')
                && k.trim() == name
            {
                return Some(val.trim().to_string());
            }
        }
    }
    None
}

/// One query parameter, percent-decoded.
pub fn query_param(query: &str, name: &str) -> Option<String> {
    for pair in query.split('&') {
        let (k, v) = pair.split_once('=').unwrap_or((pair, ""));
        if percent_decode(k) == name {
            return Some(percent_decode(v));
        }
    }
    None
}

fn percent_decode(s: &str) -> String {
    let bytes = s.as_bytes();
    let mut out = Vec::with_capacity(bytes.len());
    let mut i = 0;
    while i < bytes.len() {
        match bytes[i] {
            b'%' if i + 2 < bytes.len() => {
                let hex = &s[i + 1..i + 3];
                match u8::from_str_radix(hex, 16) {
                    Ok(b) => {
                        out.push(b);
                        i += 3;
                    }
                    Err(_) => {
                        out.push(b'%');
                        i += 1;
                    }
                }
            }
            b'+' => {
                out.push(b' ');
                i += 1;
            }
            b => {
                out.push(b);
                i += 1;
            }
        }
    }
    String::from_utf8_lossy(&out).into_owned()
}

/// Turns a request into the credential pair every read and write is filtered
/// by. Cheap to clone.
#[derive(Clone)]
pub struct Auth {
    store: Store,
}

impl Auth {
    pub fn new(store: Store) -> Auth {
        Auth { store }
    }

    /// Resolves a credential using `token`'s precedence. Every failure ...
    /// unknown token, revoked, expired, disabled actor, database down ... is
    /// one error, because deny must not say why.
    pub async fn resolve(
        &self,
        headers: &HeaderMap,
        query: Option<&str>,
    ) -> Result<Credential, StoreError> {
        let Some(t) = token(headers, query) else {
            return Err(StoreError::NoCredential);
        };
        let mut conn = self.store.conn().await?;
        hive_store::resolve_credential(&mut conn, &t).await
    }
}

/// The credential a request presented, resolved. Extracting it fails with THE
/// 401 for every reason, so no handler can leak why.
#[derive(Clone, Copy, Debug)]
pub struct Authed(pub Credential);

impl<S> FromRequestParts<S> for Authed
where
    Auth: axum::extract::FromRef<S>,
    S: Send + Sync,
{
    type Rejection = Response;

    async fn from_request_parts(parts: &mut Parts, state: &S) -> Result<Self, Self::Rejection> {
        let auth = <Auth as axum::extract::FromRef<S>>::from_ref(state);
        auth.resolve(&parts.headers, parts.uri.query())
            .await
            .map(Authed)
            .map_err(|_| unauthorized())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use http::HeaderValue;

    /// The 401 body is part of the anti-oracle contract, not presentation.
    #[tokio::test]
    async fn unauthorized_is_one_shape() {
        let res = unauthorized();
        assert_eq!(res.status(), StatusCode::UNAUTHORIZED);
        assert_eq!(res.headers().get(CONTENT_TYPE).unwrap(), "application/json");
        let body = axum::body::to_bytes(res.into_body(), 1024).await.unwrap();
        assert_eq!(body.as_ref(), UNAUTHORIZED_BODY.as_bytes());
        assert!(
            token(&HeaderMap::new(), None).is_none(),
            "an empty request must yield no token"
        );
    }

    #[test]
    fn token_precedence_is_header_cookie_query() {
        let mut h = HeaderMap::new();
        h.insert(AUTHORIZATION, HeaderValue::from_static("Bearer  abc "));
        h.insert(
            COOKIE,
            HeaderValue::from_static("other=1; hive_session=cookie-token"),
        );
        assert_eq!(token(&h, Some("access_token=q")).as_deref(), Some("abc"));
        h.remove(AUTHORIZATION);
        assert_eq!(
            token(&h, Some("access_token=q")).as_deref(),
            Some("cookie-token")
        );
        h.remove(COOKIE);
        assert_eq!(
            token(&h, Some("x=1&access_token=q%2Bz")).as_deref(),
            Some("q+z")
        );
        h.insert(AUTHORIZATION, HeaderValue::from_static("Basic abc"));
        assert_eq!(token(&h, None), None);
    }
}
