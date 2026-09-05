import { For, createSignal } from 'solid-js';

import type { Conversation } from './types';

const DOT = ' · ';

export function Sidebar(props: {
  conversations: Conversation[];
  currentId: string | null;
  onOpen: (id: string) => void;
  onCreate: (runtime: string, model: string, title: string) => Promise<void>;
}) {
  const [runtime, setRuntime] = createSignal('claude');
  const [model, setModel] = createSignal('');
  const [title, setTitle] = createSignal('');

  const create = async (ev: SubmitEvent) => {
    ev.preventDefault();
    await props.onCreate(runtime(), model().trim(), title().trim());
    setModel('');
    setTitle('');
  };

  const meta = (c: Conversation) => `${c.runtime}${c.model ? DOT + c.model : ''}${DOT}${new Date(c.updated_at).toLocaleString()}`;

  return (
    <aside class="sidebar">
      <form id="new-conversation" class="new" onSubmit={(e) => void create(e)}>
        <select id="runtime" aria-label="Runtime" value={runtime()} onChange={(e) => setRuntime(e.currentTarget.value)}>
          <option value="claude">claude</option>
          <option value="codex">codex</option>
          <option value="opencode">opencode</option>
        </select>
        <input id="model" placeholder="model (optional)" maxlength="100" value={model()} onInput={(e) => setModel(e.currentTarget.value)} />
        <input id="title" placeholder="title (optional)" maxlength="200" value={title()} onInput={(e) => setTitle(e.currentTarget.value)} />
        <button class="primary" type="submit">
          New conversation
        </button>
      </form>
      <ul id="conversations" class="list">
        <For each={props.conversations}>
          {(c) => (
            <li data-id={c.id} classList={{ active: props.currentId === c.id }} onClick={() => props.onOpen(c.id)}>
              <span>{c.title || '(untitled)'}</span>
              <span class="meta">{meta(c)}</span>
            </li>
          )}
        </For>
      </ul>
    </aside>
  );
}
