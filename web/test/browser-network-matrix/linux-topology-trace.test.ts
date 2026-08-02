import { describe, expect, it } from 'vitest'

import {
  LinuxTopologyTraceJournal,
  requireCompleteLinuxTopologyTrace,
  type LinuxTopologyTraceIdentity,
} from '../../scripts/browser-network-matrix/linux-topology/trace/index.ts'

const IDENTITY: LinuxTopologyTraceIdentity = Object.freeze({
  component: 'contained-browser-broker',
  scenario: 'contained-browser-sample',
  operationId: 'trace-operation',
  runId: 'trace-run',
  profileId: 'scheduled-public-stun',
  browser: 'chromium',
  sampleOrdinal: 1,
})

describe('Linux topology owned trace journal', () => {
  it('retains one exact lifecycle and derives the last workflow milestone itself', () => {
    const journal = new LinuxTopologyTraceJournal()
    const identity = journal.start(IDENTITY, 'contained-browser-started')
    journal.progress(identity, 'contained-evidence-accepted', 'succeeded')
    journal.progress(identity, 'input-cleanup-settled', 'succeeded', {
      cleanupOutcome: 'completed',
    })
    journal.terminal(identity, 'contained-browser-terminal', 'succeeded', 'completed')
    journal.finish()

    const snapshot = journal.view.snapshot()
    requireCompleteLinuxTopologyTrace(snapshot)
    expect(journal.view).not.toHaveProperty('append')
    expect(snapshot.events.at(-1)).toMatchObject({
      milestone: 'contained-browser-terminal',
      outcome: 'succeeded',
      context: {
        cleanupOutcome: 'completed',
        lastMilestone: 'contained-evidence-accepted',
      },
    })
  })

  it('rejects Proxy context before traps and publishes only an opaque trace failure', () => {
    const journal = new LinuxTopologyTraceJournal()
    const identity = journal.start(IDENTITY, 'contained-browser-started')
    let trapCalls = 0
    const context = new Proxy({ secret: 'must-not-surface' }, {
      get() {
        trapCalls += 1
        throw new Error('context getter invoked')
      },
      ownKeys() {
        trapCalls += 1
        throw new Error('context ownKeys invoked')
      },
    })

    expect(() => journal.progress(identity, 'contained-process-settled', 'failed', context))
      .toThrow('rejected invalid evidence')
    expect(trapCalls).toBe(0)
    expect(() => journal.terminal(
      identity,
      'contained-browser-terminal',
      'failed',
      'completed',
    )).toThrow()
    expect(() => journal.finish()).toThrow()

    const snapshot = journal.view.snapshot()
    expect(JSON.stringify(snapshot)).not.toContain('must-not-surface')
    expect(snapshot.failure).toEqual({
      name: 'LinuxTopologyTraceError',
      message: 'Linux topology trace evidence is invalid',
    })
  })

  it('cannot publish successful terminal evidence when cleanup failed', () => {
    const journal = new LinuxTopologyTraceJournal()
    const identity = journal.start(IDENTITY, 'contained-browser-started')
    journal.progress(identity, 'contained-process-settled', 'succeeded')

    expect(() => journal.terminal(
      identity,
      'contained-browser-terminal',
      'succeeded',
      'failed',
    )).toThrow('rejected invalid evidence')
    expect(() => journal.finish()).toThrow()
    expect(journal.view.snapshot().failure).not.toBeNull()
  })

  it('fails closed when the owned event authority is exhausted', () => {
    const journal = new LinuxTopologyTraceJournal()
    const identity = journal.start(IDENTITY, 'contained-browser-started')
    for (let index = 1; index < 4_096; index += 1) {
      journal.progress(identity, 'contained-process-observed', 'succeeded', { index })
    }

    expect(() => journal.progress(
      identity,
      'contained-process-observed',
      'succeeded',
      { index: 4_096 },
    )).toThrow()
    expect(() => journal.terminal(
      identity,
      'contained-browser-terminal',
      'failed',
      'completed',
    )).toThrow()
    expect(() => journal.finish()).toThrow()

    const snapshot = journal.view.snapshot()
    expect(snapshot).toMatchObject({
      capturedEvents: 4_096,
      truncated: true,
      completed: true,
    })
    expect(snapshot.observedEvents).toBeGreaterThan(snapshot.capturedEvents)
    expect(snapshot.observedBytes).toBeGreaterThan(snapshot.capturedBytes)
    expect(snapshot.failure).not.toBeNull()
  })
})
