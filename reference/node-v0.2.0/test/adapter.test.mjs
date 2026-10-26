import test from 'node:test'
import assert from 'node:assert/strict'
import { createAdapter, encode, registration } from '../adapter.mjs'
import { withTransport } from '../transport.mjs'

const stored = { type: 'cursor', cursor_auth: { type: 'oauth', access: 'test-token', refresh: 'none', expires: 9999999999999 } }
const request = extra => ({ AuthID: 'account-1', Format: 'chat-completions', StorageJSON: encode(stored), Payload: encode({ model: 'auto', messages: [{ role: 'user', content: 'hello' }] }), ...extra })
function fakePlugin(overrides = {}) {
  return {
    auth: { methods: [], loader: async () => ({ fetch: async () => Response.json({ choices: [{ message: { content: 'ok' } }] }) }) },
    provider: { models: async () => ({ auto: { name: 'Auto', limit: { context: 200000 } } }) },
    ...overrides,
  }
}

test('registration declares credential-bound Chat Completions execution', () => {
  assert.equal(registration.capabilities.executor_model_scope, 'oauth')
  assert.deepEqual(registration.capabilities.executor_input_formats, ['chat-completions'])
})

test('auth parser recognizes only Cursor, persists nested credentials and routing flags', async () => {
  const { handle } = await createAdapter({ plugin: fakePlugin() })
  assert.deepEqual(await handle('auth.parse', { RawJSON: encode({ type: 'codex' }) }), { Handled: false })
  const result = await handle('auth.parse', { RawJSON: encode({ ...stored, prefix: 'work', disabled: true }), FileName: 'a.json' })
  assert.equal(result.Auth.Provider, 'cursor')
  assert.equal(result.Auth.FileName, 'a.json')
  assert.equal(result.Auth.Prefix, 'work')
  assert.equal(result.Auth.Disabled, true)
  assert.deepEqual(JSON.parse(Buffer.from(result.Auth.StorageJSON, 'base64')).cursor_auth, stored.cursor_auth)
  await assert.rejects(handle('auth.parse', { RawJSON: encode({ type: 'cursor' }) }), { status: 401 })
  await assert.rejects(handle('auth.parse', { RawJSON: 'bad' }), { status: 400 })
  await assert.rejects(handle('auth.parse', { RawJSON: encode('null') }), { status: 400 })
})

test('models are namespaced and carry context and reasoning levels', async () => {
  const { handle } = await createAdapter({ plugin: fakePlugin({ provider: { models: async () => ({
    'model@300k': { name: 'Model', limit: { context: 300000 }, variants: { low: {}, high: {} } },
  }) } }) })
  assert.deepEqual(await handle('model.static'), { Provider: 'cursor', Models: [] })
  const result = await handle('model.for_auth', request())
  assert.equal(result.Models[0].ID, 'cursor/model@300k')
  assert.equal(result.Models[0].ContextLength, 300000)
  assert.deepEqual(result.Models[0].Thinking.Levels, ['low', 'high'])
})

test('executor preserves tools, history, reasoning, images and stable session identity', async () => {
  let sent, sentHeaders, sentAuth
  const plugin = fakePlugin({ auth: { loader: async getAuth => {
    sentAuth = await getAuth()
    return { fetch: async (url, init) => {
      sent = JSON.parse(init.body); sentHeaders = init.headers
      return Response.json({ choices: [{ message: { content: 'ok' } }] })
    } }
  } } })
  const { handle } = await createAdapter({ plugin })
  const body = { model: 'ignored', messages: [{ role: 'user', content: [{ type: 'image_url', image_url: { url: 'data:image/png;base64,AA==' } }] }], tools: [{ type: 'function', function: { name: 'lookup', parameters: { type: 'object' } } }], reasoning_effort: 'high', service_tier: 'priority' }
  const response = await handle('executor.execute_stream', request({ Model: 'cursor/model@300k', Payload: encode(body), Metadata: { canonical_session_id: 'session-1' } }))
  assert.equal(response.status, 200)
  assert.deepEqual(sent, { ...body, model: 'model@300k', stream: true })
  assert.deepEqual(sentAuth, stored.cursor_auth)
  assert.equal(sentHeaders['x-magpie-cursor-session'], 'account-1:session-1')
})

test('upstream status is preserved for both streaming and non-streaming failures', async () => {
  for (const status of [401, 403, 404, 429, 502]) {
    const plugin = fakePlugin({ auth: { loader: async () => ({ fetch: async () => Response.json({ error: { message: 'refused' } }, { status }) }) } })
    const { handle } = await createAdapter({ plugin })
    for (const method of ['executor.execute', 'executor.execute_stream']) await assert.rejects(handle(method, request()), { status, message: 'refused' })
  }
})

test('unsupported operations and protocol fail explicitly', async () => {
  const { handle } = await createAdapter({ plugin: fakePlugin() })
  await assert.rejects(handle('executor.count_tokens'), { status: 501 })
  await assert.rejects(handle('executor.http_request'), { status: 501 })
  await assert.rejects(handle('executor.execute', request({ Format: 'claude' })), { status: 400 })
  assert.equal((await handle('executor.execute', request({ AuthAttributes: { proxy_url: 'http://proxy:80' } }))).status, 200)
  const proxied = request({ StorageJSON: encode({ ...stored, proxy_url: 'http://proxy:80' }) })
  assert.equal((await handle('executor.execute', proxied)).status, 200)
  assert.equal((await handle('model.for_auth', proxied)).Provider, 'cursor')
})

test('CPA global proxies are accepted while unforwarded per-account overrides fail explicitly', async () => {
  const { handle } = await createAdapter({ plugin: fakePlugin() })
  await withTransport({ host: true }, async () => {
    const result = await handle('model.for_auth', request({ Host: { ProxyURL: 'socks5://global:1080' } }))
    assert.equal(result.Provider, 'cursor')
    await assert.rejects(handle('executor.execute', request({ StorageJSON: encode({ ...stored, proxy_url: 'http://account:8080' }) })), { status: 501 })
  })
})

test('browser login uses PKCE and returns pending, success, and idempotent success', async () => {
  let polls = 0, polled
  const { handle } = await createAdapter({ plugin: fakePlugin(), fetchImpl: async url => {
    polled = url
    if (++polls === 1) return new Response(null, { status: 404 })
    return Response.json({ accessToken: 'opaque-token', refreshToken: '' })
  } })
  const started = await handle('auth.login.start')
  const url = new URL(started.URL)
  assert.equal(url.origin, 'https://cursor.com')
  assert.equal(url.searchParams.get('mode'), 'login')
  assert.ok(url.searchParams.get('challenge'))
  assert.ok(!started.State.includes('verifier'))
  assert.deepEqual(await handle('auth.login.poll', { State: started.State }), { Status: 'pending' })
  const result = await handle('auth.login.poll', { State: started.State })
  assert.equal(result.Status, 'success')
  assert.equal(result.Auth.Metadata.cursor_auth.access, 'opaque-token')
  assert.equal(polled.searchParams.get('uuid'), url.searchParams.get('uuid'))
  const { createHash } = await import('node:crypto')
  assert.equal(createHash('sha256').update(polled.searchParams.get('verifier')).digest('base64url'), url.searchParams.get('challenge'))
  assert.deepEqual(await handle('auth.login.poll', { State: started.State }), result)
  assert.equal(polls, 2)
})

test('login expiration and stored token expiration are distinct', async () => {
  let clock = 0
  const { handle } = await createAdapter({ plugin: fakePlugin(), now: () => clock })
  const started = await handle('auth.login.start')
  clock = 16 * 60_000
  assert.equal((await handle('auth.login.poll', { State: started.State })).Status, 'error')
  await assert.rejects(handle('auth.refresh', { StorageJSON: encode({ ...stored, cursor_auth: { ...stored.cursor_auth, expires: 1 } }) }), { status: 401 })
  const refreshed = await handle('auth.refresh', request({ AuthID: 'keep-host-id' }))
  assert.equal(refreshed.Auth.ID, 'keep-host-id')
})

test('API keys and read-only cursor-agent credentials retain their credential mode', async () => {
  const plugin = fakePlugin({ auth: {
    methods: [null, { authorize: async () => ({ callback: async () => ({ type: 'success', access: '', refresh: 'cursor-agent', expires: 0, accountId: 'me@example.com' }) }) }],
  } })
  const { handle } = await createAdapter({ plugin })
  const start = await handle('auth.login.start', { Metadata: { mode: 'cli' } })
  const result = await handle('auth.login.poll', { State: start.State })
  assert.equal(result.Auth.Metadata.cursor_auth.refresh, 'cursor-agent')
  const api = await handle('auth.parse', { RawJSON: encode({ type: 'cursor', cursor_auth: { type: 'api', key: 'test-key' } }) })
  assert.equal(api.Auth.Metadata.cursor_auth.type, 'api')
})
