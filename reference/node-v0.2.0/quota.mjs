const clamp = value => Math.max(0, Math.min(1, value))

// Map the reference plugin's windows to CPA's normalized quota contract.
// An unlimited dollar-spend window must never appear as a 100%-remaining
// bucket, and unavailable percentages must never be fabricated as zero.
export function normalizeQuota(usage) {
  if (usage.error) {
    const status = /unauthor|sign.in|expired/i.test(usage.error) ? 401 : /forbidden/i.test(usage.error) ? 403 : /too many/i.test(usage.error) ? 429 : 502
    throw Object.assign(new Error(`Cursor quota: ${usage.error}`), { status })
  }
  const buckets = [], summary = []
  for (const window of usage.windows ?? []) {
    if (window.name === 'On-demand' && window.aside) {
      const amount = /^\$([\d,]+(?:\.\d+)?)/.exec(window.display || '')
      if (amount) summary.push({ key: 'on_demand_spend', label: 'On-demand spend (no fixed limit)', value: Number(amount[1].replaceAll(',', '')), format: 'currency', currency: 'USD' })
      continue
    }
    if (typeof window.used !== 'number' || !Number.isFinite(window.used)) continue
    const description = [window.display, window.aside ? 'Informational; does not independently block model access' : '', window.models?.length ? `Applies to: ${window.models.join(', ')}` : ''].filter(Boolean).join('; ')
    buckets.push({ window: window.name, remainingFraction: clamp(1 - window.used / 100),
      ...(window.resetsAt && Number.isFinite(Date.parse(window.resetsAt)) ? { resetTime: new Date(window.resetsAt).toISOString() } : {}),
      ...(description ? { description } : {}),
    })
    if (window.name === 'On-demand') {
      const dollars = [...(window.display || '').matchAll(/\$([\d,]+(?:\.\d+)?)/g)].slice(0, 2).map(match => Number(match[1].replaceAll(',', '')))
      for (let i = 0; i < dollars.length; i++) summary.push({ key: i ? 'on_demand_limit' : 'on_demand_spend', label: i ? 'On-demand limit' : 'On-demand spend', value: dollars[i], format: 'currency', currency: 'USD' })
    }
  }
  if (!buckets.length && !summary.length && !usage.plan) throw Object.assign(new Error('Cursor returned no quota data'), { status: 502 })
  return {
    ...(usage.plan ? { subscription: { plan: usage.plan } } : {}),
    ...(buckets.length ? { groups: [{ displayName: 'Cursor subscription', buckets }] } : {}),
    ...(summary.length ? { summary } : {}),
  }
}
