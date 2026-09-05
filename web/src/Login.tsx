import { createSignal, onMount } from 'solid-js';

import { signIn } from './api';

export function Login(props: { onSignedIn: () => Promise<void> }) {
  const [token, setToken] = createSignal('');
  const [error, setError] = createSignal('');
  let input: HTMLInputElement | undefined;

  const submit = async () => {
    const t = token().trim();
    setError('');
    if (!t) return;
    const ok = await signIn(t);
    // Cleared either way: the page never keeps a credential, and a refused one
    // must not sit in a field waiting for a second try to leak it.
    setToken('');
    if (!ok) {
      setError('That credential was not accepted.');
      return;
    }
    await props.onSignedIn();
  };

  onMount(() => input?.focus());

  return (
    <main id="login" class="card">
      <h1>Sign in to your hive</h1>
      <p class="hint">Paste a credential. It is exchanged once for a session cookie and never kept by this page.</p>
      <label>
        Credential
        <input
          id="token"
          ref={input}
          type="password"
          autocomplete="off"
          placeholder="token"
          value={token()}
          onInput={(e) => setToken(e.currentTarget.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter') void submit();
          }}
        />
      </label>
      <button id="signin" class="primary" onClick={() => void submit()}>
        Sign in
      </button>
      <p id="login-error" class="error" hidden={!error()}>
        {error()}
      </p>
    </main>
  );
}
