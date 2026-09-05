import { Show, batch, createSignal, onCleanup, onMount } from 'solid-js';
import { createStore, produce, reconcile } from 'solid-js/store';

import { api, signOut, unauthorized } from './api';
import { Login } from './Login';
import { Sidebar } from './Sidebar';
import { type LiveTurn, Thread } from './Thread';
import type { Conversation, Me, Message, RunFrame, Turn } from './types';

const DOT = ' · ';

interface Current {
  id: string;
  messages: Message[];
  /** request_seq -> turn state, for every unanswered turn. */
  open: Record<number, string>;
  /** request_seq -> text streamed so far. */
  live: Record<number, string>;
}

type View = 'boot' | 'login' | 'app';

export function App() {
  const [view, setView] = createSignal<View>('boot');
  const [me, setMe] = createSignal<Me | null>(null);
  const [conversations, setConversations] = createSignal<Conversation[]>([]);
  const [current, setCurrent] = createStore<{ thread: Current | null }>({ thread: null });
  const [notice, setNotice] = createSignal('');
  let noticeTimer: ReturnType<typeof setTimeout> | undefined;
  let stream: EventSource | null = null;

  const showNotice = (text: string) => {
    setNotice(text);
    clearTimeout(noticeTimer);
    noticeTimer = setTimeout(() => setNotice(''), 4000);
  };

  const closeStream = () => {
    stream?.close();
    stream = null;
  };

  const showLogin = () => {
    closeStream();
    batch(() => {
      setMe(null);
      setCurrent('thread', null);
      setView('login');
    });
  };

  const boot = async () => {
    let who: Me | null;
    try {
      who = await api<Me>('GET', '/whoami');
    } catch {
      return; // the 401 handler already showed the login
    }
    if (!who) return;
    setMe(who);
    setView('app');
    await loadConversations();
  };

  const loadConversations = async () => {
    const data = await api<{ conversations: Conversation[] }>('GET', '/conversations?limit=100');
    setConversations(data?.conversations ?? []);
  };

  const createConversation = async (runtime: string, model: string, title: string) => {
    try {
      const data = await api<{ conversation: Conversation }>('POST', '/conversations', { runtime, model, title });
      await loadConversations();
      if (data) await openConversation(data.conversation.id);
    } catch (e) {
      showNotice(`Could not start a conversation: ${(e as Error).message}`);
    }
  };

  const openConversation = async (id: string) => {
    closeStream();
    setCurrent('thread', { id, messages: [], open: {}, live: {} });
    try {
      const [head, msgs] = await Promise.all([
        api<{ conversation: Conversation; open_turns: Turn[] }>('GET', `/conversations/${id}`),
        api<{ messages: Message[] }>('GET', `/conversations/${id}/messages?limit=200`),
      ]);
      if (current.thread?.id !== id) return;
      const open: Record<number, string> = {};
      for (const t of head?.open_turns ?? []) open[t.request_seq] = t.state;
      setCurrent(
        'thread',
        produce((cur) => {
          if (!cur) return;
          cur.messages = msgs?.messages ?? [];
          cur.open = open;
        }),
      );
    } catch (e) {
      showNotice(`Could not open the conversation: ${(e as Error).message}`);
      return;
    }
    openStream(id);
  };

  const fetchNewMessages = async () => {
    const cur = current.thread;
    if (!cur) return;
    const id = cur.id;
    const last = cur.messages.length ? cur.messages[cur.messages.length - 1]!.seq : 0;
    const data = await api<{ messages: Message[] }>('GET', `/conversations/${id}/messages?after=${last}&limit=200`);
    if (current.thread?.id !== id) return;
    setCurrent(
      'thread',
      produce((c) => {
        if (!c) return;
        for (const m of data?.messages ?? []) {
          if (!c.messages.some((x) => x.seq === m.seq)) c.messages.push(m);
        }
        c.messages.sort((a, b) => a.seq - b.seq);
      }),
    );
  };

  const send = async (body: string) => {
    const cur = current.thread;
    if (!cur) return;
    const id = cur.id;
    try {
      const data = await api<{ message: Message; turn?: Turn }>('POST', `/conversations/${id}/messages`, { body });
      if (current.thread?.id !== id || !data) return;
      setCurrent(
        'thread',
        produce((c) => {
          if (!c) return;
          c.messages.push(data.message);
          if (data.turn) c.open[data.turn.request_seq] = data.turn.state;
        }),
      );
    } catch (e) {
      showNotice(`Could not send: ${(e as Error).message}`);
      throw e;
    }
  };

  const openStream = (id: string) => {
    const es = new EventSource(`/conversations/${id}/stream`);
    stream = es;
    es.addEventListener('turn', (ev) => {
      if (current.thread?.id !== id) return;
      const t = JSON.parse((ev as MessageEvent<string>).data) as Turn;
      if (t.state === 'done' || t.state === 'failed') {
        setCurrent(
          'thread',
          produce((c) => {
            if (!c) return;
            delete c.open[t.request_seq];
            delete c.live[t.request_seq];
          }),
        );
        fetchNewMessages().catch((e: Error) => showNotice(e.message));
      } else {
        setCurrent('thread', 'open', t.request_seq, t.state);
      }
    });
    es.addEventListener('run', (ev) => {
      if (current.thread?.id !== id) return;
      const f = JSON.parse((ev as MessageEvent<string>).data) as RunFrame;
      if (!f.text) return;
      setCurrent(
        'thread',
        produce((c) => {
          if (!c) return;
          c.live[f.request_seq] = (c.live[f.request_seq] ?? '') + f.text;
          if (!(f.request_seq in c.open)) c.open[f.request_seq] = 'claimed';
        }),
      );
    });
    es.onerror = () => {
      // EventSource reconnects on its own with Last-Event-ID. A 401 shows up
      // as a closed stream that never reopens; confirm with a cheap call so a
      // revoked session lands on the sign-in page rather than a silent thread.
      if (es.readyState === EventSource.CLOSED) api('GET', '/whoami').catch(() => {});
    };
  };

  const logout = async () => {
    await signOut();
    showLogin();
  };

  const live = (): LiveTurn[] => {
    const cur = current.thread;
    if (!cur) return [];
    return Object.keys(cur.open)
      .map(Number)
      .sort((a, b) => a - b)
      .map((seq) => ({ request_seq: seq, state: cur.open[seq] ?? 'pending', text: cur.live[seq] ?? '' }));
  };

  const identity = () => {
    const m = me();
    return m ? `${m.actor.display_name}${DOT}${m.principal.kind}${DOT}v${m.version}` : '';
  };

  onMount(() => {
    const onUnauthorized = () => showLogin();
    unauthorized.addEventListener('unauthorized', onUnauthorized);
    onCleanup(() => unauthorized.removeEventListener('unauthorized', onUnauthorized));
    void boot();
  });
  onCleanup(closeStream);

  return (
    <>
      <header>
        <span class="brand">hive</span>
        <span id="identity" class="identity">
          {identity()}
        </span>
        <button id="logout" class="ghost" hidden={view() !== 'app'} onClick={() => void logout()}>
          sign out
        </button>
      </header>

      <Show when={view() === 'login'}>
        <Login onSignedIn={boot} />
      </Show>

      <main id="app" class="app" hidden={view() !== 'app'}>
        <Sidebar conversations={conversations()} currentId={current.thread?.id ?? null} onOpen={(id) => void openConversation(id)} onCreate={createConversation} />
        <Thread open={current.thread !== null} messages={current.thread?.messages ?? []} live={live()} onSend={send} />
      </main>

      <p id="notice" class="notice" hidden={!notice()}>
        {notice()}
      </p>
    </>
  );
}

// reconcile is imported so a future list refresh can diff rather than
// replace; the sidebar is small enough today that replacing is fine.
void reconcile;
