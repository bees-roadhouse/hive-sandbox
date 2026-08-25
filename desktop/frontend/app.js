// hive desktop, Phase A. Polls /api/state and /api/events; no build step.
// The polling cadence is a viewer concern only: the session keeps its own
// cursor advancing server-side regardless of what this page renders.
"use strict";

let eventsVersion = 0;

const $ = (id) => document.getElementById(id);

async function api(path, opts) {
  const res = await fetch(path, {
    headers: { "Content-Type": "application/json" },
    ...opts,
  });
  if (res.status === 204) return {};
  const body = await res.json().catch(() => ({}));
  if (!res.ok) throw Object.assign(new Error(body.error || res.statusText), { code: body.error });
  return body;
}

const STATE_LABEL = {
  empty: "not connected",
  connecting: "connecting…",
  connected: "connected",
  reconnecting: "reconnecting…",
  needs_enrollment: "needs enrollment",
  keyring_unavailable: "no keyring service",
};

function show(el, on) { el.classList.toggle("hidden", !on); }

function render(state) {
  const pill = $("state-pill");
  pill.textContent = STATE_LABEL[state.state] || state.state;
  pill.className = "pill " + state.state;
  $("identity").textContent =
    state.identity ? `${state.identity.handle} @ ${state.server_url}` : "";
  show($("forget"), !!state.server_url);

  const setup = $("setup");
  const pane = $("events-pane");
  const notice = $("notice");

  switch (state.state) {
    case "empty":
      show(setup, true); show(pane, false); show(notice, false);
      show($("resume"), false);
      break;
    case "needs_enrollment":
      show(setup, true); show(pane, false); show(notice, false);
      show($("token-row"), true); show($("enroll"), true);
      if (state.server_url) $("server-url").value = state.server_url;
      show($("resume"), false);
      break;
    case "keyring_unavailable":
      show(setup, false); show(pane, false); show(notice, true);
      notice.innerHTML =
        "<h1>No keyring service</h1><p>This device keeps its credential in the " +
        "system keyring (gnome-keyring or KWallet). None is reachable on this " +
        "session, so the client stays disconnected rather than writing the " +
        "token to disk. Start a keyring service and reconnect.</p>";
      break;
    case "connected":
    case "reconnecting":
    case "connecting":
      show(setup, false); show(pane, true); show(notice, false);
      break;
  }
}

async function poll() {
  try {
    const state = await api("/api/state");
    render(state);
    const ev = await api("/api/events?since=" + eventsVersion);
    if (ev.events.length) {
      const tbody = $("events").tBodies[0];
      for (const e of ev.events) {
        const row = document.createElement("tr");
        const kind = document.createElement("td");
        kind.className = "kind";
        kind.textContent = e.kind || "(event)";
        const data = document.createElement("td");
        data.className = "data";
        data.textContent = typeof e.data === "string" ? e.data : JSON.stringify(e.data);
        row.append(kind, data);
        tbody.prepend(row);
        while (tbody.rows.length > 200) tbody.deleteRow(-1);
        eventsVersion = Math.max(eventsVersion, e.v);
      }
    }
  } catch (err) {
    console.warn("poll failed", err);
  }
}

$("probe").addEventListener("click", async () => {
  const out = $("probe-result");
  out.textContent = "checking…";
  try {
    const r = await api("/api/probe", {
      method: "POST",
      body: JSON.stringify({ server_url: $("server-url").value }),
    });
    out.textContent = `reachable — daemon ${r.daemon_version}`;
    show($("token-row"), true);
    show($("enroll"), true);
  } catch (err) {
    out.textContent = "unreachable: " + err.message;
  }
});

$("enroll").addEventListener("click", async () => {
  $("enroll").disabled = true;
  try {
    await api("/api/enroll", {
      method: "POST",
      body: JSON.stringify({
        server_url: $("server-url").value,
        issuer_token: $("issuer-token").value,
      }),
    });
    // One-time secret: gone from this page the moment the exchange lands.
    $("issuer-token").value = "";
  } catch (err) {
    const MESSAGES = {
      issuer_rejected: "The server rejected that token.",
      forbidden: "That credential may not issue device tokens.",
      no_keyring: "No keyring service is available; cannot store the credential.",
      timed_out: "The server did not answer in time.",
    };
    $("probe-result").textContent = MESSAGES[err.code] || err.message;
  } finally {
    $("enroll").disabled = false;
  }
});

$("resume").addEventListener("click", () => api("/api/resume", { method: "POST" }));
$("forget").addEventListener("click", () => {
  if (confirm("Remove this server and its token from this machine?")) {
    api("/api/forget", { method: "POST" });
  }
});

poll();
setInterval(poll, 1000);
