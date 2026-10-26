import { mkdir, open } from 'node:fs/promises'
import { dirname, resolve } from 'node:path'
import { setTimeout } from 'node:timers/promises'
import { createAdapter } from './adapter.mjs'

const args = process.argv.slice(2)
const outputIndex = args.indexOf('--out')
if (outputIndex < 0 || !args[outputIndex + 1]) {
  console.error('Usage: node login.mjs --out /path/to/auths/cursor.json [--cli | --api-key]')
  process.exit(1)
}
try {
  const output = resolve(args[outputIndex + 1])
  let storage
  if (args.includes('--api-key')) {
    const key = process.env.CURSOR_API_KEY?.trim()
    if (!key) throw new Error('Set CURSOR_API_KEY before using --api-key')
    storage = { type: 'cursor', cursor_auth: { type: 'api', key } }
  } else {
    const { handle } = await createAdapter()
    const start = await handle('auth.login.start', { Metadata: { mode: args.includes('--cli') ? 'cli' : 'browser' } })
    if (start.URL) console.error('Open this URL to sign in:\n' + start.URL)
    for (;;) {
      const result = await handle('auth.login.poll', { State: start.State })
      if (result.Status === 'error') throw new Error(result.Message)
      if (result.Status === 'success') {
        storage = JSON.parse(Buffer.from(result.Auth.StorageJSON, 'base64'))
        break
      }
      await setTimeout(2000)
    }
  }
  await mkdir(dirname(output), { recursive: true, mode: 0o700 })
  const file = await open(output, 'wx', 0o600)
  try { await file.writeFile(JSON.stringify(storage, null, 2) + '\n') } finally { await file.close() }
  console.error('Saved Cursor credentials to ' + output)
} catch (error) {
  console.error(error.code === 'EEXIST' ? 'Output file already exists; choose a new --out path.' : error.message)
  process.exitCode = 1
}
