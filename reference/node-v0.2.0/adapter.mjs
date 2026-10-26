import { createHash, randomBytes, randomUUID } from 'node:crypto'
import { CursorAuthPlugin } from './vendor/cursor/index.mjs'
import { cursorFetch, currentTransport } from './transport.mjs'
import { normalizeQuota } from './quota.mjs'

export const registration = {
  schema_version: 1,
  metadata: {
    Name: 'Cursor', Version: '0.2.0', Author: 'Community',
    ConfigFields: [],
  },
  capabilities: {
    auth_provider: true, model_provider: true, executor: true, quota_provider: true,
    executor_model_scope: 'oauth',
    executor_input_formats: ['chat-completions'],
    executor_output_formats: ['chat-completions'],
  },
}

export class PluginError extends Error {
  constructor(message, status = 500) { super(message); this.status = status }
}

const encode = value => Buffer.from(typeof value === 'string' ? value : JSON.stringify(value)).toString('base64')
function decode(value) {
  try {
    const result = JSON.parse(Buffer.from(value || '', 'base64').toString())
    if (!result || typeof result !== 'object' || Array.isArray(result)) throw new Error('Expected object')
    return result
  }
  catch { throw new PluginError('Invalid JSON payload', 400) }
}

// Storage uses a separate marker so the host's `type: cursor` cannot
// overwrite OpenCode's `type: oauth | api` during metadata persistence.
function credentials(req) {
  const stored = req.StorageJSON ? decode(req.StorageJSON) : (req.AuthMetadata ?? req.Metadata ?? {})
  const auth = stored.cursor_auth
  if (!auth || !['oauth', 'api'].includes(auth.type)) throw new PluginError('Missing Cursor credentials', 401)
  if (auth.type === 'api' ? !auth.key : !auth.access && auth.refresh !== 'cursor-agent')
    throw new PluginError('Missing Cursor token or API key', 401)
  return auth
}

function checkAccountProxy(req) {
  if (!currentTransport()?.host) return
  const encoded = req.StorageJSON ?? req.storage_json
  const stored = encoded ? decode(encoded) : {}
  if (stored.proxy_url || req.AuthAttributes?.proxy_url || req.Attributes?.proxy_url || req.attributes?.proxy_url || req.AuthMetadata?.proxy_url || req.Metadata?.proxy_url || req.metadata?.proxy_url)
    throw new PluginError('This CPA host HTTP interface does not forward per-account proxy overrides; configure requests.proxy-url globally and remove the account proxy_url', 501)
}

function authData(auth, filename, label, extra = {}) {
  const identity = auth.accountId || createHash('sha256').update(auth.key || auth.access || auth.refresh).digest('hex').slice(0, 16)
  const storage = { ...extra, type: 'cursor', cursor_auth: auth }
  return {
    Provider: 'cursor', ID: `cursor-${identity}`,
    FileName: filename || `cursor-${createHash('sha256').update(identity).digest('hex').slice(0, 16)}.json`,
    Label: label || auth.accountId || 'Cursor', StorageJSON: encode(storage), Metadata: storage,
  }
}

function modelInfo(id, model) {
  const levels = Object.keys(model.variants ?? {})
  return {
    ID: `cursor/${id}`, Object: 'model', OwnedBy: 'cursor', Type: 'cursor',
    Name: id, DisplayName: model.name || id,
    ContextLength: model.limit?.context || 200000,
    InputTokenLimit: model.limit?.context || 200000,
    MaxCompletionTokens: model.limit?.output || 0,
    SupportedGenerationMethods: ['chat'],
    SupportedInputModalities: ['text', 'image'], SupportedOutputModalities: ['text'],
    SupportedParameters: ['tools', 'tool_choice', ...(levels.length ? ['reasoning_effort'] : [])],
    ...(levels.length ? { Thinking: { Levels: levels, ZeroAllowed: levels.includes('none'), DynamicAllowed: false } } : {}),
  }
}

export async function createAdapter({ plugin, fetchImpl = cursorFetch, now = Date.now } = {}) {
  plugin ??= await CursorAuthPlugin()
  const logins = new Map()
  const headers = { 'Content-Type': 'application/json', 'x-cursor-client-type': 'cli', 'x-cursor-client-version': 'cli-2026.09.23-86fc751' }
  async function startLogin(req) {
    for (const [id, flow] of logins) if (flow.expires <= now()) logins.delete(id)
    if (logins.size >= 128) throw new PluginError('Too many pending logins', 429)
    const state = randomUUID(), expires = now() + 15 * 60_000
    const mode = req.Metadata?.mode || 'browser'
    if (mode === 'cli') {
      const flow = await plugin.auth.methods[1].authorize()
      logins.set(state, { mode, expires, flow })
      return { Provider: 'cursor', URL: '', State: state, ExpiresAt: new Date(expires).toISOString() }
    }
    if (mode !== 'browser') throw new PluginError('Login mode must be browser or cli', 400)
    const verifier = randomBytes(32).toString('base64url'), uuid = randomUUID()
    const challenge = createHash('sha256').update(verifier).digest('base64url')
    const url = new URL('https://cursor.com/loginDeepControl')
    for (const [key, value] of Object.entries({ challenge, uuid, mode: 'login', redirectTarget: 'cli' })) url.searchParams.set(key, value)
    logins.set(state, { mode, expires, verifier, uuid })
    return { Provider: 'cursor', URL: url.href, State: state, ExpiresAt: new Date(expires).toISOString() }
  }
  async function pollLogin(req) {
    const flow = logins.get(req.State)
    if (!flow || flow.expires <= now()) {
      logins.delete(req.State)
      return { Status: 'error', Message: 'Login expired; start again' }
    }
    // Deduplicate concurrent polling without exposing PKCE material in State.
    if (flow.pending) return flow.pending
    flow.pending = (async () => {
      let auth
      if (flow.mode === 'cli') {
        const result = await flow.flow.callback()
        if (result.type !== 'success') return { Status: 'error', Message: result.error }
        const { type, ...rest } = result
        auth = { ...rest, type: 'oauth' }
      } else {
        const url = new URL('https://api2.cursor.sh/auth/poll')
        url.searchParams.set('uuid', flow.uuid); url.searchParams.set('verifier', flow.verifier)
        const response = await fetchImpl(url, { headers, signal: AbortSignal.timeout(15000) })
        if (response.status === 404) return { Status: 'pending' }
        if (!response.ok) throw new PluginError(`Cursor login returned HTTP ${response.status}`, response.status)
        const result = await response.json()
        if (!result.accessToken) throw new PluginError('Cursor login returned no access token', 502)
        let expires = now() + 30 * 24 * 3600_000
        try { expires = JSON.parse(Buffer.from(result.accessToken.split('.')[1], 'base64url')).exp * 1000 || expires } catch {}
        auth = { type: 'oauth', access: result.accessToken, refresh: result.refreshToken || 'none', expires }
      }
      // Keep successful results for idempotent poll retries until expiry.
      flow.result = { Status: 'success', Auth: authData(auth) }
      return flow.result
    })().finally(() => { flow.pending = null })
    return flow.pending
  }

  async function handle(method, req = {}, signal) {
    switch (method) {
      case 'plugin.register': case 'plugin.reconfigure': return registration
      case 'executor.identifier': case 'auth.identifier': case 'quota.identifier': return { identifier: 'cursor' }
      case 'quota.describe': return { supported_providers: ['cursor'], display_name: 'Cursor subscription', supports_reset: false }
      case 'quota.reset': throw new PluginError('Cursor subscription quota cannot be reset by the plugin', 501)
      case 'quota.fetch': {
        checkAccountProxy(req)
        const credential = credentials({ StorageJSON: req.storage_json ?? req.StorageJSON, Metadata: req.metadata ?? req.Metadata })
        return normalizeQuota(await plugin.auth.usage(async () => credential, { models: {} }))
      }
      case 'auth.parse': {
        const raw = decode(req.RawJSON)
        if (raw.type !== 'cursor') return { Handled: false }
        const auth = credentials({ StorageJSON: req.RawJSON })
        return { Handled: true, Auth: { ...authData(auth, req.FileName, raw.label, raw), Prefix: raw.prefix || '', ProxyURL: raw.proxy_url || '', Disabled: Boolean(raw.disabled) } }
      }
      case 'auth.login.start': return startLogin(req)
      case 'auth.login.poll': {
        const flow = logins.get(req.State)
        return flow?.expires > now() && flow.result ? flow.result : pollLogin(req)
      }
      case 'auth.refresh': {
        const auth = credentials(req)
        if (auth.type === 'oauth' && auth.refresh !== 'cursor-agent' && auth.expires && auth.expires <= now())
          throw new PluginError('Cursor token expired; sign in again', 401)
        const stored = req.StorageJSON ? decode(req.StorageJSON) : (req.Metadata || {})
        return { Auth: { ...authData(auth, undefined, undefined, stored), ID: req.AuthID || authData(auth).ID }, NextRefreshAfter: new Date(now() + 3600_000).toISOString() }
      }
      case 'model.static': return { Provider: 'cursor', Models: [] }
      case 'model.for_auth': {
        checkAccountProxy(req)
        const models = await plugin.provider.models({ models: {} }, { auth: credentials(req) })
        return { Provider: 'cursor', Models: Object.entries(models).map(([id, model]) => modelInfo(id, model)) }
      }
      case 'executor.count_tokens': throw new PluginError('Cursor does not expose a token counting API', 501)
      case 'executor.http_request': throw new PluginError('Arbitrary HTTP forwarding is not supported', 501)
      case 'executor.execute': case 'executor.execute_stream': {
        checkAccountProxy(req)
        if (req.Format && req.Format !== 'chat-completions') throw new PluginError('Expected chat-completions format', 400)
        const auth = credentials(req), chat = decode(req.Payload)
        chat.model = (req.Model || chat.model || 'auto').replace(/^cursor\//, '')
        chat.stream = method === 'executor.execute_stream'
        const loader = await plugin.auth.loader(async () => auth)
        const session = req.Metadata?.canonical_session_id || req.Metadata?.execution_session_id || req.Metadata?.derived_session_id
        const requestHeaders = session ? { 'x-magpie-cursor-session': `${req.AuthID || 'cursor'}:${session}` } : {}
        const response = await loader.fetch('https://cursor.invalid/chat/completions', {
          method: 'POST', body: JSON.stringify(chat), signal, headers: requestHeaders,
        })
        if (!response.ok) {
          let message = `Cursor returned HTTP ${response.status}`
          try { const body = await response.json(); message = body.error?.message || body.message || message } catch {}
          throw new PluginError(message, response.status)
        }
        return response
      }
      default: throw new PluginError(`Unknown method: ${method}`, 400)
    }
  }
  return { handle }
}

export { encode }
