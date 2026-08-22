import { describe, expect, it } from 'vitest'
import { effectiveLabels, emptyPool, scaled, type Pool } from './api'

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

// A pool has a kind — fixed, or autoscaling between two bounds — and stepping
// it from a list row must never change that kind. Someone reaching for one more
// runner is not asking to switch the autoscaler off.
describe('scaled', () => {
  const pool = (minReplicas: number, maxReplicas: number): Pool =>
    ({ ...emptyPool(1), id: 1, minReplicas, maxReplicas }) as Pool

  it('moves both bounds of a fixed pool, so it stays fixed', () => {
    expect(scaled(pool(3, 3), 1)).toMatchObject({ minReplicas: 4, maxReplicas: 4 })
    expect(scaled(pool(3, 3), -1)).toMatchObject({ minReplicas: 2, maxReplicas: 2 })
  })

  it('moves only the ceiling of an autoscaling pool', () => {
    // The floor is what the operator guaranteed; the ceiling is how far the
    // autoscaler may go, and that is what "scale this up" is asking about.
    expect(scaled(pool(2, 5), 1)).toMatchObject({ minReplicas: 2, maxReplicas: 6 })
    expect(scaled(pool(2, 5), -1)).toMatchObject({ minReplicas: 2, maxReplicas: 4 })
  })

  it('will not shrink an autoscaling pool onto its floor', () => {
    // 2–3 stepped down would be 2–2, which is a fixed pool wearing the same
    // numbers. Turning the autoscaler off is an editor decision.
    expect(scaled(pool(2, 3), -1)).toBeNull()
  })

  it('will not take the last runner away', () => {
    // An empty pool cannot accept a job, so it can never discover that it needs
    // to grow. Stopping a pool is what the enabled switch is for.
    expect(scaled(pool(1, 1), -1)).toBeNull()
  })

  it('stops at the ceiling the daemon would refuse anyway', () => {
    expect(scaled(pool(1, 64), 1)).toBeNull()
    expect(scaled(pool(64, 64), 1)).toBeNull()
  })

  it('leaves the rest of the pool alone', () => {
    const before = pool(1, 4)
    expect(scaled(before, 1)).toEqual({ ...before, maxReplicas: 5 })
  })
})
