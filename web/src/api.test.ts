import { describe, expect, it } from 'vitest'
import { effectiveLabels, emptyPool } from './api'

describe('effectiveLabels', () => {
  it('describes what the runner actually is', () => {
    expect(effectiveLabels({ runtime: 'container', nested: true, ephemeral: true, labels: ['gpu'] }))
      .toEqual(['container', 'nestedvirt', 'ephemeral', 'gpu'])
  })

  it('follows the settings rather than the name', () => {
    // A workflow asking for "nestedvirt" must not reach a pool that only calls
    // itself that.
    expect(effectiveLabels({ runtime: 'vm', nested: false })).toEqual(['vm'])
  })

  it('keeps an automatic label once when it is also typed by hand', () => {
    expect(effectiveLabels({ runtime: 'vm', labels: ['VM', 'custom'] })).toEqual(['vm', 'custom'])
  })

  it('matches the daemon for a default pool', () => {
    // The editor shows this list before saving, so it has to agree with what
    // the daemon will do afterwards.
    expect(effectiveLabels(emptyPool(1))).toEqual(['vm', 'ephemeral'])
  })
})
