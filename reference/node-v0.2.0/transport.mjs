import { AsyncLocalStorage } from 'node:async_hooks'

const scope = new AsyncLocalStorage()
export const withTransport = (transport, fn) => scope.run(transport, fn)
export const currentTransport = () => scope.getStore()
export const cursorFetch = (...args) => (scope.getStore()?.fetch ?? globalThis.fetch)(...args)

// CPA owns connections, proxies, logging and cancellation. No request made
// from a native plugin call may silently fall back to Node's direct network.
export function createHostFetch(call, { onOpen = () => {}, onClose = () => {}, signal: parentSignal } = {}) {
  return async (input, init = {}) => {
    const request = new Request(input, init)
    const signal = parentSignal ? AbortSignal.any([request.signal, parentSignal]) : request.signal
    signal.throwIfAborted()
    const body = request.body ? Buffer.from(await request.arrayBuffer()) : null
    signal.throwIfAborted()
    const { operation_id: operationID } = await call('host.http.operation_open', {})
    if (!operationID) throw new Error('CPA did not allocate an HTTP operation')
    let streamID, closed = false
    const finish = async () => {
      if (closed) return
      closed = true
      signal.removeEventListener('abort', abort)
      onClose(operationID)
      // Cancel also releases an operation still waiting for its response head.
      const closing = [call('host.http.cancel', { operation_id: operationID })]
      if (streamID) closing.push(call('host.http.stream_close', { stream_id: streamID }))
      await Promise.allSettled(closing)
    }
    const abort = () => { void finish() }
    onOpen(operationID, finish)
    signal.addEventListener('abort', abort, { once: true })
    try {
      if (signal.aborted) { await finish(); signal.throwIfAborted() }
      const response = await call('host.http.do_stream', {
        operation_id: operationID,
        method: request.method, url: request.url,
        // Go's http.Header.Get expects canonical keys. In particular, keep
        // Accept-Encoding: identity visible so net/http does not inject gzip
        // into Cursor's binary RunSSE transport alongside the caller's header.
        headers: Object.fromEntries([...request.headers].map(([key, value]) => [key.replace(/(^|-)[a-z]/g, part => part.toUpperCase()), [value]])),
        ...(body ? { body: body.toString('base64') } : {}),
        wire_profile: { http1_only: true },
      })
      streamID = response.stream_id
      if (!streamID) throw new Error('CPA did not return an HTTP stream')
      if (signal.aborted || closed) {
        await call('host.http.stream_close', { stream_id: streamID }).catch(() => {})
        signal.throwIfAborted()
        throw new Error('CPA request scope closed')
      }
      const status = response.status_code
      const headers = new Headers()
      for (const [key, values] of Object.entries(response.headers ?? {}))
        for (const value of values) headers.append(key, value)
      if (request.method === 'HEAD' || [101, 204, 205, 304].includes(status)) {
        await finish()
        return new Response(null, { status, headers })
      }
      const stream = new ReadableStream({
        async pull(controller) {
          try {
            signal.throwIfAborted()
            const chunk = await call('host.http.stream_read', { stream_id: streamID })
            signal.throwIfAborted()
            if (chunk.error) throw new Error(chunk.error)
            if (chunk.payload) controller.enqueue(Buffer.from(chunk.payload, 'base64'))
            if (chunk.done) { await finish(); controller.close() }
          } catch (error) {
            await finish()
            controller.error(signal.aborted ? signal.reason : error)
          }
        },
        cancel: finish,
      }, { highWaterMark: 0 })
      return new Response(stream, { status, headers })
    } catch (error) {
      await finish()
      if (signal.aborted) throw signal.reason
      throw error
    }
  }
}
