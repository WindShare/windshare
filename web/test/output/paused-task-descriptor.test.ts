import { describe, expect, it, vi } from 'vitest'

import { encodeBase64Url } from '../../src/crypto/bytes'
import {
  FILE_SYSTEM_ACCESS_BACKEND,
  SINGLE_FILE_STREAM_BACKEND,
} from '../../src/output/capability/contract'
import {
  PausedTaskShareAuthorityError,
  pausedTaskDescriptorV1,
  validatePausedTaskDescriptorV1,
} from '../../src/output/resume/descriptor'
import { reconstructPausedTask } from '../../src/output/resume/authority'
import {
  createTransferIntentDraft,
  freezeTransferIntent,
  type TransferIntent,
  type TransferRun,
} from '../../src/transfer/intent'
import type { OutputSession } from '../../src/transfer/output-session'

const CURRENT_SHARE = createTransferIntentDraft({
  shareInstance: identity(0x11, 16),
  syntheticRoot: identity(0x21, 16),
  selection: {
    mode: 'node-id',
    defaultSelected: true,
    rules: [{ kind: 'file', id: identity(0x31, 16), selected: false }],
  },
})

describe('PausedTaskDescriptorV1', () => {
  it('round trips only canonical intent and an independent capability reference', async () => {
    const intent = await durableIntent()
    const descriptor = await pausedTaskDescriptorV1({
      intent,
      rootCapabilityRef: identity(0x51, 32),
    })
    const decoded = await validatePausedTaskDescriptorV1(structuredClone(descriptor))

    expect(decoded).toEqual(descriptor)
    expect(Object.keys(decoded)).toEqual(['schemaVersion', 'intent', 'rootCapabilityRef'])
    expect(Object.keys(decoded.intent)).toEqual([
      'version',
      'shareInstance',
      'syntheticRoot',
      'selection',
      'output',
      'digest',
      'canonicalBytes',
    ])
    expect(decoded.rootCapabilityRef).not.toBe(decoded.intent.digest)
    expect(decoded.rootCapabilityRef).not.toBe(decoded.intent.output.target)
    expect(forbiddenPersistedKeys(decoded)).toEqual([])
  })

  it('rejects schema drift, forged canonical authority, and runtime fields', async () => {
    const descriptor = await pausedTaskDescriptorV1({
      intent: await durableIntent(),
      rootCapabilityRef: identity(0x52, 32),
    })
    await expect(validatePausedTaskDescriptorV1({
      ...descriptor,
      schemaVersion: 2,
    })).rejects.toThrow(/schema version/)
    await expect(validatePausedTaskDescriptorV1({
      ...descriptor,
      transferJobId: identity(0x61, 16),
    })).rejects.toThrow(/structured shape/)
    await expect(validatePausedTaskDescriptorV1({
      ...descriptor,
      intent: { ...descriptor.intent, outputSessionId: identity(0x62, 16) },
    })).rejects.toThrow(/structured shape/)
    await expect(validatePausedTaskDescriptorV1({
      ...descriptor,
      intent: { ...descriptor.intent, digest: identity(0x63, 32) },
    })).rejects.toThrow(/digest/)
    await expect(validatePausedTaskDescriptorV1({
      ...descriptor,
      intent: {
        ...descriptor.intent,
        canonicalBytes: Uint8Array.from(
          descriptor.intent.canonicalBytes,
          (value, index) => index === 0 ? value ^ 1 : value,
        ),
      },
    })).rejects.toThrow(/canonical bytes/)
  })

  it('accepts only durable FSA or OPFS output intents', async () => {
    const streamIntent = await freezeTransferIntent(CURRENT_SHARE, {
      target: identity(0x41, 32),
      targetKind: 2,
      backend: SINGLE_FILE_STREAM_BACKEND,
      format: 'single-file',
    })
    await expect(pausedTaskDescriptorV1({
      intent: streamIntent,
      rootCapabilityRef: identity(0x53, 32),
    })).rejects.toThrow(/durable browser output/)
  })

  it('reconstructs only under current share authority and a fresh runtime', async () => {
    const descriptor = await pausedTaskDescriptorV1({
      intent: await durableIntent(),
      rootCapabilityRef: identity(0x54, 32),
    })
    const run = Object.freeze({
      transferJobId: identity(0x71, 16),
      outputSessionId: identity(0x72, 16),
    })
    const openSession = vi.fn(async (created: TransferRun) => outputSession(
      descriptor.intent,
      created.outputSessionId,
    ))

    const reconstructed = await reconstructPausedTask({
      descriptor,
      currentShare: CURRENT_SHARE,
      createRun: () => run,
      openSession,
    })

    expect(openSession).toHaveBeenCalledWith(run)
    expect(reconstructed.run).toEqual(run)
    expect(reconstructed.intent).toBe(descriptor.intent)
    expect(Object.hasOwn(reconstructed.descriptor, 'ranges')).toBe(false)
    expect(Object.hasOwn(reconstructed.descriptor, 'progress')).toBe(false)

    const changedShare = createTransferIntentDraft({
      shareInstance: CURRENT_SHARE.shareInstance,
      syntheticRoot: CURRENT_SHARE.syntheticRoot,
      selection: { mode: 'node-id', defaultSelected: false, rules: [] },
    })
    await expect(reconstructPausedTask({
      descriptor,
      currentShare: changedShare,
      createRun: () => run,
      openSession,
    })).rejects.toBeInstanceOf(PausedTaskShareAuthorityError)
    expect(openSession).toHaveBeenCalledOnce()
  })
})

async function durableIntent(): Promise<TransferIntent> {
  return freezeTransferIntent(CURRENT_SHARE, {
    target: identity(0x41, 32),
    targetKind: 2,
    backend: FILE_SYSTEM_ACCESS_BACKEND,
    format: 'directory',
  })
}

function outputSession(intent: TransferIntent, outputSessionId: string): OutputSession {
  return {
    identity: Object.freeze({
      backend: intent.output.backend,
      outputSessionId,
    }),
    format: intent.output.format,
    capabilities: Object.freeze({
      durability: 'ProcessRestart',
      randomWrite: true,
      fileFailureIsolation: true,
      modificationTime: true,
    }),
    admitDirectory: vi.fn(),
    finalizeDirectory: vi.fn(),
    beginFile: vi.fn(),
    completeJob: vi.fn(),
    pauseJob: vi.fn(),
  } as unknown as OutputSession
}

function forbiddenPersistedKeys(value: unknown): readonly string[] {
  const forbidden = /(?:secret|key|receipt|token|sessionid|runid|jobid|ledger|progress|ranges?)/iu
  const found: string[] = []
  visit(value)
  return found.sort()

  function visit(current: unknown): void {
    if (current instanceof Uint8Array || current === null || typeof current !== 'object') return
    if (Array.isArray(current)) {
      current.forEach(visit)
      return
    }
    for (const [key, child] of Object.entries(current)) {
      if (forbidden.test(key)) found.push(key)
      visit(child)
    }
  }
}

function identity(seed: number, width: number): string {
  const bytes = Uint8Array.from({ length: width }, (_, index) => (seed + index) & 0xff)
  return encodeBase64Url(bytes)
}
