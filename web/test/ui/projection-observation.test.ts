import { describe, expect, it } from 'vitest'

import {
  RetryableProjectionDiscoveryError,
  type AuthenticatedDiscoveryRequest,
} from '../../src/transfer/projection'
import { SelectionProjectionRuntime } from '../../src/ui/controller/projection-observation'
import { V2ControllerObservability } from '../../src/ui/controller/controller-observability'
import type { V2JoinedBrowserShare } from '../../src/ui/v2-gateway'
import {
  FakeJoinedShare,
  FakeReceiveComposition,
  MANAGED_ENVIRONMENT,
  deferred,
  waitFor,
} from './v2-receiver-orchestration-fixture'

describe('selection projection runtime', () => {
  it('fences stale environment acquisition before publishing an observation', async () => {
    const firstEnvironment = deferred<typeof MANAGED_ENVIRONMENT>()
    const secondEnvironment = deferred<typeof MANAGED_ENVIRONMENT>()
    const receive = new FakeReceiveComposition(MANAGED_ENVIRONMENT, [
      firstEnvironment.promise,
      secondEnvironment.promise,
    ])
    const discovery = deferred<void>()
    const joined = new FakeJoinedShare(true, [discovery.promise]) as unknown as V2JoinedBrowserShare
    const observed: number[] = []
    const replacements: unknown[] = []
    const runtime = new SelectionProjectionRuntime({
      receive,
      authority: {
        observeProjection: active => { observed.push(active.revision) },
        startObservationReplacement: reason => { replacements.push(reason) },
        invalidate: () => undefined,
      },
      observability: new V2ControllerObservability({}),
      currentJoinedShare: () => joined,
      isDisposed: () => false,
      onFailure: error => { throw error },
    })

    runtime.start(joined, 'selection-change')
    runtime.start(joined, 'observation-replacement')
    secondEnvironment.resolve(MANAGED_ENVIRONMENT)
    await waitFor(() => runtime.current !== undefined)
    firstEnvironment.resolve(MANAGED_ENVIRONMENT)

    expect(observed.every(revision => revision === 2)).toBe(true)
    expect(replacements).toHaveLength(1)
    runtime.stop(new DOMException('Test completed', 'AbortError'))
  })

  it('retries discovery on the same active observation with a fresh controller', async () => {
    const receive = new FakeReceiveComposition(MANAGED_ENVIRONMENT)
    const initialDiscovery = deferred<void>()
    const retryDiscovery = deferred<void>()
    const joinedFixture = new FakeJoinedShare(true, [
      initialDiscovery.promise,
      retryDiscovery.promise,
    ])
    const initialProjectionSource = joinedFixture.projectionSource.bind(joinedFixture)
    let sourceCalls = 0
    joinedFixture.projectionSource = selection => {
      sourceCalls += 1
      if (sourceCalls === 1) return initialProjectionSource(selection)
      const retrySource = initialProjectionSource(selection)
      return Object.freeze({
        discover: async function* (request: AuthenticatedDiscoveryRequest) {
          const iterator = retrySource.discover(request)
          while (true) {
            const step = await iterator.next()
            if (step.done) return Object.freeze({})
            yield step.value
          }
        },
      })
    }
    const joined = joinedFixture as unknown as V2JoinedBrowserShare
    const observed: number[] = []
    let replacements = 0
    const runtime = new SelectionProjectionRuntime({
      receive,
      authority: {
        observeProjection: active => { observed.push(active.revision) },
        startObservationReplacement: () => { replacements += 1 },
        invalidate: () => undefined,
      },
      observability: new V2ControllerObservability({}),
      currentJoinedShare: () => joined,
      isDisposed: () => false,
      onFailure: error => { throw error },
    })
    runtime.start(joined, 'selection-change')
    await waitFor(() => runtime.current !== undefined)
    initialDiscovery.reject(new RetryableProjectionDiscoveryError('receiver-reconnecting'))
    await waitFor(() => runtime.current?.state.discovery.kind === 'retryable-failure')
    const active = runtime.current!
    const firstController = active.controller

    const retry = runtime.retry(active)
    await waitFor(() => active.controller !== firstController)

    expect(runtime.current).toBe(active)
    expect(firstController.signal.aborted).toBe(true)
    expect(active.controller).not.toBe(firstController)
    expect(receive.environmentCalls).toBe(2)
    expect(replacements).toBe(1)
    expect(observed.length).toBeGreaterThan(1)
    retryDiscovery.resolve()
    await retry
    runtime.stop(new DOMException('Test completed', 'AbortError'))
  })
})
