// The browser client. Plain fetch() against the daemon, a cookie for the
// credential, an EventSource for the live turn. Everything a person or an
// agent wrote is rendered with textContent, never as markup.
'use strict';

const $ = (id) => document.getElementById(id);

const state = {
  me: null,
  conversations: [],
  current: null, // { id, messages: [], open: Map(request_seq -> state), live: Map(request_seq -> text) }
  stream: null,
};

// --- transport -------------------------------------------------------------

async function api(method, path, body) {
  const init = { method, headers: {}, credentials: 'same-origin' };
  if (body !== undefined) {
    init.headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(body);
  }
  const res = await fetch(path, init);
  if (res.status === 401) {
    showLogin();
    throw new Error('unauthorized');
  }
  if (res.status === 204) return null;
  const text = await res.text();
  let data = null;
  try { data = text ? JSON.parse(text) : null; } catch { data = null; }
  if (!res.ok) {
    const err = new Error((data && data.error) || `${res.status}`);
    err.status = res.status;
    throw err;
  }
  return data;
}

function notice(text) {
  const el = $('notice');
  el.textContent = text;
  el.hidden = false;
  clearTimeout(notice.timer);
  notice.timer = setTimeout(() => { el.hidden = true; }, 4000);
}

// --- sign in ---------------------------------------------------------------

function showLogin() {
  closeStream();
  $('app').hidden = true;
  $('logout').hidden = true;
  $('identity').textContent = '';
  $('login').hidden = false;
  $('token').focus();
}

async function signIn() {
  const token = $('token').value.trim();
  const errEl = $('login-error');
  errEl.hidden = true;
  if (!token) return;
  const res = await fetch('/session', {
    method: 'POST',
    headers: { Authorization: 'Bearer ' + token },
    credentials: 'same-origin',
  });
  $('token').value = '';
  if (res.status !== 204) {
    errEl.textContent = 'That credential was not accepted.';
    errEl.hidden = false;
    return;
  }
  await boot();
}

async function signOut() {
  await fetch('/session', { method: 'DELETE', credentials: 'same-origin' });
  state.me = null;
  state.current = null;
  showLogin();
}

// --- boot --------------------------------------------------------------------

async function boot() {
  let me;
  try {
    me = await api('GET', '/whoami');
  } catch {
    return; // showLogin already ran on 401
  }
  state.me = me;
  $('identity').textContent = `${me.actor.display_name} · ${me.principal.kind} · v${me.version}`;
  $('login').hidden = true;
  $('logout').hidden = false;
  $('app').hidden = false;
  await loadConversations();
}

async function loadConversations() {
  const data = await api('GET', '/conversations?limit=100');
  state.conversations = data.conversations || [];
  renderConversations();
}

function renderConversations() {
  const ul = $('conversations');
  ul.replaceChildren();
  for (const c of state.conversations) {
    const li = document.createElement('li');
    li.dataset.id = c.id;
    if (state.current && state.current.id === c.id) li.classList.add('active');
    const title = document.createElement('span');
    title.textContent = c.title || '(untitled)';
    const meta = document.createElement('span');
    meta.className = 'meta';
    meta.textContent = `${c.runtime}${c.model ? ' · ' + c.model : ''} · ${new Date(c.updated_at).toLocaleString()}`;
    li.append(title, meta);
    li.addEventListener('click', () => openConversation(c.id));
    ul.append(li);
  }
}

async function createConversation(ev) {
  ev.preventDefault();
  const body = {
    runtime: $('runtime').value,
    model: $('model').value.trim(),
    title: $('title').value.trim(),
  };
  try {
    const data = await api('POST', '/conversations', body);
    $('model').value = '';
    $('title').value = '';
    await loadConversations();
    await openConversation(data.conversation.id);
  } catch (e) {
    notice(`Could not start a conversation: ${e.message}`);
  }
}

// --- one thread ----------------------------------------------------------------

async function openConversation(id) {
  closeStream();
  state.current = { id, messages: [], open: new Map(), live: new Map() };
  renderConversations();
  $('empty').hidden = true;
  $('messages').hidden = false;
  $('composer').hidden = false;

  try {
    const [head, msgs] = await Promise.all([
      api('GET', `/conversations/${id}`),
      api('GET', `/conversations/${id}/messages?limit=200`),
    ]);
    if (!state.current || state.current.id !== id) return;
    state.current.messages = msgs.messages || [];
    for (const t of head.open_turns || []) state.current.open.set(t.request_seq, t.state);
  } catch (e) {
    notice(`Could not open the conversation: ${e.message}`);
    return;
  }
  renderMessages();
  openStream(id);
  $('body').focus();
}

async function fetchNewMessages() {
  const cur = state.current;
  if (!cur) return;
  const last = cur.messages.length ? cur.messages[cur.messages.length - 1].seq : 0;
  const data = await api('GET', `/conversations/${cur.id}/messages?after=${last}&limit=200`);
  if (state.current !== cur) return;
  for (const m of data.messages || []) {
    if (!cur.messages.some((x) => x.seq === m.seq)) cur.messages.push(m);
  }
  cur.messages.sort((a, b) => a.seq - b.seq);
  renderMessages();
}

function renderMessages() {
  const cur = state.current;
  const box = $('messages');
  const stick = box.scrollTop + box.clientHeight >= box.scrollHeight - 40;
  box.replaceChildren();
  if (!cur) return;

  for (const m of cur.messages) {
    box.append(bubble(m.role, m.role === 'agent' ? 'agent' : m.role === 'system' ? 'system' : 'you', m.body));
  }
  // One in-flight bubble per unanswered turn, showing whatever has streamed.
  const seqs = [...cur.open.keys()].sort((a, b) => a - b);
  for (const seq of seqs) {
    const text = cur.live.get(seq) || '';
    const el = bubble('agent', 'agent', text);
    el.classList.add('live');
    if (!text) {
      const who = el.querySelector('.who');
      who.textContent = cur.open.get(seq) === 'claimed' ? 'agent is thinking' : 'waiting for an agent';
      who.classList.add('thinking');
    }
    box.append(el);
  }
  if (stick) box.scrollTop = box.scrollHeight;
}

function bubble(role, who, text) {
  const el = document.createElement('div');
  el.className = `msg ${role}`;
  const w = document.createElement('span');
  w.className = 'who';
  w.textContent = who;
  const body = document.createElement('span');
  body.textContent = text;
  el.append(w, body);
  return el;
}

async function send(ev) {
  ev.preventDefault();
  const cur = state.current;
  const body = $('body').value.trim();
  if (!cur || !body) return;
  const btn = ev.target.querySelector('button');
  btn.disabled = true;
  try {
    const data = await api('POST', `/conversations/${cur.id}/messages`, { body });
    $('body').value = '';
    if (state.current === cur) {
      cur.messages.push(data.message);
      if (data.turn) cur.open.set(data.turn.request_seq, data.turn.state);
      renderMessages();
    }
  } catch (e) {
    notice(`Could not send: ${e.message}`);
  } finally {
    btn.disabled = false;
  }
}

// --- the live stream -------------------------------------------------------------

function openStream(id) {
  const es = new EventSource(`/conversations/${id}/stream`);
  state.stream = es;
  es.addEventListener('turn', (ev) => {
    const cur = state.current;
    if (!cur || cur.id !== id) return;
    const t = JSON.parse(ev.data);
    if (t.state === 'done' || t.state === 'failed') {
      cur.open.delete(t.request_seq);
      cur.live.delete(t.request_seq);
      fetchNewMessages().catch((e) => notice(e.message));
    } else {
      cur.open.set(t.request_seq, t.state);
    }
    renderMessages();
  });
  es.addEventListener('run', (ev) => {
    const cur = state.current;
    if (!cur || cur.id !== id) return;
    const f = JSON.parse(ev.data);
    if (!f.text) return;
    cur.live.set(f.request_seq, (cur.live.get(f.request_seq) || '') + f.text);
    if (!cur.open.has(f.request_seq)) cur.open.set(f.request_seq, 'claimed');
    renderMessages();
  });
  es.onerror = () => {
    // EventSource reconnects on its own with Last-Event-ID. A 401 shows up as
    // a closed stream that never reopens; confirm with a cheap call so a
    // revoked session lands on the sign-in page rather than a silent thread.
    if (es.readyState === EventSource.CLOSED) api('GET', '/whoami').catch(() => {});
  };
}

function closeStream() {
  if (state.stream) {
    state.stream.close();
    state.stream = null;
  }
}

// --- wiring ------------------------------------------------------------------------

$('signin').addEventListener('click', signIn);
$('token').addEventListener('keydown', (e) => { if (e.key === 'Enter') signIn(); });
$('logout').addEventListener('click', signOut);
$('new-conversation').addEventListener('submit', createConversation);
$('composer').addEventListener('submit', send);
$('body').addEventListener('keydown', (e) => {
  if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) $('composer').requestSubmit();
});

boot();
