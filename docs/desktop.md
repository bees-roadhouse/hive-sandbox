# Desktop client

The Linux desktop client for hive-sandbox (D19.1 for how a first credential
exists at all, D19.3 for who may issue more). It lives in `desktop/` as its own
Go module, next to `guest/` ... invisible to the host gate's `./...`, which is
why it has its own script and its own CI job.

The design lives in the epic: `plan/desktop.md` and D19 in the decision log.

## The rule that shapes everything

**Nothing under `desktop/internal/` imports a GUI toolkit.** All logic ...
enrollment, token storage, SSE parsing, reconnection ... lives there; only
`cmd/hive-desktop/main.go` and `webui.go` touch Wails. That is not
aesthetics: it is what lets every test run without a display, on any OS,
in CI, headless.

## Enrollment exchanges an operator token for a device token

First run pastes a server address and an operator/bootstrap token. The client
calls `POST /credentials {label:"desktop:<hostname>"}` with that token and
keeps what comes back. **The issuer token is used exactly once and never
stored** ... not on disk, not in the keyring, not held after the call returns.
What comes back is the device's own credential, attributed to its actor by the
server, revocable without touching the issuer's other sessions.

Who may issue is decided entirely by the server-side `credentials_issue_check`
trigger. The client composes no opinion about it, and neither does the HTTP
handler it talks to (invariant 11, all the way through).

## The token never touches disk

Device tokens live in the system keyring via Secret Service (gnome-keyring or
KWallet). **There is no plaintext fallback**: a machine with no keyring shows
`keyring_unavailable` and stays disconnected, because the fallback outlives
the caution that avoided it. Non-secrets (server URL, stream cursor) go to
`$XDG_CONFIG_HOME/hive-sandbox-desktop/config.json` at 0600, and a test in
that package fails if a secret-shaped key ever appears in it.

Keyring entries are keyed by **server origin**, not by user: one person, two
daemons, two entries. This is invariant 14 applied to ourselves.

## The client dedupes by event id

The daemon assigns event ids before commit, so replay overlaps by design and
the same frame can arrive twice (`docs/events-tailing.md`). The SSE client
dedupes within a bounded window, resumes by handing back whatever cursor it
holds **verbatim** in `Last-Event-ID`, treats a bare `id:` frame as a cursor
advance rather than an event, and treats `event: resync` as "forget your
position". A 401 stops retrying and raises `needs_enrollment`; everything else
backs off 1s..32s, jittered ±50%, honoring the server's `retry:` hint.

## Why there is no rate limit yet

Both endpoints the client uses require a live credential; guessing tokens
costs an indexed lookup against a 256-bit random space; deployment is a family
LAN behind no public ingress. What would change this: public exposure, or an
unauthenticated enrollment path (rejected ... D19.1 keeps first credentials
out-of-band).

## Building it

```powershell
.\scripts\build-desktop.ps1      # vet + test of the webview-free packages
```

```bash
./scripts/build-desktop.sh             # checks + windowed binary; builds in a
                                       # container when the host lacks webkit2gtk
./scripts/build-desktop.sh --headless  # checks only
./scripts/build-desktop.sh --image     # rebuild the toolchain image first
```

Wails v3 defaults to GTK4/WebKitGTK 6.0; bookworm does not ship that, so this
repo builds with `-tags gtk3` (WebKit2GTK 4.1), which v3 supports through
3.0.x. The pinned wails version lives in `desktop/go.mod`; betas move weekly
and upgrades should stay a one-file diff because only `main.go` imports them.

Phase A serves the frontend over `/api/*` JSON through the asset-server
middleware rather than Wails' generated bindings: fetch() is testable from
curl and httptest, and the binding generator is the piece of the beta most
likely to move.

## What is deliberately absent

Machine capabilities (files, notifications, clipboard, screenshots) have no
code yet ... Phase B wires the request channel, Phase C the portals behind it.
Provider credentials ("Claude profiles"), local Claude Code/Codex launcher
management, packaging (Flatpak/AppImage), tray, auto-update, TLS termination,
and Windows/macOS builds are all later phases. There is no CORS anywhere: the
desktop is native, not a browser page.
