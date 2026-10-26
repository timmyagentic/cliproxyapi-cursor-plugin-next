// Developer-only fixture generation. Native builds and tests do not run JS.
import { readFileSync, writeFileSync } from 'node:fs'
import vm from 'node:vm'
import { _internal as ref } from '../reference/node-v0.2.0/vendor/cursor/index.mjs'

const upstream = readFileSync(new URL('../reference/node-v0.2.0/vendor/cursor/context.test.mjs', import.meta.url), 'utf8')
const pickerText = upstream.match(/export const PICKER = (\[[\s\S]*?\n\])\n/)[1]
const helpers = upstream.slice(upstream.indexOf('const en ='), upstream.indexOf('export const PICKER'))
const picker = JSON.parse(JSON.stringify(vm.runInNewContext(helpers + '\n;(' + pickerText + ')')))
const choices = [null, ['claude-opus-5-5-medium', 'claude-opus-5-5-high', 'composer-2.5'], ['grok-4.7-high', 'composer-2.5']]
const modelCases = []
for (const ids of choices) {
  const raw = ref.modelsOf(picker, ids ? new Set(ids) : null)
  const names = [...raw.map(entry => entry.id), 'claude-opus-5-5', 'claude-5.5-opus', 'claude-opus-5-5-high-fast', 'claude-haiku-5-5-low', 'auto', 'some-cursor-id']
  const resolutions = []
  for (const model of names) for (const effort of ['', 'none', 'low', 'high', 'xhigh', 'ultra']) for (const tier of ['', 'priority', 'default']) {
    const result = ref.request(raw, model, effort, tier === 'priority' ? true : tier === 'default' ? false : undefined)
    resolutions.push({ model, effort, tier, id: result.id, max: result.maxMode, params: Object.fromEntries(result.params.map(p => [p.id, p.value])) })
  }
  modelCases.push({ picker, ids, catalog: raw.map(m => ({ id: m.id, name: m.name, context: m.context, levels: m.levels, fast: m.offersFast })), resolutions })
}
const tool = id => ({ id, type: 'function', function: { name: 'read', arguments: '{"path":"a"}' } })
const tools = [{ type: 'function', function: { name: 'read', description: 'Read source', parameters: { type: 'object', properties: { path: { type: 'string' } } } } }]
const chats = [
  { messages: [{ role: 'user', content: '你好，hello' }] },
  { tools, messages: [{ role: 'system', content: 'System' }, { role: 'developer', content: 'Developer' }, { role: 'user', content: 'read a' }] },
  { messages: [{ role: 'user', content: 'read both' }, { role: 'assistant', content: null, reasoning_content: 'First', tool_calls: [tool('a')] }, { role: 'assistant', content: 'Reading', reasoning: 'Second', tool_calls: [tool('b')] }, { role: 'tool', tool_call_id: 'a', content: 'A' }, { role: 'tool', tool_call_id: 'b', content: 'B' }] },
  { messages: [{ role: 'user', content: 'read' }, { role: 'assistant', tool_calls: [tool('missing')] }, { role: 'user', content: 'continue' }] },
  { messages: [{ role: 'user', content: 'read' }, { role: 'assistant', tool_calls: [tool('call_a__fc_1')] }, { role: 'tool', tool_call_id: 'call_a__fc_1', content: '{"error":"denied"}', is_error: true }] },
  { messages: [{ role: 'user', content: [{ type: 'text', text: 'image' }, { type: 'image_url', image_url: { url: 'data:image/png;base64,AAEC/w==' } }, { type: 'image_url', image_url: { url: 'https://example.invalid/image.png' } }, { type: 'file', file: { filename: 'a.txt' } }] }] },
  { tools, tool_choice: 'none', messages: [{ role: 'assistant', content: null, reasoning: 'Reasoning only.' }, { role: 'user', content: 'continue' }] },
]
const conversations = chats.map(chat => {
  const tools = ref.toolsOf(chat), conv = ref.conversation(chat, tools)
  return { chat, messages: conv.msgs.map(buffer => JSON.parse(buffer)), last: conv.last }
})
const values = [null, true, false, 0, 3.25, '你好', [], {}, { name: 'example', values: [null, false, 17, { x: 'value' }] }]
const pb = values.map(value => ({ value, hex: Buffer.from(ref.pbValue(value).done()).toString('hex') }))
writeFileSync(new URL('../testdata/reference-golden.json', import.meta.url), JSON.stringify({ modelCases, conversations, pb }, null, 2) + '\n')
console.log('Generated native parity fixtures from the MIT reference implementation.')
