import assert from 'node:assert/strict'
import test from 'node:test'

import {
  asArray,
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
