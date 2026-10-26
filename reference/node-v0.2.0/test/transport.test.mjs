import test from 'node:test'
import assert from 'node:assert/strict'
import { createHostFetch, cursorFetch, withTransport } from '../transport.mjs'

function hostFixture({ status = 200, chunks = ['hello', ' world'] } = {}) {
  const calls = []
  let at = 0
  const call = async (method, req) => {
    calls.push({ method, req })
    switch (method) {
      case 'host.http.operation_open': return { operation_id: 'operation' }
      case 'host.http.do_stream': return { status_code: status, headers: { 'content-type': ['text/plain'] }, stream_id: 'stream' }
      case 'host.http.stream_read': return at < chunks.length ? { payload: Buffer.from(chunks[at++]).toString('base64') } : { done: true }
      case 'host.http.cancel': case 'host.http.stream_close': return {}
      default: throw new Error(`Unexpected callback: ${method}`)
    }
  }
  return { call, calls }
}

test('all network bytes use CPA callbacks and the host wire profile', async () => {
  const host = hostFixture()
  const fetch = createHostFetch(host.call)
  const response = await fetch('https://api2.cursor.sh/example', { method: 'POST', headers: { Authorization: 'Bearer synthetic' }, body: Buffer.from([0, 255, 4]) })
  assert.equal(response.status, 200)
  assert.equal(await response.text(), 'hello world')
  const req = host.calls.find(call => call.method === 'host.http.do_stream').req
  assert.equal(req.operation_id, 'operation')
  assert.equal(req.url, 'https://api2.cursor.sh/example')
  assert.deepEqual(req.wire_profile, { http1_only: true })
  assert.deepEqual(Buffer.from(req.body, 'base64'), Buffer.from([0, 255, 4]))
  assert.deepEqual(req.headers.Authorization, ['Bearer synthetic'])
  assert.ok(host.calls.some(call => call.method === 'host.http.stream_close'))
  assert.ok(host.calls.some(call => call.method === 'host.http.cancel'))
})

test('canonical headers preserve explicit identity encoding for Go http.Header.Get', async () => {
  const host = hostFixture()
  const response = await createHostFetch(host.call)('https://api2.cursor.sh/run', { headers: { 'accept-encoding': 'identity', 'content-type': 'application/connect+proto' } })
  await response.body.cancel()
  const headers = host.calls.find(call => call.method === 'host.http.do_stream').req.headers
  assert.deepEqual(headers['Accept-Encoding'], ['identity'])
  assert.deepEqual(headers['Content-Type'], ['application/connect+proto'])
})

test('upstream errors retain status and body instead of becoming host failures', async () => {
  const host = hostFixture({ status: 429, chunks: ['quota exceeded'] })
  const response = await createHostFetch(host.call)('https://api2.cursor.sh/limited')
  assert.equal(response.status, 429)
  assert.equal(await response.text(), 'quota exceeded')
})

test('streams read on demand and cancellation releases both CPA handles', async () => {
  const host = hostFixture()
  const response = await createHostFetch(host.call)('https://api2.cursor.sh/run')
  assert.equal(host.calls.filter(call => call.method === 'host.http.stream_read').length, 0)
  const reader = response.body.getReader()
  assert.equal(Buffer.from((await reader.read()).value).toString(), 'hello')
  assert.equal(host.calls.filter(call => call.method === 'host.http.stream_read').length, 1)
  await reader.cancel()
  assert.equal(host.calls.filter(call => call.method === 'host.http.cancel').length, 1)
  assert.equal(host.calls.filter(call => call.method === 'host.http.stream_close').length, 1)
})

test('abort before network setup makes no callbacks', async () => {
  const host = hostFixture()
  await assert.rejects(createHostFetch(host.call)('https://api2.cursor.sh/run', { signal: AbortSignal.abort() }), { name: 'AbortError' })
  assert.equal(host.calls.length, 0)
})

test('abort while waiting for response head uses operation cancellation', async () => {
  let startedResolve, canceledResolve, releaseHead
  const started = new Promise(resolve => { startedResolve = resolve })
  const canceled = new Promise(resolve => { canceledResolve = resolve })
  const controller = new AbortController()
  const call = async method => {
    if (method === 'host.http.operation_open') return { operation_id: 'pending-head' }
    if (method === 'host.http.do_stream') {
      startedResolve()
      return new Promise(resolve => { releaseHead = resolve })
    }
    if (method === 'host.http.cancel') canceledResolve()
    return {}
  }
  const response = createHostFetch(call)('https://api2.cursor.sh/run', { signal: controller.signal })
  await started
  controller.abort()
  await canceled
  releaseHead({ status_code: 200, stream_id: 'late-stream' })
  await assert.rejects(response, { name: 'AbortError' })
})

test('a silent blocked read can be canceled from a separate callback', async () => {
  let startedResolve, rejectRead
  const started = new Promise(resolve => { startedResolve = resolve })
  const controller = new AbortController()
  const host = hostFixture()
  const call = async (method, req) => {
    if (method === 'host.http.stream_read') {
      startedResolve()
      return new Promise((resolve, reject) => { rejectRead = reject })
    }
    if (method === 'host.http.cancel') rejectRead?.(new Error('host operation canceled'))
    return host.call(method, req)
  }
  const response = await createHostFetch(call, { signal: controller.signal })('https://api2.cursor.sh/run')
  const read = response.body.getReader().read()
  await started
  controller.abort()
  await assert.rejects(read, { name: 'AbortError' })
})

test('missing host capability fails without falling back to direct networking', async () => {
  const fetch = createHostFetch(async () => { throw new Error('unsupported host callback') })
  await assert.rejects(fetch('https://api2.cursor.sh/run'), /unsupported host callback/)
})

test('concurrent transport scopes stay isolated across await boundaries', async () => {
  const outputs = await Promise.all(['account-A', 'account-B'].map(name => withTransport({ fetch: async () => {
    await Promise.resolve()
    return new Response(name)
  } }, async () => {
    await Promise.resolve()
    return (await cursorFetch('https://api2.cursor.sh/quota')).text()
  })))
  assert.deepEqual(outputs, ['account-A', 'account-B'])
})

test('204 responses release handles and have no response body', async () => {
  const host = hostFixture({ status: 204 })
  const response = await createHostFetch(host.call)('https://api2.cursor.sh/empty')
  assert.equal(response.body, null)
  assert.ok(host.calls.some(call => call.method === 'host.http.cancel'))
})
