import assert from 'node:assert/strict'
import test from 'node:test'

import {
  asArray,
  collectGroupRates,
  collectModelsByGroup,
  finiteNumber,
  normalizeHealth,
  normalizeURL,
  sanitizeError,
  sha256,
  unwrap,
} from '../dist/contracts.js'

test('normalizes Sub2API response wrappers', () => {
  assert.deepEqual(unwrap({ data: { items: [1] } }), { items: [1] })
  assert.deepEqual(asArray({ records: [1, 2] }), [1, 2])
  assert.deepEqual(asArray(null), [])
})

test('normalizes health without promoting unknown values', () => {
  assert.equal(normalizeHealth('正常'), 'operational')
  assert.equal(normalizeHealth('operational'), 'operational')
  assert.equal(normalizeHealth('DEGRADED'), 'degraded')
  assert.equal(normalizeHealth('red'), 'failed')
})

test('sanitizes credentials and validates numeric values', () => {
  assert.equal(finiteNumber(-1), undefined)
  assert.equal(finiteNumber('0.15'), 0.15)
  assert.equal(
    sanitizeError('Bearer abc sk-secret'),
    'Bearer [redacted] sk-[redacted]'
  )
})

test('normalizes HTTPS URLs and hashes keys deterministically', async () => {
  assert.equal(normalizeURL('https://example.com/'), 'https://example.com')
  assert.equal(normalizeURL('http://example.com'), '')
  assert.equal(
    await sha256('key'),
    '2c70e12b7a0646f92279f427c7b38e7334d8e5389cff167a1dc30e73f826b683'
  )
})

test('joins current Sub2API platform sections to groups and user rates', () => {
  const modelsByGroup = collectModelsByGroup({
    data: [
      {
        name: 'OpenAI',
        platforms: [
          {
            platform: 'openai',
            groups: [{ id: 12, name: 'OpenAI 0.12x' }],
            supported_models: [{ name: 'gpt-5.5' }, { name: 'gpt-5.6' }],
          },
        ],
      },
    ],
  })
  const rates = collectGroupRates({ data: { 12: 0.08 } })

  assert.deepEqual(modelsByGroup.get('12'), ['gpt-5.5', 'gpt-5.6'])
  assert.equal(rates.get('12').user_rate_multiplier, 0.08)
})

test('keeps compatibility with legacy flat channel and rate arrays', () => {
  const modelsByGroup = collectModelsByGroup({
    data: [
      {
        groups: [{ group_id: 'legacy' }],
        models: 'claude-sonnet-5,claude-opus-5',
      },
    ],
  })
  const rates = collectGroupRates({
    data: [{ group_id: 'legacy', user_rate_multiplier: 0.2 }],
  })

  assert.deepEqual(modelsByGroup.get('legacy'), [
    'claude-sonnet-5',
    'claude-opus-5',
  ])
  assert.equal(rates.get('legacy').user_rate_multiplier, 0.2)
})
