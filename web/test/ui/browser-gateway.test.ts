import { describe, expect, it, vi } from 'vitest'

import type { CapabilityLink, TransferPlan, ValidatedManifestV1 } from '../../src/contracts'
import { PeerSessionError } from '../../src/session'
import {
  BrowserReceiverGateway,
  sanitizePeerTerminalMessage,
  type BrowserGatewayRuntime,
} from '../../src/ui/browser-gateway'
import {
  prepareBrowserOutput,
  type BrowserOutputRuntime,
} from '../../src/ui/browser-output'
import type { JoinedShare, ReceiverTransferObserver } from '../../src/ui/model'

const MANIFEST = Object.freeze({
  version: 1,
  chunkSize: 1024,
  entries: Object.freeze([]),
}) as unknown as ValidatedManifestV1
const PREPARATION_CLEANUP_ATTEMPTS = 3

describe('browser gateway public errors', () => {
  it('normalizes controls and bounds peer-provided terminal text', () => {
    const error = new PeerSessionError({
      type: 'error',
      code: 1,
      message: ` first\u0000\nsecond ${'x'.repeat(400)}`,
    })

    const message = sanitizePeerTerminalMessage(error)
    expect(message).toMatch(/^The sender stopped the transfer: first second/u)
    expect(message).not.toMatch(/[\r\n]/u)
    expect(message.length).toBeLessThanOrEqual(275)
  })

  it('removes Unicode formatting controls and truncates without splitting scalars', () => {
    const error = new PeerSessionError({
      type: 'error',
      code: 1,
      message: `start\u202e\u2066 ${'x'.repeat(233)}😀tail`,
    })

    const message = sanitizePeerTerminalMessage(error)
    const detail = message.replace('The sender stopped the transfer: ', '')
    expect(message).not.toMatch(/[\p{Cc}\p{Cf}\p{Zl}\p{Zp}]/u)
    expect(Array.from(detail)).toHaveLength(240)
    expect(detail.endsWith('😀')).toBe(true)
  })

  it('snapshots capability bytes before async work and clears its owned copy on close', async () => {
    const readSecret = Uint8Array.from({ length: 16 }, (_, index) => index + 1)
    const relayHints = ['https://relay.test']
    const capability = {
      suite: 1,
      shareId: 'AAECAwQFBgcI',
      readSecret,
      relayHints,
    } as unknown as CapabilityLink
    let openedCapability: CapabilityLink | undefined
    const close = vi.fn(async () => undefined)
    const connection = {
      sealedManifest: new Uint8Array(),
      close,
    }
    const runtime = {
      dialReceiver: async () => connection,
      openManifest: async (owned: CapabilityLink) => {
        openedCapability = owned
        return { manifest: MANIFEST, fingerprint: new Uint8Array(32) }
      },
    } as unknown as BrowserGatewayRuntime
    const gateway = new BrowserReceiverGateway(
      () => Promise.reject(new Error('unused')),
      [],
      runtime,
    )

    const joinedTask = gateway.join(capability, new AbortController().signal)
    readSecret.fill(0xff)
    relayHints[0] = 'https://changed.test'
    const joined = await joinedTask

    expect(openedCapability?.readSecret).toEqual(
      Uint8Array.from({ length: 16 }, (_, index) => index + 1),
    )
    expect(openedCapability?.relayHints).toEqual(['https://relay.test'])
    await joined.close()
    expect(openedCapability?.readSecret).toEqual(new Uint8Array(16))
    expect(close).toHaveBeenCalledOnce()
  })

  it('clears its capability snapshot when authenticated join fails', async () => {
    const capability = {
      suite: 1,
      shareId: 'AAECAwQFBgcI',
      readSecret: Uint8Array.from({ length: 16 }, () => 7),
      relayHints: ['https://relay.test'],
    } as unknown as CapabilityLink
    let openedCapability: CapabilityLink | undefined
    const close = vi.fn(async () => undefined)
    const runtime = {
      dialReceiver: async () => ({ sealedManifest: new Uint8Array(), close }),
      openManifest: async (owned: CapabilityLink) => {
        openedCapability = owned
        throw new Error('authentication failed')
      },
    } as unknown as BrowserGatewayRuntime
    const gateway = new BrowserReceiverGateway(
      () => Promise.reject(new Error('unused')),
      [],
      runtime,
    )

    await expect(
      gateway.join(capability, new AbortController().signal),
    ).rejects.toMatchObject({ code: 'connection-failed' })
    expect(openedCapability?.readSecret).toEqual(new Uint8Array(16))
    expect(close).toHaveBeenCalledOnce()
  })

  it('normalizes synchronous picker failures before any transfer capability is used', async () => {
    const observer = {} as ReceiverTransferObserver
    const share = {} as JoinedShare
    const plan = {} as TransferPlan
    const canceled = new BrowserReceiverGateway(
      () => {
        throw new DOMException('private picker detail', 'AbortError')
      },
      [],
    )
    const failed = new BrowserReceiverGateway(
      () => {
        throw new Error('PRIVATE-INTERNAL-PICKER-DETAIL')
      },
      [],
    )

    await expect(
      canceled.start(share, plan, 'download', observer, new AbortController().signal),
    ).rejects.toMatchObject({
      code: 'output-cancelled',
      message: 'The save prompt was canceled.',
    })
    await expect(
      failed.start(share, plan, 'download', observer, new AbortController().signal),
    ).rejects.toMatchObject({
      code: 'output-unavailable',
      message: 'The selected save destination is not available.',
    })
  })

  it('does not report clean cancellation when preparation cleanup exhausts its retry window', async () => {
    const creationError = new DOMException('private stream creation failure', 'AbortError')
    const cleanupError = new Error('private exact-name cleanup failure')
    const removedNames: string[] = []
    const root = {
      getFileHandle: (_name: string, options?: { readonly create?: boolean }) =>
        options?.create === true
          ? Promise.resolve({
              createWritable: () => Promise.reject(creationError),
            } as unknown as FileSystemFileHandle)
          : Promise.reject(new DOMException('missing', 'NotFoundError')),
      removeEntry: (name: string) => {
        removedNames.push(name)
        return Promise.reject(cleanupError)
      },
    } as unknown as FileSystemDirectoryHandle
    const outputRuntime = {
      browserWindow: {},
      storage: { getDirectory: () => Promise.resolve(root) },
      randomId: () => 'gateway-failure',
      present: () => undefined,
    } as unknown as BrowserOutputRuntime
    const gateway = new BrowserReceiverGateway(
      (plan, choice) => prepareBrowserOutput(plan, choice, outputRuntime),
      [],
    )
    const observer = {
      started: vi.fn(),
      progress: vi.fn(),
      reconnecting: vi.fn(),
      reconnected: vi.fn(),
    }
    const plan = {
      selectedEntries: [{ kind: 'file', path: 'report.txt', size: 0, mtime: 0 }],
    } as unknown as TransferPlan

    await expect(
      gateway.start(
        {} as JoinedShare,
        plan,
        'download',
        observer,
        new AbortController().signal,
      ),
    ).rejects.toMatchObject({
      code: 'output-unavailable',
      message: 'The selected save destination is not available.',
    })
    expect(removedNames).toEqual(
      Array.from(
        { length: PREPARATION_CLEANUP_ATTEMPTS + 1 },
        () => '.windshare-download-gateway-failure',
      ),
    )
    expect(observer.started).not.toHaveBeenCalled()
    expect(observer.progress).not.toHaveBeenCalled()
  })

  it('does not hide prepared-output cleanup failure behind the primary transfer error', async () => {
    const abort = vi.fn(async () => {
      throw new Error('PRIVATE-CLEANUP-DETAIL')
    })
    const connection = {
      sealedManifest: new Uint8Array(),
      close: async () => undefined,
    }
    const runtime = {
      dialReceiver: async () => connection,
      openManifest: async () => ({ manifest: MANIFEST, fingerprint: new Uint8Array(32) }),
    } as unknown as BrowserGatewayRuntime
    const gateway = new BrowserReceiverGateway(
      async () => ({
        transferTarget: (receiver) => receiver({} as never),
        commit: async () => undefined,
        abort,
      }),
      [],
      runtime,
    )
    const capability = {
      suite: 1,
      shareId: 'AAECAwQFBgcI',
      readSecret: new Uint8Array(16),
      relayHints: ['https://relay.test'],
    } as unknown as CapabilityLink
    const share = await gateway.join(capability, new AbortController().signal)

    await expect(
      gateway.start(
        share,
        {} as TransferPlan,
        'download',
        {} as ReceiverTransferObserver,
        new AbortController().signal,
      ),
    ).rejects.toMatchObject({
      code: 'transfer-failed',
      message: 'The download could not be completed safely.',
    })
    expect(abort).toHaveBeenCalledOnce()
  })
})
