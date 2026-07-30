import type { ChildProcessWithoutNullStreams } from 'node:child_process'

import {
  WINDOWS_JOB_MAXIMUM_DEADLINE_MS,
  WINDOWS_JOB_MAXIMUM_TERMINATION_GRACE_MS,
  WINDOWS_JOB_POST_KILL_LEASE_MS,
  WINDOWS_JOB_WATCHDOG_SLACK_MS,
  errorMessage,
  requireBoundedPositiveInteger,
} from './contract.ts'

export type WindowsJobHelperKillOutcome = 'not-attempted' | 'accepted' | 'rejected' | 'threw'

export interface WindowsJobHelperTerminal {
  readonly code: number | null
  readonly signal: NodeJS.Signals | null
  readonly spawnError: unknown
  readonly watchdogExpired: boolean
  readonly postKillLeaseExpired: boolean
  readonly postKillLeaseMs: number
  readonly killOutcome: WindowsJobHelperKillOutcome
  readonly killError: unknown
  readonly handleReleaseErrors: readonly string[]
}

export interface WindowsJobHelperLeaseTarget {
  readonly onError: (listener: (cause: unknown) => void) => () => void
  readonly onClose: (
    listener: (code: number | null, signal: NodeJS.Signals | null) => void,
  ) => () => void
  readonly kill: () => boolean
  readonly releaseHandles: () => readonly string[]
}

export interface WindowsJobHelperLeaseClock {
  readonly setReferencedTimeout: (callback: () => void, delayMs: number) => unknown
  readonly clearTimeout: (handle: unknown) => void
}

export interface WindowsJobHelperLease {
  readonly terminal: Promise<WindowsJobHelperTerminal>
  readonly terminateRejectedStart: () => void
}

const SYSTEM_WINDOWS_JOB_LEASE_CLOCK: WindowsJobHelperLeaseClock = Object.freeze({
  // Node timers are referenced by default. The watchdog is authority, so allowing
  // it to disappear merely because no other handle is live would reopen the hang.
  setReferencedTimeout: (callback: () => void, delayMs: number) => {
    const timer = setTimeout(callback, delayMs)
    timer.ref()
    return timer
  },
  clearTimeout: (handle: unknown) => clearTimeout(handle as ReturnType<typeof setTimeout>),
})

export function createWindowsJobHelperLease(
  target: WindowsJobHelperLeaseTarget,
  watchdogMs: number,
  dependencies: {
    readonly clock?: WindowsJobHelperLeaseClock
    readonly postKillLeaseMs?: number
  } = {},
): WindowsJobHelperLease {
  requireBoundedPositiveInteger(
    watchdogMs,
    WINDOWS_JOB_MAXIMUM_DEADLINE_MS + WINDOWS_JOB_MAXIMUM_TERMINATION_GRACE_MS
      + WINDOWS_JOB_WATCHDOG_SLACK_MS,
    'Windows Job authority watchdog',
  )
  const postKillLeaseMs = dependencies.postKillLeaseMs ?? WINDOWS_JOB_POST_KILL_LEASE_MS
  requireBoundedPositiveInteger(
    postKillLeaseMs,
    WINDOWS_JOB_MAXIMUM_TERMINATION_GRACE_MS,
    'Windows Job post-kill lease',
  )
  const clock = dependencies.clock ?? SYSTEM_WINDOWS_JOB_LEASE_CLOCK
  let spawnError: unknown
  let watchdogExpired = false
  let killOutcome: WindowsJobHelperKillOutcome = 'not-attempted'
  let killError: unknown
  let handleReleaseErrors: readonly string[] = Object.freeze([])
  let settled = false
  let terminationStarted = false
  let postKillLeaseExpiring = false
  let watchdog: unknown
  let postKillLease: unknown
  let stopObservingError: () => void = () => undefined
  let stopObservingClose: () => void = () => undefined
  let resolveTerminal!: (terminal: WindowsJobHelperTerminal) => void
  const terminal = new Promise<WindowsJobHelperTerminal>((resolve) => {
    resolveTerminal = resolve
  })

  const clearLeaseTimers = () => {
    if (watchdog !== undefined) {
      clock.clearTimeout(watchdog)
      watchdog = undefined
    }
    if (postKillLease !== undefined) {
      clock.clearTimeout(postKillLease)
      postKillLease = undefined
    }
  }
  const settle = (
    code: number | null,
    signal: NodeJS.Signals | null,
    postKillLeaseExpired: boolean,
  ) => {
    if (settled) return
    settled = true
    clearLeaseTimers()
    stopObservingError()
    stopObservingClose()
    resolveTerminal(Object.freeze({
      code,
      signal,
      spawnError,
      watchdogExpired,
      postKillLeaseExpired,
      postKillLeaseMs,
      killOutcome,
      killError,
      handleReleaseErrors,
    }))
  }
  const forceTermination = (watchdogTriggered: boolean) => {
    if (settled || terminationStarted) return
    terminationStarted = true
    watchdogExpired = watchdogTriggered
    if (watchdog !== undefined) {
      clock.clearTimeout(watchdog)
      watchdog = undefined
    }
    try {
      killOutcome = target.kill() ? 'accepted' : 'rejected'
    } catch (cause) {
      killOutcome = 'threw'
      killError = cause
    }
    if (settled) return
    // kill() is only a request. A second referenced lease ensures a missing close
    // event cannot retain Node's stdio and child-process handles indefinitely.
    postKillLease = clock.setReferencedTimeout(() => {
      postKillLeaseExpiring = true
      try {
        handleReleaseErrors = Object.freeze([...target.releaseHandles()])
      } catch (cause) {
        handleReleaseErrors = Object.freeze([
          `helper handle release threw: ${errorMessage(cause)}`,
        ])
      }
      settle(null, null, true)
    }, postKillLeaseMs)
  }

  stopObservingError = target.onError((cause) => {
    // ChildProcess also uses "error" for a failed kill request. Once forced
    // termination starts, that event describes cleanup rather than spawn authority.
    if (terminationStarted) {
      killError ??= cause
      return
    }
    spawnError = cause
    forceTermination(false)
  })
  stopObservingClose = target.onClose((code, signal) => {
    if (!postKillLeaseExpiring) settle(code, signal, false)
  })
  watchdog = clock.setReferencedTimeout(() => forceTermination(true), watchdogMs)

  return Object.freeze({
    terminal,
    terminateRejectedStart: () => forceTermination(false),
  })
}

export function windowsJobHelperLeaseTarget(
  helper: ChildProcessWithoutNullStreams,
): WindowsJobHelperLeaseTarget {
  return Object.freeze({
    onError(listener: (cause: unknown) => void) {
      helper.once('error', listener)
      return () => helper.off('error', listener)
    },
    onClose(listener: (code: number | null, signal: NodeJS.Signals | null) => void) {
      helper.once('close', listener)
      return () => helper.off('close', listener)
    },
    kill: () => helper.kill('SIGKILL'),
    releaseHandles: () => releaseWindowsJobHelperHandles(helper),
  })
}

export function windowsJobHelperFailureMessage(terminal: WindowsJobHelperTerminal): string {
  if (terminal.spawnError !== undefined) {
    const detail = terminal.spawnError instanceof Error
      ? terminal.spawnError.message
      : String(terminal.spawnError)
    const failure = `Windows Job helper failed to spawn: ${detail}`
    return terminal.postKillLeaseExpired
      ? `${failure}; ${windowsJobPostKillLeaseFailureMessage(terminal)}`
      : failure
  }
  if (terminal.postKillLeaseExpired) {
    const trigger = terminal.watchdogExpired
      ? 'Windows Job helper exceeded its authority watchdog'
      : 'Windows Job helper termination did not complete'
    return `${trigger}; ${windowsJobPostKillLeaseFailureMessage(terminal)}`
  }
  if (terminal.watchdogExpired) {
    return `Windows Job helper exceeded its authority watchdog; SIGKILL was ${terminal.killOutcome}`
  }
  if (terminal.signal !== null) return `Windows Job helper terminated by ${terminal.signal}`
  return `Windows Job helper exited without authority (code ${String(terminal.code)})`
}

function releaseWindowsJobHelperHandles(
  helper: ChildProcessWithoutNullStreams,
): readonly string[] {
  const failures: string[] = []
  for (const [label, release] of [
    ['stdin', () => helper.stdin.destroy()],
    ['stdout', () => helper.stdout.destroy()],
    ['stderr', () => helper.stderr.destroy()],
    ['process', () => helper.unref()],
  ] as const) {
    try {
      release()
    } catch (cause) {
      failures.push(`${label} handle: ${errorMessage(cause)}`)
    }
  }
  return Object.freeze(failures)
}

function windowsJobPostKillLeaseFailureMessage(terminal: WindowsJobHelperTerminal): string {
  return `${windowsJobHelperKillMessage(terminal)}; helper did not close within `
    + `${terminal.postKillLeaseMs} ms; ${windowsJobHelperHandleReleaseMessage(terminal)}`
}

function windowsJobHelperKillMessage(terminal: WindowsJobHelperTerminal): string {
  if (terminal.killOutcome === 'accepted') return 'SIGKILL was accepted'
  if (terminal.killOutcome === 'rejected') {
    return terminal.killError === undefined
      ? 'SIGKILL was rejected'
      : `SIGKILL was rejected: ${errorMessage(terminal.killError)}`
  }
  if (terminal.killOutcome === 'threw') {
    return `SIGKILL threw: ${errorMessage(terminal.killError)}`
  }
  return 'SIGKILL was not attempted'
}

function windowsJobHelperHandleReleaseMessage(terminal: WindowsJobHelperTerminal): string {
  if (terminal.handleReleaseErrors.length === 0) {
    return 'stdio and process handles were released'
  }
  return `handle release reported ${terminal.handleReleaseErrors.join('; ')}`
}
