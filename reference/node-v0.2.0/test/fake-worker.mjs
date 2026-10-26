// Exercises the native bridge without network calls or Cursor credentials.
import { createInterface } from 'node:readline'
const states = new Map()
const callbacks = new Map()
const send = value => process.stdout.write(JSON.stringify(value) + '\n')
createInterface({ input: process.stdin }).on('line', line => {
  const message = JSON.parse(line), { id } = message
  if (message.callback_reply) {
    const origin = callbacks.get(message.callback_reply)
    callbacks.delete(message.callback_reply)
    send({ id: origin, result: message.envelope })
    return
  }
  if (message.cancel) { states.delete(id); send({ id, done: true }); return }
  if (message.ack) {
    const step = states.get(id)
    if (step === 1) {
      states.set(id, 2)
      send({ id, event: { payload: Buffer.from('data: [DONE]\n\n').toString('base64') } })
    } else { states.delete(id); send({ id, done: true }) }
    return
  }
  if (message.method === 'fixture.callback') {
    const callbackID = `callback-${id}`
    callbacks.set(callbackID, id)
    send({ id, callback: { id: callbackID, method: message.request.method || 'host.http.operation_open', request: { host_callback_id: 'forged-context' } } })
  } else if (message.method === 'executor.execute_stream') {
    send({ id, result: { headers: { 'content-type': ['text/event-stream'] } } })
    states.set(id, 1)
    send({ id, event: { payload: Buffer.from('data: {"choices":[{"delta":{"content":"hello"}}]}\n\n').toString('base64') } })
  } else send({ id, result: { identifier: 'fixture' } })
})
