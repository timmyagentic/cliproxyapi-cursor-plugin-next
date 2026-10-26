import { createInterface } from 'node:readline'
import { once } from 'node:events'
import { createAdapter } from './adapter.mjs'
import { createHostFetch, withTransport } from './transport.mjs'

const adapter = await createAdapter()
const active = new Map()
const pendingHost = new Map()
let nextCallback = 0
async function send(message) {
  if (!process.stdout.write(JSON.stringify(message) + '\n')) await once(process.stdout, 'drain')
}

async function dispatch(message) {
  if (message.callback_reply) {
    const pending = pendingHost.get(message.callback_reply)
    if (!pending) return
    pendingHost.delete(message.callback_reply)
    const envelope = message.envelope
    if (!envelope?.ok) pending.reject(Object.assign(new Error(envelope?.error?.message || 'CPA HTTP callback failed'), { status: envelope?.error?.http_status || 502 }))
    else pending.resolve(envelope.result ?? {})
    return
  }
  const { id, method, request } = message
  if (message.ack) { active.get(id)?.ack?.(); return }
  if (message.cancel) {
    const current = active.get(id)
    current?.controller.abort(); current?.ack?.()
    return
  }
  const current = { controller: new AbortController(), operations: new Map() }
  active.set(id, current)
  const call = (method, request) => new Promise((resolve, reject) => {
    const callbackID = String(++nextCallback)
    pendingHost.set(callbackID, { resolve, reject })
    void send({ id, callback: { id: callbackID, method, request } }).catch(error => {
      pendingHost.delete(callbackID); reject(error)
    })
  })
  const fetch = createHostFetch(call, {
    signal: current.controller.signal,
    onOpen: (operationID, close) => current.operations.set(operationID, close),
    onClose: operationID => current.operations.delete(operationID),
  })
  const cleanup = () => Promise.allSettled([...current.operations.values()].map(close => close()))
  return withTransport({ host: true, fetch }, async () => {
  let started = false
  try {
    const result = await adapter.handle(method, request, current.controller.signal)
    if (!(result instanceof Response)) { await cleanup(); await send({ id, result }); return }
    const headers = Object.fromEntries([...result.headers].map(([key, value]) => [key, [value]]))
    if (method !== 'executor.execute_stream') {
      const payload = Buffer.from(await result.arrayBuffer()).toString('base64')
      await cleanup()
      await send({ id, result: { Payload: payload, Headers: headers } })
      return
    }
    started = true
    await send({ id, result: { headers } })
    const reader = result.body.getReader()
    try {
      while (!current.controller.signal.aborted) {
        const { done, value } = await reader.read()
        if (done) break
        // One unacknowledged chunk per stream bounds memory and applies
        // the host's downstream backpressure to the upstream reader.
        const acknowledged = new Promise(resolve => { current.ack = resolve })
        await send({ id, event: { payload: Buffer.from(value).toString('base64') } })
        await acknowledged
        current.ack = null
      }
    } finally {
      await reader.cancel().catch(() => {})
      reader.releaseLock()
    }
    await cleanup()
    await send({ id, done: true })
  } catch (error) {
    await cleanup()
    await send({ id, ...(started ? { done: true } : {}), error: {
      code: 'cursor_error', message: error.message || 'Cursor request failed', http_status: error.status || 502,
    } })
  } finally { active.delete(id) }
  })
}

const lines = createInterface({ input: process.stdin, crlfDelay: Infinity })
lines.on('line', line => {
  let message
  try { message = JSON.parse(line) } catch { process.exitCode = 1; lines.close(); return }
  void dispatch(message).catch(() => { process.exit(1) })
})
// A host crash closes stdin. Do not leave native Runs or login state alive.
lines.on('close', () => {
  for (const value of active.values()) { value.controller.abort(); value.ack?.() }
  process.exit(0)
})
