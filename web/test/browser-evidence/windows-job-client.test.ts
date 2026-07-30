import { describe, expect, it } from 'vitest'

import {
  canonicalWindowsJobEnvironment,
  canonicalWindowsJobStdinMetadata,
  createWindowsJobHelperLease,
  deliverAndEraseWindowsJobRawInput,
  executeWindowsJob,
  parseWindowsJobAuthorityStatus,
  windowsJobSupervisorEnvironment,
  windowsJobHelperFailureMessage,
  type WindowsJobHelperLeaseClock,
  type WindowsJobHelperLeaseTarget,
} from '../../scripts/browser-evidence/process/windows-job-client.ts'

const OPERATION_ID = 'run/main/chromium/1'
const NONCE = 'a'.repeat(64)

describe('Windows Job client contract', () => {
  it('serializes only bounded nonsecret authority for the anonymous raw stdin channel', () => {
    const authority = {
      channelId: 'channel',
      runId: 'run',
      profileId: 'profile',
      attemptId: 'attempt',
    }
    const metadata = canonicalWindowsJobStdinMetadata(3, authority)
    expect(metadata).toEqual({
      kind: 'anonymous-pipe',
      descriptor: 0,
      byteLength: 3,
      ...authority,
    })
    expect(JSON.stringify(metadata)).not.toMatch(/AQID|base64|digest|sha256/iu)
    expect(() => canonicalWindowsJobStdinMetadata(0, authority)).toThrow(/\[1,/u)
    expect(() => canonicalWindowsJobStdinMetadata(1, {
      ...authority,
      attemptId: 'not portable',
    })).toThrow(/portable/u)
  })

  it('erases the raw buffer when synchronous pipe delivery fails', async () => {
    const raw = Buffer.from([1, 2, 3])
    const pipe = {
      once: () => pipe,
      end: () => { throw new Error('injected synchronous delivery failure') },
    }

    await expect(deliverAndEraseWindowsJobRawInput(pipe, raw)).rejects.toThrow(
      /injected synchronous delivery failure/u,
    )
    expect([...raw]).toEqual([0, 0, 0])
  })

  it('erases caller stdin even when preflight rejects before spawn', async () => {
    const raw = Uint8Array.from([4, 5, 6])
    await expect(executeWindowsJob({
      helperPath: 'not-absolute.exe',
      operationId: OPERATION_ID,
      command: {
        executable: 'also-not-absolute.exe',
        arguments: [],
        stdin: raw,
        stdinAuthority: {
          channelId: 'channel',
          runId: 'run',
          profileId: 'profile',
          attemptId: 'attempt',
        },
      },
      inheritedEnvironment: {},
      injectedEnvironment: {},
      deadlineMs: 1,
      terminationGraceMs: 1,
      stdout: () => undefined,
      stderr: () => undefined,
    })).rejects.toThrow(/absolute canonical Windows path/u)
    expect([...raw]).toEqual([0, 0, 0])
  })

  it('merges environment layers with Windows identity and deterministic wire ordering', () => {
    const environment = canonicalWindowsJobEnvironment(
      {
        Path: 'inherited',
        ZED: 'z',
        UNSET: undefined,
        Ä: 'upper',
      },
      {
        PATH: 'command',
        alpha: 'a',
        ä: 'lower',
      },
      {
        path: 'injected',
        BETA: 'b',
      },
    )

    expect(environment).toEqual([
      { name: 'alpha', value: 'a' },
      { name: 'BETA', value: 'b' },
      { name: 'path', value: 'injected' },
      { name: 'ZED', value: 'z' },
      { name: 'ä', value: 'lower' },
    ])
    expect(() => canonicalWindowsJobEnvironment({ 'BAD=NAME': 'value' }, undefined, {}))
      .toThrow(/invalid name/u)
    expect(() => canonicalWindowsJobEnvironment({ GOOD: 'bad\0value' }, undefined, {}))
      .toThrow(/invalid value/u)
  })

  it('gives the supervisor no ambient environment without changing target serialization', () => {
    expect(windowsJobSupervisorEnvironment()).toEqual({})

    expect(canonicalWindowsJobEnvironment(
      {},
      { SYNTHETIC_TARGET_INPUT: 'target-value' },
      { WINDSHARE_BROWSER_EVIDENCE_CONTEXT: '{"sample":1}' },
    )).toEqual([
      { name: 'SYNTHETIC_TARGET_INPUT', value: 'target-value' },
      { name: 'WINDSHARE_BROWSER_EVIDENCE_CONTEXT', value: '{"sample":1}' },
    ])
  })

  it('accepts only exact canonical authenticated zero-process authority', () => {
    const status = canonicalStatus({
      root: { pid: 42, exitCode: 0xffff_ffff },
    })
    const parsed = parseWindowsJobAuthorityStatus(
      new TextEncoder().encode(JSON.stringify(status)),
      OPERATION_ID,
      NONCE,
      false,
    )

    expect(parsed).toEqual(status)
    expect(parsed.root?.exitCode).toBe(0xffff_ffff)
  })

  it('rejects formatting, identity, field-order, and lifecycle contradictions', () => {
    const status = canonicalStatus({ root: { pid: 42, exitCode: 0 } })
    const encoded = JSON.stringify(status)
    const { schemaVersion, operationId, ...remainingStatus } = status
    const reorderedStatus = { operationId, schemaVersion, ...remainingStatus }

    expect(() => parseWindowsJobAuthorityStatus(
      new TextEncoder().encode(` ${encoded}`),
      OPERATION_ID,
      NONCE,
      false,
    )).toThrow(/exact canonical JSON/u)
    expect(() => parseWindowsJobAuthorityStatus(
      new TextEncoder().encode(JSON.stringify(reorderedStatus)),
      OPERATION_ID,
      NONCE,
      false,
    )).toThrow(/canonical order/u)
    expect(() => parseWindowsJobAuthorityStatus(
      new TextEncoder().encode(encoded),
      'another-operation',
      NONCE,
      false,
    )).toThrow(/identity/u)
    expect(() => parseWindowsJobAuthorityStatus(
      new TextEncoder().encode(JSON.stringify(canonicalStatus({
        timedOut: true,
        root: { pid: 42, exitCode: 0 },
      }))),
      OPERATION_ID,
      NONCE,
      false,
    )).toThrow(/deadline reason/u)
    expect(() => parseWindowsJobAuthorityStatus(
      new TextEncoder().encode(JSON.stringify(canonicalStatus({
        activeProcessCount: 1,
        root: { pid: 42, exitCode: 0 },
      }))),
      OPERATION_ID,
      NONCE,
      false,
    )).toThrow(/active process count/u)
  })

  it('models target spawn failure without inventing a root process', () => {
    const status = canonicalStatus({
      supervisionOutcome: 'spawn-failed',
      terminationReason: 'target-spawn-failed',
      inputOutcome: 'not-started',
      root: null,
      spawnFailure: 'CreateProcess failed',
    })
    expect(parseWindowsJobAuthorityStatus(
      new TextEncoder().encode(JSON.stringify(status)),
      OPERATION_ID,
      NONCE,
      false,
    )).toEqual(status)
  })

  it.each([
    {
      name: 'rejects SIGKILL',
      kill: () => false,
      expected: /SIGKILL was rejected/u,
    },
    {
      name: 'throws while requesting SIGKILL',
      kill: () => { throw new Error('injected kill failure') },
      expected: /SIGKILL threw: injected kill failure/u,
    },
  ])('bounds a watchdog lease when a helper $name and never closes', async ({ kill, expected }) => {
    const clock = new ManualLeaseClock()
    const target = new FakeLeaseTarget(kill)
    const lease = createWindowsJobHelperLease(target, 100, {
      clock,
      postKillLeaseMs: 25,
    })
    let settled = false
    const observeSettlement = lease.terminal.then(() => { settled = true })

    clock.advanceBy(100)
    await Promise.resolve()
    expect(target.killCount).toBe(1)
    expect(settled).toBe(false)
    // The watchdog and cleanup lease remain scheduled authority; neither timer
    // exposes an unref escape hatch to the supervisor.
    expect(clock.pendingDelays()).toEqual([25])

    clock.advanceBy(24)
    await Promise.resolve()
    expect(settled).toBe(false)
    clock.advanceBy(1)
    const terminal = await lease.terminal
    await observeSettlement

    expect(terminal).toMatchObject({
      code: null,
      signal: null,
      watchdogExpired: true,
      postKillLeaseExpired: true,
      postKillLeaseMs: 25,
    })
    expect(target.releaseCount).toBe(1)
    expect(windowsJobHelperFailureMessage(terminal)).toMatch(expected)
    expect(windowsJobHelperFailureMessage(terminal)).toMatch(
      /helper did not close within 25 ms; stdio and process handles were released/u,
    )
  })

  it('cancels both leases when close arrives after watchdog termination', async () => {
    const clock = new ManualLeaseClock()
    const target = new FakeLeaseTarget(() => true)
    const lease = createWindowsJobHelperLease(target, 100, {
      clock,
      postKillLeaseMs: 25,
    })

    clock.advanceBy(100)
    target.close(null, 'SIGKILL')
    const terminal = await lease.terminal

    expect(terminal).toMatchObject({
      signal: 'SIGKILL',
      watchdogExpired: true,
      postKillLeaseExpired: false,
      killOutcome: 'accepted',
    })
    expect(target.releaseCount).toBe(0)
    expect(clock.pendingDelays()).toEqual([])
  })

  it('classifies an error emitted by a rejected kill as cleanup evidence', async () => {
    const clock = new ManualLeaseClock()
    const target = new FakeLeaseTarget((leaseTarget) => {
      leaseTarget.error(new Error('injected kill event'))
      return false
    })
    const lease = createWindowsJobHelperLease(target, 100, {
      clock,
      postKillLeaseMs: 25,
    })

    clock.advanceBy(100)
    clock.advanceBy(25)
    const terminal = await lease.terminal

    expect(terminal.spawnError).toBeUndefined()
    expect(terminal.killError).toMatchObject({ message: 'injected kill event' })
    expect(windowsJobHelperFailureMessage(terminal)).toMatch(
      /authority watchdog; SIGKILL was rejected: injected kill event/u,
    )
  })

  it('bounds rejected-start cleanup without waiting for the watchdog', async () => {
    const clock = new ManualLeaseClock()
    const target = new FakeLeaseTarget(() => false)
    const lease = createWindowsJobHelperLease(target, 1_000, {
      clock,
      postKillLeaseMs: 25,
    })

    lease.terminateRejectedStart()
    expect(target.killCount).toBe(1)
    expect(clock.pendingDelays()).toEqual([25])
    clock.advanceBy(25)
    const terminal = await lease.terminal

    expect(terminal).toMatchObject({
      watchdogExpired: false,
      postKillLeaseExpired: true,
      killOutcome: 'rejected',
    })
    expect(windowsJobHelperFailureMessage(terminal)).toMatch(
      /helper termination did not complete; SIGKILL was rejected/u,
    )
  })
})

interface ScheduledLeaseTimer {
  readonly id: number
  readonly dueAt: number
  readonly callback: () => void
  cancelled: boolean
}

class ManualLeaseClock implements WindowsJobHelperLeaseClock {
  readonly #timers: ScheduledLeaseTimer[] = []
  #now = 0
  #nextId = 1

  readonly setReferencedTimeout = (
    callback: () => void,
    delayMs: number,
  ): ScheduledLeaseTimer => {
    const timer = { id: this.#nextId, dueAt: this.#now + delayMs, callback, cancelled: false }
    this.#nextId += 1
    this.#timers.push(timer)
    return timer
  }

  readonly clearTimeout = (handle: unknown): void => {
    const timer = handle as ScheduledLeaseTimer
    timer.cancelled = true
  }

  advanceBy(milliseconds: number): void {
    this.#now += milliseconds
    while (true) {
      const timer = this.#timers
        .filter((candidate) => !candidate.cancelled && candidate.dueAt <= this.#now)
        .sort((left, right) => left.dueAt - right.dueAt || left.id - right.id)[0]
      if (timer === undefined) return
      timer.cancelled = true
      timer.callback()
    }
  }

  pendingDelays(): readonly number[] {
    return this.#timers
      .filter((timer) => !timer.cancelled)
      .map((timer) => timer.dueAt - this.#now)
      .sort((left, right) => left - right)
  }
}

class FakeLeaseTarget implements WindowsJobHelperLeaseTarget {
  #errorListener: ((cause: unknown) => void) | undefined
  #closeListener: ((code: number | null, signal: NodeJS.Signals | null) => void) | undefined
  readonly #kill: (target: FakeLeaseTarget) => boolean
  killCount = 0
  releaseCount = 0

  constructor(kill: (target: FakeLeaseTarget) => boolean) {
    this.#kill = kill
  }

  readonly onError = (listener: (cause: unknown) => void): (() => void) => {
    this.#errorListener = listener
    return () => {
      if (this.#errorListener === listener) this.#errorListener = undefined
    }
  }

  readonly onClose = (
    listener: (code: number | null, signal: NodeJS.Signals | null) => void,
  ): (() => void) => {
    this.#closeListener = listener
    return () => {
      if (this.#closeListener === listener) this.#closeListener = undefined
    }
  }

  readonly kill = (): boolean => {
    this.killCount += 1
    return this.#kill(this)
  }

  readonly releaseHandles = (): readonly string[] => {
    this.releaseCount += 1
    return Object.freeze([])
  }

  close(code: number | null, signal: NodeJS.Signals | null): void {
    this.#closeListener?.(code, signal)
  }

  error(cause: unknown): void {
    this.#errorListener?.(cause)
  }
}

function canonicalStatus(overrides: Readonly<Record<string, unknown>> = {}) {
  return {
    schemaVersion: 2,
    operationId: OPERATION_ID,
    nonce: NONCE,
    supervisionOutcome: 'tree-empty',
    terminationReason: 'natural',
    timedOut: false,
    activeProcessCount: 0,
    inputOutcome: 'not-requested',
    root: null,
    spawnFailure: null,
    ...overrides,
  }
}
