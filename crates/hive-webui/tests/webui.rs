//! Ported from webui_test.go.

use axum::Router;
use axum::routing::get;
use http::StatusCode;
use tokio_util::sync::CancellationToken;

async fn serve(app: Router) -> (String, CancellationToken) {
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    let cancel = CancellationToken::new();
    let c = cancel.clone();
    tokio::spawn(async move {
        let _ = axum::serve(listener, app)
            .with_graceful_shutdown(async move { c.cancelled().await })
            .await;
    });
    (format!("http://{addr}"), cancel)
}

#[tokio::test]
async fn page_and_assets_are_served_under_policy() {
    // A route the API would own must not be shadowed by the file server.
    let app =
        hive_webui::router().route("/conversations", get(|| async { StatusCode::IM_A_TEAPOT }));
    let (url, cancel) = serve(app).await;
    let client = reqwest::Client::new();
    for (path, status, content_type, contains) in [
        ("/", 200, "text/html", "<title>hive</title>"),
        ("/ui/app.js", 200, "text/javascript", "EventSource"),
        ("/ui/styles.css", 200, "text/css", "body"),
        ("/ui/nope.js", 404, "", ""),
        ("/ui/index.html/../Cargo.toml", 404, "", ""),
        ("/conversations", 418, "", ""),
    ] {
        let res = client.get(format!("{url}{path}")).send().await.unwrap();
        assert_eq!(res.status().as_u16(), status, "{path}");
        if status != 200 {
            continue;
        }
        let ct = res
            .headers()
            .get("content-type")
            .and_then(|v| v.to_str().ok())
            .unwrap_or("")
            .to_string();
        assert!(
            ct.starts_with(content_type),
            "{path}: content type {ct:?}, want {content_type}"
        );
        let csp = res
            .headers()
            .get("content-security-policy")
            .and_then(|v| v.to_str().ok())
            .unwrap_or("")
            .to_string();
        assert!(
            csp.contains("default-src 'none'") && csp.contains("script-src 'self'"),
            "{path}: policy = {csp:?}"
        );
        assert_eq!(
            res.headers()
                .get("x-content-type-options")
                .and_then(|v| v.to_str().ok()),
            Some("nosniff"),
            "{path}: no nosniff"
        );
        let body = res.text().await.unwrap();
        assert!(body.contains(contains), "{path}: body lacks {contains:?}");
    }
    cancel.cancel();
}

/// The page must not carry inline script or style, or the policy that keeps a
/// rendered message inert would have to be loosened to run the page itself.
#[test]
fn page_has_no_inline_script_or_style() {
    let page =
        String::from_utf8(hive_webui::page("index.html").expect("index.html is embedded")).unwrap();
    for forbidden in [
        "<script>",
        "<style>",
        " onclick=",
        " onload=",
        "javascript:",
        "style=\"",
    ] {
        assert!(
            !page.contains(forbidden),
            "index.html contains {forbidden:?}"
        );
    }
    // And it loads exactly the two files the daemon serves, from the prefix
    // the daemon mounts.
    assert!(page.contains("src=\"/ui/app.js\""), "{page}");
    assert!(page.contains("href=\"/ui/styles.css\""), "{page}");
}

/// The client never puts a token in a URL or in script-readable storage: it
/// exchanges it once for the cookie. Asserted against the built bundle, so a
/// change in web/ that starts remembering a credential fails here.
#[test]
fn bundle_keeps_the_credential_out_of_storage() {
    let js = String::from_utf8(hive_webui::page("app.js").expect("app.js is embedded")).unwrap();
    for forbidden in ["localStorage", "sessionStorage", "access_token="] {
        assert!(!js.contains(forbidden), "app.js contains {forbidden:?}");
    }
    assert!(
        js.contains("/session"),
        "app.js does not exchange the token for a session"
    );
}
