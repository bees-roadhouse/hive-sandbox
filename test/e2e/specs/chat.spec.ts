import { expect, test } from '../fixtures';

/**
 * The browser client, driven end to end: sign in, start a thread, send a
 * message, see it wait for an agent, survive a reload, sign out.
 *
 * No agent answers here. The daemon under test runs with the chat worker off
 * (see the daemon fixture), so the assertion is that a posted message is
 * durably waiting ... "waiting for an agent" is what the page shows for a
 * pending turn, and it must still show it after a reload, because that state
 * comes from the daemon and not from anything the page remembered.
 *
 * Nothing in these specs puts a token in a URL or in script-readable storage.
 * The page exchanges it once for the cookie, and the reload is what proves the
 * cookie is what carried it.
 */

test('sign in, start a conversation, and post a message that waits for an agent', async ({ page, daemon }) => {
  await page.goto(`${daemon.url}/`);
  await expect(page.locator('#login')).toBeVisible();
  await expect(page.locator('#app')).toBeHidden();

  await page.fill('#token', daemon.token);
  await page.click('#signin');
  await expect(page.locator('#app')).toBeVisible();
  await expect(page.locator('#identity')).toContainText('e2e-root');
  await expect(page.locator('#identity')).toContainText(daemon.version);

  await page.fill('#title', 'first thread');
  await page.click('#new-conversation button[type=submit]');
  await expect(page.locator('#conversations li.active')).toContainText('first thread');
  await expect(page.locator('#conversations li.active')).toContainText('claude');

  await page.fill('#body', 'hello there');
  await page.click('#composer button[type=submit]');
  await expect(page.locator('.msg.user')).toContainText('hello there');
  await expect(page.locator('.msg.live .who')).toContainText('waiting for an agent');
  await expect(page.locator('#body')).toHaveValue('');

  // The state survives a reload because the daemon holds it, and the session
  // survives because the cookie does.
  await page.reload();
  await expect(page.locator('#app')).toBeVisible();
  await expect(page.locator('#login')).toBeHidden();
  await page.click('#conversations li');
  await expect(page.locator('.msg.user')).toContainText('hello there');
  await expect(page.locator('.msg.live .who')).toContainText('waiting for an agent');

  await page.click('#logout');
  await expect(page.locator('#login')).toBeVisible();
  await page.reload();
  await expect(page.locator('#login')).toBeVisible();
});

test('a credential that does not resolve is refused on the page', async ({ page, daemon }) => {
  await page.goto(`${daemon.url}/`);
  await page.fill('#token', 'not-a-real-token');
  await page.click('#signin');
  await expect(page.locator('#login-error')).toBeVisible();
  await expect(page.locator('#app')).toBeHidden();
  await expect(page.locator('#token')).toHaveValue('');
});

test('the page ships under a policy that keeps a rendered message inert', async ({ request, daemon }) => {
  const res = await request.get(`${daemon.url}/`);
  expect(res.status()).toBe(200);
  const csp = res.headers()['content-security-policy'] ?? '';
  expect(csp).toContain("default-src 'none'");
  expect(csp).toContain("script-src 'self'");
  expect(res.headers()['x-content-type-options']).toBe('nosniff');
});
