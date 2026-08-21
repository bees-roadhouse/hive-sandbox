import { expect, test } from '../fixtures';

test('GET /healthz returns 200 and the version the binary was built with', async ({ request, daemon }) => {
  const res = await request.get(`${daemon.url}/healthz`);

  expect(res.status()).toBe(200);
  expect(res.headers()['content-type']).toContain('application/json');

  const body = (await res.json()) as { status?: string; version?: string };
  expect(body.status).toBe('ok');
  // daemon.version comes from `hive-sandbox -version` on the same binary, so
  // this catches the handler reporting something other than what it is.
  expect(body.version).toBe(daemon.version);
  expect(body.version).not.toBe('');
});

test('the daemon 404s a path it does not serve', async ({ request, daemon }) => {
  const res = await request.get(`${daemon.url}/nope`);
  expect(res.status()).toBe(404);
});

test('/healthz rejects a non-GET method', async ({ request, daemon }) => {
  // The route is registered as `GET /healthz`, so ServeMux answers 405 here.
  const res = await request.post(`${daemon.url}/healthz`);
  expect(res.status()).toBe(405);
});
