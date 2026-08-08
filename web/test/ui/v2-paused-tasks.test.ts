import { describe, expect, it, vi } from 'vitest'

import { encodeBase64Url } from '../../src/crypto/bytes'
import {
  ResumeStateBusyError,
  ResumeStateDiscardKind,
  ResumeStateInventory,
  ResumeStateRef,
  type ResumeStateDiscardResult,
} from '../../src/output/resume/authority'
import {
  pausedTaskDescriptorV1,
  type PausedTaskDescriptorV1,
} from '../../src/output/resume/descriptor'
import {
  createTransferIntentDraft,
  freezeTransferIntent,
} from '../../src/transfer/intent'
import {
  BrowserV2PausedTaskControlPort,
  V2PausedTaskController,
  type V2PausedTaskControlPort,
} from '../../src/ui/v2-paused-tasks'
import type { V2JoinedBrowserShare } from '../../src/ui/v2-gateway'
import type { V2ReceiverSnapshot } from '../../src/ui/v2-model'

const CURRENT_SHARE = createTransferIntentDraft({
  shareInstance: identity(0x11, 16),
  syntheticRoot: identity(0x21, 16),
  selection: { mode: 'node-id', defaultSelected: true, rules: [] },
})

describe('browser paused-task confirmation', () => {
  it('states that committed OPFS members are exported before discard', () => {
    const confirm = vi.fn(() => true)
    const controls = new BrowserV2PausedTaskControlPort({
      windowPort: { confirm } as never,
    })

    expect(controls.confirmDiscard(reference('origin-private-staging', 2))).toBe(true)
    expect(confirm).toHaveBeenCalledWith(expect.stringMatching(
      /2 completed file\(s\) will be exported as a partial ZIP first/u,
    ))
  })

  it('states that committed FSA files remain in the selected folder', () => {
    const confirm = vi.fn(() => false)
    const controls = new BrowserV2PausedTaskControlPort({
      windowPort: { confirm } as never,
    })

    expect(controls.confirmDiscard(reference('file-system-access', 1))).toBe(false)
    expect(confirm).toHaveBeenCalledWith(expect.stringMatching(
      /1 completed file\(s\) will remain in the selected folder/u,
    ))
  })
})

describe('paused-task controller authority', () => {
  it('starts resume capability acquisition before connectivity and publishes the row transition first', async () => {
    const descriptor = await durableDescriptor()
    const events: string[] = []
    const controls = controlsFor(descriptor, {
      resume: () => {
        events.push('resume-capability')
        return Promise.reject(new DOMException('picker cancelled', 'AbortError'))
      },
    })
    const fixture = controllerFixture(descriptor, controls, events)
    await fixture.controller.refresh()
    events.length = 0

    fixture.controller.resume(descriptor.intent.digest)

    expect(events.slice(0, 4)).toEqual([
      'resume-capability',
      'begin-connectivity',
      'publish:paused:resuming',
      'publish:resuming:resuming',
    ])
    await turn()
    expect(fixture.snapshot().phase).toBe('paused')
    fixture.controller.close()
  })

  it('retains a needs-attention task and publishes its typed row state', async () => {
    const descriptor = await durableDescriptor()
    const discard = deferred<ResumeStateDiscardResult>()
    const events: string[] = []
    const controls = controlsFor(descriptor, {
      confirmDiscard: () => {
        events.push('confirm-discard')
        return true
      },
      discard: () => {
        events.push('discard-capability')
        return discard.promise
      },
    })
    const fixture = controllerFixture(descriptor, controls, events)
    await fixture.controller.refresh()
    events.length = 0

    fixture.controller.discard(descriptor.intent.digest)
    expect(events.slice(0, 4)).toEqual([
      'confirm-discard',
      'discard-capability',
      'publish:paused:discarding',
      'publish:discarding:discarding',
    ])
    discard.resolve({
      kind: ResumeStateDiscardKind.NeedsAttention,
      reason: 'output-changed',
    })
    await turn()

    expect(fixture.snapshot()).toMatchObject({
      phase: 'needs-attention',
      pausedTasks: [{ state: 'needs-attention' }],
    })
    fixture.controller.close()
  })

  it('projects lease contention as busy without consuming the paused task', async () => {
    const descriptor = await durableDescriptor()
    const controls = controlsFor(descriptor, {
      discard: () => Promise.reject(new ResumeStateBusyError()),
    })
    const fixture = controllerFixture(descriptor, controls)
    await fixture.controller.refresh()

    fixture.controller.discard(descriptor.intent.digest)
    await turn()

    expect(fixture.snapshot()).toMatchObject({
      phase: 'paused',
      pausedTasks: [{ state: 'busy' }],
    })
    fixture.controller.close()
  })

  it('publishes discarded only after the authority removes the durable task', async () => {
    const descriptor = await durableDescriptor()
    let removed = false
    const controls = controlsFor(descriptor, {
      refresh: async () => removed ? emptyInventory() : inventory(descriptor),
      discard: async () => {
        removed = true
        return {
          kind: ResumeStateDiscardKind.Discarded,
          preservedCompletedFiles: 2,
          exportedPartialZip: false,
        }
      },
    })
    const fixture = controllerFixture(descriptor, controls)
    await fixture.controller.refresh()

    fixture.controller.discard(descriptor.intent.digest)
    await turn()

    expect(fixture.snapshot()).toMatchObject({
      phase: 'discarded',
      pausedTasks: [],
    })
    fixture.controller.close()
  })
})

function reference(backend: string, completedFileCount: number): ResumeStateRef {
  return new ResumeStateRef(
    { open: true },
    {
      intent: {
        output: { backend },
      },
    } as PausedTaskDescriptorV1,
    completedFileCount,
    {},
  )
}

async function durableDescriptor(): Promise<PausedTaskDescriptorV1> {
  const intent = await freezeTransferIntent(CURRENT_SHARE, {
    target: identity(0x31, 32),
    targetKind: 2,
    backend: 'file-system-access',
    format: 'directory',
  })
  return pausedTaskDescriptorV1({
    intent,
    rootCapabilityRef: identity(0x51, 32),
  })
}

function controlsFor(
  descriptor: PausedTaskDescriptorV1,
  overrides: Partial<V2PausedTaskControlPort> = {},
): V2PausedTaskControlPort {
  return {
    refresh: async () => inventory(descriptor),
    confirmDiscard: () => true,
    resume: () => Promise.reject(new Error('unexpected resume')),
    discard: async () => ({
      kind: ResumeStateDiscardKind.Discarded,
      preservedCompletedFiles: 0,
      exportedPartialZip: false,
    }),
    removeCompleted: async () => undefined,
    close: () => undefined,
    ...overrides,
  }
}

function inventory(descriptor: PausedTaskDescriptorV1): ResumeStateInventory {
  const owner = { open: true }
  return new ResumeStateInventory(owner, [new ResumeStateRef(owner, descriptor, 2, {})])
}

function emptyInventory(): ResumeStateInventory {
  return new ResumeStateInventory({ open: true }, [])
}

function controllerFixture(
  descriptor: PausedTaskDescriptorV1,
  controls: V2PausedTaskControlPort,
  events: string[] = [],
) {
  let snapshot = {
    phase: 'paused',
    status: 'Paused.',
    error: null,
    pausedTasks: Object.freeze([]),
  } as unknown as V2ReceiverSnapshot
  const joined = {
    descriptor: {
      shareInstanceId: CURRENT_SHARE.shareInstance,
      syntheticRootId: CURRENT_SHARE.syntheticRoot,
    },
    recoveryIdentity: 'unused',
    beginDownloadConnectivity: () => {
      events.push('begin-connectivity')
      return {
        routes: {},
        observeSizeClass: () => undefined,
        close: () => undefined,
      }
    },
  } as unknown as V2JoinedBrowserShare
  const controller = new V2PausedTaskController(controls, {
    joined: () => joined,
    disposed: () => false,
    regularTransferActive: () => false,
    snapshot: () => snapshot,
    publish: (next) => {
      snapshot = next
      const state = next.pausedTasks.find((task) => task.id === descriptor.intent.digest)?.state ?? 'none'
      events.push(`publish:${next.phase}:${state}`)
    },
    publicError: (error) => error instanceof Error ? error.message : String(error),
    transferTrace: () => undefined,
  })
  return { controller, snapshot: () => snapshot }
}

function deferred<T>(): {
  readonly promise: Promise<T>
  readonly resolve: (value: T) => void
} {
  let resolve!: (value: T) => void
  return {
    promise: new Promise<T>((accept) => { resolve = accept }),
    resolve: (value) => resolve(value),
  }
}

async function turn(): Promise<void> {
  for (let index = 0; index < 8; index += 1) await Promise.resolve()
}

function identity(seed: number, width: number): string {
  return encodeBase64Url(Uint8Array.from({ length: width }, (_, index) => (seed + index) & 0xff))
}
