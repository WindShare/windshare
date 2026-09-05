import { describe, expect, it } from 'vitest'
import { PeerAttemptBudget } from '../../src/connectivity/peer-set/budget'
import { selectedPairFact } from '../../src/connectivity/peer-set/provider-facts'

describe('shared peer admission budget', () => {
  it('reserves elapsed capacity atomically and refunds unused time once', () => {
    const budget = new PeerAttemptBudget(2, 100, 1000)
    expect(budget.takeAttempt(0)).toBe(true)
    expect(budget.takeAttempt(0)).toBe(true)
    expect(budget.takeAttempt(0)).toBe(false)
    const first = budget.reserveElapsed(0, 80)
    const second = budget.reserveElapsed(0, 80)
    expect(second.milliseconds).toBe(20)
    first.release(20, 0)
    first.release(0, 0)
    expect(budget.available(0)).toEqual({ attempts: 0, elapsedMilliseconds: 60 })
    expect(budget.nextAttemptDelay(0)).toBe(500)
    expect(budget.available(500)).toEqual({ attempts: 1, elapsedMilliseconds: 100 })
    expect(() => budget.available(499)).toThrow(/monotonic/)
  })
})

describe('selected pair classification', () => {
  it('keeps TURN distinct and reports pair RTT without inventing STUN facts', () => {
    const stats = new Map([
      ['transport', { id: 'transport', type: 'transport', selectedCandidatePairId: 'pair' }],
      ['pair', { id: 'pair', type: 'candidate-pair', localCandidateId: 'local', remoteCandidateId: 'remote', currentRoundTripTime: 0.025 }],
      ['local', { id: 'local', candidateType: 'relay', protocol: 'udp', address: '2001:db8::1', usernameFragment: 'private-credential' }],
      ['remote', { id: 'remote', candidateType: 'srflx' }],
    ]) as unknown as RTCStatsReport
    const selected = selectedPairFact(stats, undefined, 0, 100)
    expect(selected?.fact).toMatchObject({ route: 'turn', rttMs: 25, family: 'ipv6', ageMs: 0 })
    expect(JSON.stringify(selected)).not.toContain('private-credential')
    expect(selectedPairFact(stats, 'pair', 100, 150)?.fact.ageMs).toBe(50)
    expect(selectedPairFact(new Map() as unknown as RTCStatsReport, undefined, 0, 0)).toBeUndefined()
  })
})
