import { vi } from 'vitest'

import type { RecoverySummary } from '../../src/output/file-system-access/recovery-summary'
import type {
  ReceiveOperationMutationPort,
  ReceiveOperationResumeSource,
} from '../../src/output/resume/authority'
import type {
  AuthorityOwnedReceiveOperationContinuation,
  AuthorityOwnedReceiveOperationMutationResult,
} from '../../src/output/resume/reopen-authority'
import type {
  ReceiveLifecycleState,
  ReceiveLifecycleStatePayload,
} from '../../src/output/workspace'
import { createSelectionSpec } from '../../src/transfer/intent'
import type {
  BrowserReceiveWindow,
  BrowserRetainedContinuationExecutor,
} from '../../src/ui/v2-browser-receive-composition'
import type { V2BoundReceiveOperation } from '../../src/ui/v2-receive-runtime'

type ResumableFileSetPayload = Extract<ReceiveLifecycleStatePayload, {
  kind: 'resumable-receive'
  payloadKind: 'file-set'
}>
type TestReceiveLifecyclePayload = Exclude<ReceiveLifecycleStatePayload, ResumableFileSetPayload> |
  (Omit<ResumableFileSetPayload, 'selectionFacts'> &
    Readonly<{ selectionFacts?: ResumableFileSetPayload['selectionFacts'] }>)

export type ContinuationKind = AuthorityOwnedReceiveOperationContinuation['kind']

export function fakeContinuation<Kind extends ContinuationKind>(
  kind: Kind,
  close: () => Promise<void>,
): Extract<AuthorityOwnedReceiveOperationContinuation, { readonly kind: Kind }> {
  return Object.freeze({
    kind,
    operation: Object.freeze({ close }),
  }) as unknown as Extract<AuthorityOwnedReceiveOperationContinuation, { readonly kind: Kind }>
}

export function mutationPort(
  resume: ReceiveOperationMutationPort<AuthorityOwnedReceiveOperationMutationResult>['resume'],
): ReceiveOperationMutationPort<AuthorityOwnedReceiveOperationMutationResult> {
  return Object.freeze({
    resume,
    expire: () => Promise.reject(new Error('unexpected expiry')),
    discard: () => Promise.resolve(Object.freeze({ kind: 'already-absent' })),
  })
}

export function fakeContinuationExecutor(
  overrides: Partial<BrowserRetainedContinuationExecutor>,
): BrowserRetainedContinuationExecutor {
  return Object.freeze({
    resumeReceive: () => Promise.reject(new Error('unexpected receive continuation')),
    resumePackage: () => Promise.reject(new Error('unexpected package continuation')),
    continueRetained: () => Promise.reject(new Error('unexpected retained continuation')),
    ...overrides,
  })
}

export function fakeContinuationRuntime(
  close: () => Promise<void>,
): V2BoundReceiveOperation {
  let detached = false
  return Object.freeze({
    detach: async () => {
      if (detached) return
      detached = true
      await close()
    },
  }) as unknown as V2BoundReceiveOperation
}

export class FakeResumeSource implements ReceiveOperationResumeSource {
  readonly #lifecycles: readonly ReceiveLifecycleState[]
  readonly #recoverySummary: RecoverySummary | undefined
  closeCalls = 0

  constructor(lifecycles: readonly ReceiveLifecycleState[], recoverySummary?: RecoverySummary) {
    this.#lifecycles = Object.freeze([...lifecycles])
    this.#recoverySummary = recoverySummary
  }

  listLifecycleStates(): Promise<readonly ReceiveLifecycleState[]> {
    return Promise.resolve(this.#lifecycles)
  }

  readRecoverySummary(): Promise<RecoverySummary | undefined> {
    return Promise.resolve(this.#recoverySummary)
  }

  close(): void {
    this.closeCalls += 1
  }
}

export function retainedLifecycles(): readonly ReceiveLifecycleState[] {
  return Object.freeze([
    receiveLifecycle(1, {
      kind: 'resumable-receive',
      payloadKind: 'file-set',
      checkpointSetDigest: identity(30, 32),
      completedFileCount: 2n,
      completedBytes: 256n,
      expiresAt: 5_000,
    }),
    receiveLifecycle(2, {
      kind: 'resumable-package',
      sealedMaterializationDigest: identity(31, 32),
      tempCleanupProofDigest: identity(32, 32),
      expiresAt: 5_000,
    }),
    receiveLifecycle(3, {
      kind: 'waiting-to-save',
      packageDigest: identity(33, 32),
      expiresAt: 5_000,
    }),
    receiveLifecycle(4, {
      kind: 'download-started',
      attemptKind: 'workspace',
      attemptId: identity(34),
      packageDigest: identity(35, 32),
      retryableUntil: 5_000,
    }),
    receiveLifecycle(5, {
      kind: 'expired',
      priorStableState: 'waiting-to-save',
      expiresAt: 900,
      cleanupState: 'cleanup-pending',
      expiryReceiptDigest: identity(36, 32),
    }),
    receiveLifecycle(6, {
      kind: 'published',
      receiptDigest: identity(37, 32),
      cleanupState: 'cleanup-pending',
    }),
    receiveLifecycle(7, {
      kind: 'needs-attention',
      reason: 'target-ownership-unknown',
      lastVerifiedRecordDigest: identity(38, 32),
    }),
    receiveLifecycle(8, {
      kind: 'published',
      receiptDigest: identity(39, 32),
      cleanupState: 'clean',
    }),
  ])
}

export function receiveLifecycle(
  seed: number,
  payload: TestReceiveLifecyclePayload,
): ReceiveLifecycleState {
  const selectionFacts = payload.kind === 'resumable-receive' && payload.payloadKind === 'file-set'
    ? payload.selectionFacts ?? Object.freeze({
        discoveredFileCount: payload.completedFileCount,
        discoveredBytes: payload.completedBytes,
        discovery: 'failed' as const,
      })
    : undefined
  return Object.freeze({
    ...payload,
    ...(selectionFacts === undefined ? {} : { selectionFacts }),
    operationId: identity(seed),
    receiveIntentDigest: identity(seed + 40, 32),
    generation: 1n,
  }) as ReceiveLifecycleState
}

export async function testSelection() {
  return createSelectionSpec({
    shareInstance: identity(1),
    syntheticRoot: identity(2),
    rules: { mode: 'node-id', defaultSelected: true, rules: [] },
  })
}

export function capableWindow(
  showDirectoryPicker: NonNullable<BrowserReceiveWindow['showDirectoryPicker']>,
): BrowserReceiveWindow {
  class TestFile extends Blob {
    readonly name: string
    readonly lastModified = 0
    readonly webkitRelativePath = ''

    constructor(parts: BlobPart[], name: string) {
      super(parts)
      this.name = name
    }
  }
  const anchor = {
    download: '',
    href: '',
    hidden: false,
    click: vi.fn(),
    remove: vi.fn(),
  }
  const candidate = {
    indexedDB: { open: vi.fn() },
    navigator: {
      locks: { request: vi.fn() },
      storage: {
        getDirectory: vi.fn(),
        estimate: vi.fn(async () => ({ usage: 1_024, quota: 8_192 })),
      },
    },
    showDirectoryPicker,
    URL: {
      createObjectURL: vi.fn(() => 'blob:windshare-test'),
      revokeObjectURL: vi.fn(),
    },
    Blob,
    File: TestFile,
    WritableStream,
    document: {
      createElement: vi.fn(() => anchor),
      documentElement: { append: vi.fn() },
    },
    setTimeout,
    clearTimeout,
  }
  return candidate as unknown as BrowserReceiveWindow
}

export function directoryHandle(): FileSystemDirectoryHandle {
  return Object.freeze({ kind: 'directory', name: 'downloads' }) as FileSystemDirectoryHandle
}

export function identity(seed: number, width = 16): string {
  const bytes = new Uint8Array(width)
  bytes[0] = seed
  bytes[bytes.length - 1] = seed ^ 0xff
  return Buffer.from(bytes).toString('base64url')
}

export function retainedRecoverySummary(lifecycle: ReceiveLifecycleState): RecoverySummary {
  if (lifecycle.kind !== 'resumable-receive' || lifecycle.payloadKind !== 'file-set') {
    throw new TypeError('retained summary fixture requires a file-set lifecycle')
  }
  return Object.freeze({
    lifecycleGeneration: lifecycle.generation,
    checkpointSetDigest: lifecycle.checkpointSetDigest,
    discoveredFileCount: 3n,
    discoveredBytes: 256n,
    discovery: 'known-so-far',
    completedFileCount: lifecycle.completedFileCount,
    completedBytes: lifecycle.completedBytes,
    incompleteFileCount: 1n,
    verifiedPartialFileCount: 1n,
    verifiedPartialBytes: 32n,
    unstartedFileCount: 1n,
    unstartedBytes: 64n,
    preservingRemainingBytes: 160n,
    restartRemainingBytes: 192n,
    restartRedownloadBytes: 32n,
    maximumPreservingTemporaryBytes: 32n,
  })
}
