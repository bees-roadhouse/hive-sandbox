import { For, Show, createEffect, createSignal, on } from 'solid-js';

import type { Message } from './types';

export interface LiveTurn {
  request_seq: number;
  state: string;
  text: string;
}

/**
 * One conversation: its messages, one in-flight bubble per unanswered turn
 * showing whatever has streamed so far, and the composer.
 *
 * Everything a person or an agent wrote reaches the DOM as a text node. Solid
 * renders `{expr}` as text, never markup, and the CSP the daemon sends makes
 * that a property rather than a habit.
 */
export function Thread(props: { open: boolean; messages: Message[]; live: LiveTurn[]; onSend: (body: string) => Promise<void> }) {
  const [body, setBody] = createSignal('');
  const [sending, setSending] = createSignal(false);
  let box: HTMLDivElement | undefined;
  let textarea: HTMLTextAreaElement | undefined;

  // Stick to the bottom while the reader is already there; leave them alone
  // when they have scrolled up to read.
  createEffect(
    on(
      () => [props.messages.length, props.live.map((t) => t.text).join(' ')],
      () => {
        if (!box) return;
        const el = box;
        const stick = el.scrollTop + el.clientHeight >= el.scrollHeight - 40;
        if (stick) queueMicrotask(() => (el.scrollTop = el.scrollHeight));
      },
    ),
  );
  createEffect(() => {
    if (props.open) queueMicrotask(() => textarea?.focus());
  });

  const send = async (ev: SubmitEvent) => {
    ev.preventDefault();
    const text = body().trim();
    if (!text || sending()) return;
    setSending(true);
    try {
      await props.onSend(text);
      setBody('');
    } finally {
      setSending(false);
    }
  };

  const who = (role: string) => (role === 'agent' ? 'agent' : role === 'system' ? 'system' : 'you');
  const waiting = (state: string) => (state === 'claimed' ? 'agent is thinking' : 'waiting for an agent');

  return (
    <section class="thread">
      <div id="empty" class="hint center" hidden={props.open}>
        Pick a conversation, or start one.
      </div>
      <div id="messages" class="messages" ref={box} hidden={!props.open}>
        <For each={props.messages}>
          {(m) => (
            <div class={`msg ${m.role}`}>
              <span class="who">{who(m.role)}</span>
              <span>{m.body}</span>
            </div>
          )}
        </For>
        <For each={props.live}>
          {(t) => (
            <div class="msg agent live">
              <Show when={t.text} fallback={<span class="who thinking">{waiting(t.state)}</span>}>
                <span class="who">agent</span>
              </Show>
              <span>{t.text}</span>
            </div>
          )}
        </For>
      </div>
      <form id="composer" class="composer" hidden={!props.open} onSubmit={(e) => void send(e)}>
        <textarea
          id="body"
          ref={textarea}
          rows="3"
          placeholder="Message"
          required
          value={body()}
          onInput={(e) => setBody(e.currentTarget.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) e.currentTarget.form?.requestSubmit();
          }}
        />
        <button class="primary" type="submit" disabled={sending()}>
          Send
        </button>
      </form>
    </section>
  );
}
