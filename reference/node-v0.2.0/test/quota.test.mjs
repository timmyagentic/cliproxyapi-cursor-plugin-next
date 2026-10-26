import test from 'node:test'
import assert from 'node:assert/strict'
import { normalizeQuota } from '../quota.mjs'
import { createAdapter, encode, registration } from '../adapter.mjs'

test('quota windows become CPA buckets with plan and reset times', () => {
  const result = normalizeQuota({ plan: 'Pro', windows: [
    { name: 'Cursor Models', used: 25, resetsAt: '2026-11-01T00:00:00Z', models: ['auto', 'composer'] },
    { name: 'Other Models', used: 90 },
    { name: 'Total', used: 45, aside: true },
  ] })
  assert.deepEqual(result.subscription, { plan: 'Pro' })
  const buckets = result.groups[0].buckets
  assert.equal(buckets[0].remainingFraction, 0.75)
  assert.equal(buckets[0].resetTime, '2026-11-01T00:00:00.000Z')
  assert.equal(buckets[1].remainingFraction, 0.09999999999999998)
  assert.match(buckets[2].description, /Informational/)
})

test('clamps overdrawn quotas and skips unknown or invalid percentages', () => {
  const result = normalizeQuota({ windows: [
    { name: 'spent', used: 150 }, { name: 'unused', used: -2 },
    { name: 'unknown' }, { name: 'invalid', used: NaN },
  ] })
  assert.deepEqual(result.groups[0].buckets.map(b => b.remainingFraction), [0, 1])
})

test('on-demand spend without a fixed limit is money rather than fabricated remaining quota', () => {
  const result = normalizeQuota({ windows: [{ name: 'On-demand', used: 0, aside: true, display: '$1,234.56' }] })
  assert.equal(result.groups, undefined)
  assert.equal(result.summary[0].value, 1234.56)
  assert.equal(result.summary[0].currency, 'USD')
})

test('fixed on-demand limits expose both spend and allowance', () => {
  const result = normalizeQuota({ windows: [{ name: 'On-demand', used: 30, display: '$30.00 / $100.00 (you: $5.00)' }] })
  assert.equal(result.groups[0].buckets[0].remainingFraction, 0.7)
  assert.deepEqual(result.summary.map(m => m.value), [30, 100])
})

test('quota failures remain failures and never look like a full allowance', () => {
  assert.throws(() => normalizeQuota({ error: 'Unauthorized', windows: [] }), { status: 401 })
  assert.throws(() => normalizeQuota({ error: 'Too Many Requests', windows: [] }), { status: 429 })
  assert.throws(() => normalizeQuota({ windows: [] }), { status: 502 })
  assert.deepEqual(normalizeQuota({ plan: 'Enterprise', windows: [] }), { subscription: { plan: 'Enterprise' } })
})

test('CPA quota RPC accepts snake-case credentials and declines remote reset', async () => {
  let seenAuth
  const { handle } = await createAdapter({ plugin: { auth: { usage: async getAuth => {
    seenAuth = await getAuth()
    return { plan: 'Ultra', windows: [{ name: 'Cursor Models', used: 12 }] }
  } } } })
  assert.equal(registration.capabilities.quota_provider, true)
  assert.equal((await handle('quota.identifier')).identifier, 'cursor')
  assert.deepEqual((await handle('quota.describe')).supported_providers, ['cursor'])
  assert.equal((await handle('quota.describe')).supports_reset, false)
  const result = await handle('quota.fetch', { storage_json: encode({ type: 'cursor', cursor_auth: { type: 'api', key: 'key' } }) })
  assert.equal(seenAuth.key, 'key')
  assert.equal(result.subscription.plan, 'Ultra')
  await assert.rejects(handle('quota.reset'), { status: 501 })
})
