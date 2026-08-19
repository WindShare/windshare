import { describe, expect, it } from 'vitest'

import { encodeBase64Url } from '../../../src/crypto/bytes'
import { createSelectionSpec } from '../../../src/transfer/intent'
import {
  RetryableProjectionDiscoveryError,
  SelectionProjectionController,
  discoverAuthenticatedSelection,
  retryAuthenticatedSelectionDiscovery,
  type AuthenticatedDiscoveryCompletion,
  type AuthenticatedDiscoverySource,
  type AuthenticatedProjectionEvidence,
  type ProjectionTraceEvent,
  type SelectionProjectionState,
} from '../../../src/transfer/projection'

const NO_EVIDENCE: readonly AuthenticatedProjectionEvidence[] = Object.freeze([])

describe('authenticated projection discovery coordinator', () => {
  it('keeps retry as DiscoveryState while preserving the same projection epoch', async () => {
    const controller = new SelectionProjectionController()
    controller.beginSelection(await emptySelection())
    const epoch = controller.state.projection.epoch
    const failedStates = await collect(discoverAuthenticatedSelection(
      controller,
      retryableSource('catalog-temporarily-unavailable'),
      new AbortController().signal,
    ))

    expect(failedStates.map((state) => state.discovery.kind)).toEqual([
      'discovering',
      'retryable-failure',
    ])
    expect(controller.state.projection.epoch).toBe(epoch)
    const retained = controller.state.projection

    const retryStates = await collect(retryAuthenticatedSelectionDiscovery(
      controller,
      completingSource(),
      new AbortController().signal,
    ))
    expect(retryStates.map((state) => state.discovery.kind)).toEqual(['discovering', 'complete'])
    expect(controller.state.projection.epoch).toBe(epoch)
    expect(controller.state.projection.metrics).toBe(retained.metrics)
    expect(controller.state.projection.proof.kind).toBe('none')
  })

  it('fences an in-flight source when a selection mutation starts a new epoch', async () => {
    const traces: ProjectionTraceEvent[] = []
    const controller = new SelectionProjectionController({ current: event => { traces.push(event) } })
    const spec = await emptySelection()
    const first = controller.beginSelection(spec)
    const gate = deferred<void>()
    const run = discoverAuthenticatedSelection(
      controller,
      gatedSource(gate.promise),
      new AbortController().signal,
    )
    const started = await run.next()
    expect(started.value.discovery.kind).toBe('discovering')
    const oldCompletion = run.next()

    const current = controller.beginSelection(spec)
    gate.resolve()
    const fenced = await oldCompletion
    expect(fenced.done).toBe(true)
    expect(fenced.value).toBe(current)
    expect(controller.state).toBe(current)
    expect(controller.state.discovery.kind).toBe('idle')
    expect(traces.at(-1)).toMatchObject({
      name: 'projection_transition',
      transition: 'stale_event_dropped',
      currentProjectionEpoch: current.projection.epoch,
      staleProjectionEpoch: first.projection.epoch,
      eventClass: 'discovery_result',
    })
  })
})

function retryableSource(
  reason: RetryableProjectionDiscoveryError['reason'],
): AuthenticatedDiscoverySource {
  return Object.freeze({
    discover: () => failedDiscovery(reason),
  })
}

function completingSource(): AuthenticatedDiscoverySource {
  return Object.freeze({
    discover: () => completedDiscovery(),
  })
}

function gatedSource(gate: Promise<void>): AuthenticatedDiscoverySource {
  return Object.freeze({
    discover: () => gatedDiscovery(gate),
  })
}

async function* failedDiscovery(
  reason: RetryableProjectionDiscoveryError['reason'],
): AsyncGenerator<AuthenticatedProjectionEvidence, AuthenticatedDiscoveryCompletion> {
  yield* NO_EVIDENCE
  throw new RetryableProjectionDiscoveryError(reason)
}

async function* completedDiscovery(): AsyncGenerator<
  AuthenticatedProjectionEvidence,
  AuthenticatedDiscoveryCompletion
> {
  yield* NO_EVIDENCE
  return Object.freeze({})
}

async function* gatedDiscovery(gate: Promise<void>): AsyncGenerator<
  AuthenticatedProjectionEvidence,
  AuthenticatedDiscoveryCompletion
> {
  yield* NO_EVIDENCE
  await gate
  return Object.freeze({})
}

async function collect(
  states: AsyncGenerator<SelectionProjectionState, SelectionProjectionState>,
): Promise<readonly SelectionProjectionState[]> {
  const collected: SelectionProjectionState[] = []
  for await (const state of states) collected.push(state)
  return collected
}

async function emptySelection() {
  return createSelectionSpec({
    shareInstance: identity(1),
    syntheticRoot: identity(2),
    rules: { mode: 'node-id', defaultSelected: false, rules: [] },
  })
}

function identity(seed: number): string {
  const bytes = new Uint8Array(16)
  new DataView(bytes.buffer).setUint32(12, seed, false)
  return encodeBase64Url(bytes)
}

function deferred<T>(): Readonly<{
  promise: Promise<T>
  resolve: (value: T | PromiseLike<T>) => void
}> {
  let resolve: (value: T | PromiseLike<T>) => void = () => undefined
  const promise = new Promise<T>((complete) => { resolve = complete })
  return Object.freeze({ promise, resolve })
}
