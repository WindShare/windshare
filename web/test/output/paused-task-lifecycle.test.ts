import { describe, expect, it, vi } from 'vitest'

import { encodeBase64Url } from '../../src/crypto/bytes'
import {
  FILE_SYSTEM_ACCESS_BACKEND,
  SINGLE_FILE_STREAM_BACKEND,
  type OutputCapabilityIdentity,
} from '../../src/output/capability/contract'
import {
  IndexedDbBrowserPausedTaskLifecycle,
  type PausedTaskRepositoryFactory,
} from '../../src/output/browser/paused-task-lifecycle'
import { pausedTaskDescriptorV1 } from '../../src/output/resume/descriptor'
import {
  createTransferIntentDraft,
  freezeTransferIntent,
  type TransferIntent,
} from '../../src/transfer/intent'
import {
  COMPLETED_JOB_SETTLEMENT,
  JobSettlementKind,
  type OutputSession,
} from '../../src/transfer/output-session'
import type { JobOutcome } from '../../src/transfer/outcome'

describe('browser paused-task lifecycle', () => {
  it('persists after durable open, retains on pause, and removes only after completion', async () => {
    const intent = await durableIntent()
    const descriptor = await pausedTaskDescriptorV1({
      intent,
      rootCapabilityRef: identity(0x51, 32),
    })
    const persist = vi.fn(async () => descriptor)
    const removeCompleted = vi.fn(async () => undefined)
    const close = vi.fn()
    const openRepository = vi.fn(async () => ({ persist, removeCompleted, list: vi.fn(), close }))
    const inner = outputSession(intent)
    const lifecycle = new IndexedDbBrowserPausedTaskLifecycle(openRepository)
    const root = { kind: 'directory' } as FileSystemDirectoryHandle

    const tracked = await lifecycle.track(intent, {
      kind: 'PersistentDirectory',
      root,
      rootIdentity: identityBytes(0x41),
      targetKind: 2,
      backend: FILE_SYSTEM_ACCESS_BACKEND,
      format: 'directory',
    }, inner)

    expect(persist).toHaveBeenCalledWith(intent, root)
    await tracked.pauseJob(new DOMException('pause', 'AbortError'))
    expect(removeCompleted).not.toHaveBeenCalled()

    await tracked.completeJob({ status: 'Succeeded' } as JobOutcome, new AbortController().signal)
    expect(removeCompleted).toHaveBeenCalledWith(descriptor)
    expect(openRepository).toHaveBeenCalledTimes(2)
    expect(close).toHaveBeenCalledTimes(2)
  })

  it('suspends without destructive fallback when descriptor persistence fails', async () => {
    const intent = await durableIntent()
    const persistenceFailure = new Error('persistence unavailable')
    const openRepository: PausedTaskRepositoryFactory = async () => ({
      persist: vi.fn(async () => { throw persistenceFailure }),
      removeCompleted: vi.fn(),
      list: vi.fn(),
      close: vi.fn(),
    })
    const inner = outputSession(intent)
    const lifecycle = new IndexedDbBrowserPausedTaskLifecycle(openRepository)

    await expect(lifecycle.track(intent, {
      kind: 'PersistentDirectory',
      root: { kind: 'directory' } as FileSystemDirectoryHandle,
      rootIdentity: identityBytes(0x41),
      targetKind: 2,
      backend: FILE_SYSTEM_ACCESS_BACKEND,
      format: 'directory',
    }, inner)).rejects.toBe(persistenceFailure)
    expect(inner.pauseJob).toHaveBeenCalledWith(persistenceFailure)
    expect(inner.pauseJob).toHaveBeenCalledTimes(1)
  })

  it('never creates a descriptor for non-durable streams', async () => {
    const intent = await freezeTransferIntent(shareDraft(), {
      target: identity(0x61, 32),
      targetKind: 2,
      backend: SINGLE_FILE_STREAM_BACKEND,
      format: 'single-file',
    })
    const openRepository = vi.fn<PausedTaskRepositoryFactory>()
    const inner = outputSession(intent)
    const lifecycle = new IndexedDbBrowserPausedTaskLifecycle(openRepository)

    const tracked = await lifecycle.track(intent, {
      kind: 'SingleFileStream',
      output: new WritableStream<Uint8Array>(),
      rootIdentity: identityBytes(0x61),
      targetKind: 2,
      backend: SINGLE_FILE_STREAM_BACKEND,
      format: 'single-file',
    }, inner)

    expect(tracked).toBe(inner)
    expect(openRepository).not.toHaveBeenCalled()
  })
})

async function durableIntent(): Promise<TransferIntent> {
  return freezeTransferIntent(shareDraft(), {
    target: identity(0x41, 32),
    targetKind: 2,
    backend: FILE_SYSTEM_ACCESS_BACKEND,
    format: 'directory',
  })
}

function shareDraft() {
  return createTransferIntentDraft({
    shareInstance: identity(0x11, 16),
    syntheticRoot: identity(0x21, 16),
    selection: { mode: 'node-id', defaultSelected: true, rules: [] },
  })
}

function outputSession(intent: TransferIntent): OutputSession {
  return {
    identity: {
      backend: intent.output.backend,
      outputSessionId: identity(0x31, 16),
    },
    format: intent.output.format,
    capabilities: {
      durability: intent.output.backend === SINGLE_FILE_STREAM_BACKEND
        ? 'None'
        : 'ProcessRestart',
      randomWrite: intent.output.backend !== SINGLE_FILE_STREAM_BACKEND,
      fileFailureIsolation: intent.output.backend !== SINGLE_FILE_STREAM_BACKEND,
      modificationTime: intent.output.backend !== SINGLE_FILE_STREAM_BACKEND,
    },
    admitDirectory: vi.fn(),
    finalizeDirectory: vi.fn(),
    beginFile: vi.fn(),
    completeJob: vi.fn(async () => COMPLETED_JOB_SETTLEMENT),
    pauseJob: vi.fn(async () => Object.freeze({
      kind: JobSettlementKind.Paused,
      durability: intent.output.backend === SINGLE_FILE_STREAM_BACKEND
        ? 'None' as const
        : 'ProcessRestart' as const,
    })),
  }
}

function identityBytes(seed: number): OutputCapabilityIdentity {
  return Uint8Array.from({ length: 32 }, (_, index) => (seed + index) & 0xff)
}

function identity(seed: number, width: number): string {
  return encodeBase64Url(Uint8Array.from(
    { length: width },
    (_, index) => (seed + index) & 0xff,
  ))
}
