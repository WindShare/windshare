import { mkdir } from 'node:fs/promises'
import { dirname, resolve } from 'node:path'

import type { Page, TestInfo } from '@playwright/test'

import type { AttemptEvidence } from '../../scripts/browser-evidence/attempt-evidence'
import {
  CHILD_EVIDENCE_CONTEXT_ENV,
  ChildEvidenceReporter,
  publicBrowserDiagnosticSink,
} from '../../scripts/browser-evidence/child-evidence'
import {
  acquireWholeSampleResource,
  WholeSampleDeadline,
  WholeSampleDeadlineExpiredError,
} from './hot-switch-evidence'
import {
  FixtureInfrastructureError,
  containsFixtureInfrastructureFailure,
} from './managed-process'
import {
  acquireRealStackBinaries,
  releaseRealStackBinaries,
} from './v2-real-stack'

const HOT_SWITCH_TRACE_RELATIVE_PATH = 'main/hot-switch-trace.zip'

export interface PromiseSettlement<T> {
  readonly value?: T
  readonly error?: unknown
}

export interface RegisteredLateCleanup {
  readonly boundary: string
  readonly settlement: Promise<PromiseSettlement<unknown>>
}

export type RegisterLateCleanup = (boundary: string, task: Promise<unknown>) => void

export class IntermediateEvidenceGate {
  readonly #reporter: ChildEvidenceReporter | null
  #open = true

  constructor(reporter: ChildEvidenceReporter | null) {
    this.#reporter = reporter
  }

  close(): void {
    this.#open = false
  }

  publish(operation: (reporter: ChildEvidenceReporter) => void): void {
    if (!this.#open || this.#reporter === null) return
    operation(this.#reporter)
  }

  recordAttempt(evidence: AttemptEvidence): void {
    this.publish((reporter) => reporter.recordAttempt(evidence))
  }
}

export async function releaseOwnedBinaries(
  deadline: WholeSampleDeadline,
  binaries: Awaited<ReturnType<typeof acquireRealStackBinaries>> | undefined,
): Promise<readonly unknown[]> {
  if (binaries === undefined) return []
  try {
    await fixtureCleanup(
      deadline,
      'Real-stack binary release failed',
      () => releaseRealStackBinaries(binaries),
    )
    return []
  } catch (error) {
    return [error]
  }
}

export async function drainLateCleanupTasks(
  deadline: WholeSampleDeadline,
  tasks: readonly RegisteredLateCleanup[],
): Promise<readonly unknown[]> {
  if (tasks.length === 0) return []
  try {
    const settlements = await fixtureCleanup(
      deadline,
      'Late resource rollback drain failed',
      () => Promise.all(tasks.map((task) => task.settlement)),
    )
    return settlements.flatMap((settlement, index) => {
      if (settlement.error === undefined) return []
      return [new FixtureInfrastructureError(
        tasks[index]?.boundary ?? 'Late resource rollback failed',
        settlement.error,
      )]
    })
  } catch (error) {
    return [error]
  }
}

export async function drainTimedOutPlaywrightOwners(options: {
  readonly abortCleanup: () => void
  readonly abortWork: () => void
  readonly deadline: WholeSampleDeadline
  readonly pendingContextClose: () => Promise<PromiseSettlement<void>> | undefined
  readonly pendingPageClose: () => Promise<PromiseSettlement<void>> | undefined
}): Promise<readonly unknown[]> {
  const failures: unknown[] = []
  // Work can no longer create page ownership. Keep the cleanup listener live
  // while page close drains so context close remains available at the cutoff.
  options.deadline.workSignal.removeEventListener('abort', options.abortWork)
  failures.push(...await drainForcedPlaywrightClose(
    options.deadline,
    'Timed-out receiver page close failed',
    options.pendingPageClose(),
  ))
  options.deadline.cleanupSignal.removeEventListener('abort', options.abortCleanup)
  // abortCleanup may have created this owner during the preceding await.
  failures.push(...await drainForcedPlaywrightClose(
    options.deadline,
    'Timed-out receiver context close failed',
    options.pendingContextClose(),
  ))
  return failures
}

async function drainForcedPlaywrightClose(
  deadline: WholeSampleDeadline,
  boundary: string,
  pending: Promise<PromiseSettlement<void>> | undefined,
): Promise<readonly unknown[]> {
  if (pending === undefined) return []
  try {
    const closed = await fixtureCleanup(deadline, boundary, () => pending)
    return closed.error === undefined
      ? []
      : [new FixtureInfrastructureError(boundary, closed.error)]
  } catch (error) {
    return [error]
  }
}

export async function publishChildBoundary(
  deadline: WholeSampleDeadline,
  reporter: ChildEvidenceReporter | null,
  failures: readonly unknown[],
): Promise<readonly unknown[]> {
  const publicationFailures: unknown[] = []
  const observedFailure = failures.length === 0
    ? undefined
    : aggregateFailure(failures, 'Hot-switch sample boundary failed')
  if (observedFailure !== undefined) {
    try {
      await fixturePublication(deadline, 'Child failure evidence publication failed', () => {
        if (containsFixtureInfrastructureFailure(observedFailure)) {
          reporter?.recordInfrastructureFailure(observedFailure)
        }
      })
    } catch (error) {
      publicationFailures.push(error)
    }
  }
  if (reporter !== null) {
    try {
      await fixturePublication(deadline, 'Child lifecycle publication failed', async () => {
        reporter.completeLifecycle()
        await reporter.flush()
      })
    } catch (error) {
      publicationFailures.push(error)
    }
  }
  return publicationFailures
}

export async function runWithSanitizedFailureTrace<T>(
  options: {
    readonly deadline: WholeSampleDeadline
    readonly page: Page
    readonly reporter: ChildEvidenceReporter | null
    readonly registerLateCleanup: RegisterLateCleanup
    readonly testInfo: TestInfo
    readonly beginFixtureCleanup: () => readonly Promise<unknown>[]
  },
  operation: () => Promise<T>,
): Promise<T> {
  const tracing = options.page.context().tracing
  await acquireFixtureWork(
    options.deadline,
    'Sanitized trace startup failed',
    () => tracing.start({ screenshots: true, snapshots: true, sources: true }),
    'Late sanitized trace startup rollback failed',
    () => tracing.stop(),
    options.registerLateCleanup,
  )
  let result: T | undefined
  let operationError: unknown
  let operationFailed = false
  let operationDrain: Promise<unknown> | undefined
  let operationTask: Promise<T> | undefined
  try {
    // The outer work race closes the small synchronous gaps between named
    // evidence waits, so a verdict cannot resolve just after the absolute cutoff.
    result = await options.deadline.runWork(() => {
      operationTask = Promise.resolve().then(operation)
      return operationTask
    })
  } catch (error) {
    operationFailed = true
    operationError = error instanceof WholeSampleDeadlineExpiredError
      ? new FixtureInfrastructureError(
          'Hot-switch product operation exceeded its work authority',
          error,
        )
      : error
    if (error instanceof WholeSampleDeadlineExpiredError && operationTask !== undefined) {
      const admittedOperation = operationTask
      operationDrain = fixtureCleanup(
        options.deadline,
        'Late hot-switch product operation drain failed',
        async () => {
          const late = await settle(admittedOperation)
          if (late.error !== undefined && late.error !== error) {
            throw new FixtureInfrastructureError(
              'Hot-switch product operation failed after its work cutoff',
              late.error,
            )
          }
        },
      )
    }
  }
  const ownerCleanup = options.beginFixtureCleanup()
  const traceFinalization = operationFailed
    ? fixtureCleanup(
        options.deadline,
        'Sanitized failure trace retention failed',
        async (signal) => {
          signal.throwIfAborted()
          const tracePath = options.reporter === null
            ? options.testInfo.outputPath('hot-switch-trace.zip')
            : resolve(
                options.reporter.context.artifactRoot,
                ...HOT_SWITCH_TRACE_RELATIVE_PATH.split('/'),
              )
          await mkdir(dirname(tracePath), { recursive: true })
          signal.throwIfAborted()
          await tracing.stop({ path: tracePath })
          signal.throwIfAborted()
          await options.testInfo.attach('hot-switch-trace', {
            path: tracePath,
            contentType: 'application/zip',
          })
          signal.throwIfAborted()
          options.reporter?.recordArtifact({
            kind: 'trace',
            relativePath: HOT_SWITCH_TRACE_RELATIVE_PATH,
            mediaType: 'application/zip',
          })
        },
      )
    : fixtureCleanup(
        options.deadline,
        'Sanitized trace finalization failed',
        () => tracing.stop(),
      )
  const cleanup = await Promise.allSettled([
    ...ownerCleanup,
    traceFinalization,
    ...(operationDrain === undefined ? [] : [operationDrain]),
  ])
  const cleanupErrors = cleanup.flatMap((settlement) =>
    settlement.status === 'rejected' ? [settlement.reason] : [],
  )
  if (operationFailed || cleanupErrors.length > 0) {
    throw aggregateFailure(
      [
        ...(operationFailed ? [operationError] : []),
        ...cleanupErrors,
      ],
      operationFailed
        ? 'Hot-switch sample and cleanup failed'
        : 'Hot-switch fixture cleanup failed',
    )
  }
  return result as T
}

export function optionalChildReporter(): ChildEvidenceReporter | null {
  const encoded = process.env[CHILD_EVIDENCE_CONTEXT_ENV]
  return encoded === undefined || encoded === '' ? null : new ChildEvidenceReporter()
}

export function validateReporterIdentity(
  reporter: ChildEvidenceReporter | null,
  browserName: string,
  testInfo: TestInfo,
): void {
  if (reporter === null) return
  if (reporter.context.suite !== 'main') {
    throw new Error('Product hot-switch evidence context must identify the main suite')
  }
  if (reporter.context.browser !== browserName) {
    throw new Error('Product hot-switch evidence browser differs from the Playwright project')
  }
  if (testInfo.project.retries !== 0 || testInfo.retry !== 0) {
    throw new Error('Product hot-switch evidence prohibits Playwright retries')
  }
  if (testInfo.project.repeatEach !== 1 || testInfo.repeatEachIndex !== 0) {
    throw new Error('Product hot-switch evidence requires one sample per child process')
  }
}

export function observePublicBrowserDiagnostics(
  page: Page,
  reporter: ChildEvidenceReporter | null,
  failRuntime: (error: Error) => void,
): {
  readonly close: () => void
  readonly expectTargetClose: () => void
} {
  const sink = reporter === null ? null : publicBrowserDiagnosticSink(reporter)
  const browser = page.context().browser()
  let targetCloseExpected = false
  const crashed = () => {
    const error = new Error('Playwright page crash event')
    sink?.pageCrashed(error)
    failRuntime(error)
  }
  const pageError = (error: Error) => sink?.pageError(error)
  const closed = () => {
    if (targetCloseExpected) return
    const error = new Error('Playwright page closed unexpectedly during the product sample')
    sink?.targetCrashed(error)
    failRuntime(error)
  }
  const consoleMessage = (message: { readonly type: () => string; readonly text: () => string }) => {
    sink?.console(message.type(), message.text())
  }
  const disconnected = () => {
    const error = new Error('Playwright browser disconnected during the product sample')
    sink?.browserDisconnected(false, error)
    failRuntime(error)
  }
  page.on('crash', crashed)
  page.on('close', closed)
  page.on('pageerror', pageError)
  page.on('console', consoleMessage)
  browser?.on('disconnected', disconnected)
  return Object.freeze({
    expectTargetClose: () => { targetCloseExpected = true },
    close: () => {
      page.off('crash', crashed)
      page.off('close', closed)
      page.off('pageerror', pageError)
      page.off('console', consoleMessage)
      browser?.off('disconnected', disconnected)
    },
  })
}

async function fixtureOperation<T>(
  boundary: string,
  operation: () => Promise<T>,
): Promise<T> {
  try {
    return await operation()
  } catch (cause) {
    throw new FixtureInfrastructureError(boundary, cause)
  }
}

export function fixtureWork<T>(
  deadline: WholeSampleDeadline,
  boundary: string,
  operation: (signal: AbortSignal) => T | PromiseLike<T>,
): Promise<T> {
  return fixtureOperation(boundary, () => deadline.runWork(operation))
}

export async function acquireFixtureWork<T>(
  deadline: WholeSampleDeadline,
  acquisitionBoundary: string,
  acquire: (signal: AbortSignal) => T | PromiseLike<T>,
  rollbackBoundary: string,
  rollback: (resource: T, signal: AbortSignal) => unknown | PromiseLike<unknown>,
  registerLateCleanup: RegisterLateCleanup,
): Promise<T> {
  return fixtureOperation(acquisitionBoundary, () => acquireWholeSampleResource(
    deadline,
    acquire,
    rollbackBoundary,
    rollback,
    registerLateCleanup,
  ))
}

export function fixtureCleanup<T>(
  deadline: WholeSampleDeadline,
  boundary: string,
  operation: (signal: AbortSignal) => T | PromiseLike<T>,
): Promise<T> {
  return fixtureOperation(boundary, () => deadline.runCleanup(operation))
}

function fixturePublication<T>(
  deadline: WholeSampleDeadline,
  boundary: string,
  operation: (signal: AbortSignal) => T | PromiseLike<T>,
): Promise<T> {
  return fixtureOperation(boundary, () => deadline.runPublication(operation))
}

export function fixtureValue<T>(boundary: string, operation: () => T): T {
  try {
    return operation()
  } catch (cause) {
    throw new FixtureInfrastructureError(boundary, cause)
  }
}

export async function settle<T>(promise: Promise<T>): Promise<PromiseSettlement<T>> {
  try {
    return Object.freeze({ value: await promise })
  } catch (error) {
    return Object.freeze({ error })
  }
}

export function aggregateFailure(failures: readonly unknown[], message: string): unknown {
  if (failures.length === 1) return failures[0]
  return new AggregateError(failures, message)
}
