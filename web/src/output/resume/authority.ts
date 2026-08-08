import {
  createTransferRun,
  snapshotTransferRun,
  type TransferIntent,
  type TransferIntentDraft,
  type TransferRun,
} from '../../transfer/intent'
import {
  validateOutputSessionBinding,
  type OutputSession,
} from '../../transfer/output-session'
import {
  assertPausedTaskCurrentShare,
  type PausedTaskDescriptorV1,
} from './descriptor'

export type PausedTaskTraceName =
  | 'paused-task-descriptor-persisted'
  | 'paused-task-descriptors-listed'
  | 'paused-task-resume-prepared'
  | 'paused-task-capability-accepted'
  | 'paused-task-capability-rejected'
  | 'paused-task-resume-reconstructed'
  | 'paused-task-descriptor-removed'
  | 'paused-task-discard-started'
  | 'paused-task-discard-completed'
  | 'paused-task-discard-needs-attention'

export interface PausedTaskTraceEvent {
  readonly name: PausedTaskTraceName
  readonly atMilliseconds: number
  readonly context: {
    readonly backend: string
    readonly transferIntentDigest: string
    readonly transferJobId?: string
    readonly outputSessionId?: string
    readonly decision?: string
  }
}

export type PausedTaskTraceListener = (event: PausedTaskTraceEvent) => void

export interface ReconstructedPausedTask {
  readonly descriptor: PausedTaskDescriptorV1
  readonly intent: TransferIntent
  readonly run: TransferRun
  readonly session: OutputSession
}

export type TransferRunFactory = () => TransferRun

export interface ResumeStateReferenceOwner {
  open: boolean
}

/**
 * A listed task is a leased observation, not an identifier that can be replayed.
 * Consuming it once makes stale rows incapable of authorizing a second mutation.
 */
export class ResumeStateRef {
  readonly descriptor: PausedTaskDescriptorV1
  readonly completedFileCount: number
  readonly #owner: ResumeStateReferenceOwner
  readonly #pin: unknown
  #consumed = false

  constructor(
    owner: ResumeStateReferenceOwner,
    descriptor: PausedTaskDescriptorV1,
    completedFileCount: number,
    pin: unknown,
  ) {
    if (!Number.isSafeInteger(completedFileCount) || completedFileCount < 0) {
      throw new TypeError('completed paused-task file count is invalid')
    }
    this.#owner = owner
    this.descriptor = descriptor
    this.completedFileCount = completedFileCount
    this.#pin = pin
  }

  /** Authority implementations consume the opaque pin; application code cannot inspect it. */
  consume(owner: ResumeStateReferenceOwner): unknown {
    if (owner !== this.#owner || !owner.open) {
      throw new DOMException('Resume-state reference belongs to a closed inventory', 'InvalidStateError')
    }
    if (this.#consumed) {
      throw new DOMException('Resume-state reference was already consumed', 'InvalidStateError')
    }
    this.#consumed = true
    return this.#pin
  }
}

export class ResumeStateInventory {
  readonly tasks: readonly ResumeStateRef[]
  readonly #owner: ResumeStateReferenceOwner

  constructor(owner: ResumeStateReferenceOwner, tasks: readonly ResumeStateRef[]) {
    this.#owner = owner
    this.tasks = Object.freeze([...tasks])
  }

  close(): void {
    this.#owner.open = false
  }
}

export interface ResumeStateOperationRequest {
  readonly currentShare: TransferIntentDraft
  /** OPFS operations must synchronously begin a fresh user-selected export. */
  readonly acquireOriginPrivateOutput?: () => Promise<WritableStream<Uint8Array>>
  readonly signal?: AbortSignal
}

export const ResumeStateDiscardKind = {
  Discarded: 'Discarded',
  AlreadyAbsent: 'AlreadyAbsent',
  NeedsAttention: 'NeedsAttention',
} as const

export type ResumeStateDiscardResult =
  | Readonly<{
      kind: typeof ResumeStateDiscardKind.Discarded
      preservedCompletedFiles: number
      exportedPartialZip: boolean
    }>
  | Readonly<{ kind: typeof ResumeStateDiscardKind.AlreadyAbsent }>
  | Readonly<{
      kind: typeof ResumeStateDiscardKind.NeedsAttention
      reason: 'checkpoint-changed' | 'output-changed' | 'export-failed' | 'discard-incomplete'
    }>

export class ResumeStateBusyError extends DOMException {
  constructor() {
    super('This resumable task is active in another page', 'InvalidStateError')
  }
}

export interface ResumeStateAuthority {
  listResumeState(): Promise<ResumeStateInventory>
  resume(
    reference: ResumeStateRef,
    request: ResumeStateOperationRequest,
  ): Promise<ReconstructedPausedTask>
  discard(
    reference: ResumeStateRef,
    request: ResumeStateOperationRequest,
  ): Promise<ResumeStateDiscardResult>
}

export interface PausedTaskDescriptorRepository {
  list(): Promise<readonly PausedTaskDescriptorV1[]>
  persist(
    intent: TransferIntent,
    root: FileSystemDirectoryHandle,
  ): Promise<PausedTaskDescriptorV1>
  removeCompleted(descriptor: PausedTaskDescriptorV1): Promise<void>
}

export async function reconstructPausedTask(options: {
  readonly descriptor: PausedTaskDescriptorV1
  readonly currentShare: TransferIntentDraft
  readonly openSession: (run: TransferRun) => Promise<OutputSession>
  readonly createRun?: TransferRunFactory
  readonly onTrace?: PausedTaskTraceListener
}): Promise<ReconstructedPausedTask> {
  assertPausedTaskCurrentShare(options.descriptor, options.currentShare)
  const run = snapshotTransferRun((options.createRun ?? createTransferRun)())
  const session = await options.openSession(run)
  if (session.identity.outputSessionId !== run.outputSessionId) {
    throw new TypeError('resumed output session did not use the fresh runtime identity')
  }
  const validated = validateOutputSessionBinding(options.descriptor.intent, session)
  observePausedTask(options.onTrace, 'paused-task-resume-reconstructed', options.descriptor, {
    transferJobId: run.transferJobId,
    outputSessionId: run.outputSessionId,
    decision: 'fresh-run',
  })
  return Object.freeze({
    descriptor: options.descriptor,
    intent: options.descriptor.intent,
    run,
    session: validated,
  })
}

export function observePausedTask(
  listener: PausedTaskTraceListener | undefined,
  name: PausedTaskTraceName,
  descriptor: PausedTaskDescriptorV1,
  context: {
    readonly transferJobId?: string
    readonly outputSessionId?: string
    readonly decision?: string
  } = {},
): void {
  listener?.(Object.freeze({
    name,
    atMilliseconds: typeof performance === 'undefined' ? Date.now() : performance.now(),
    context: Object.freeze({
      backend: descriptor.intent.output.backend,
      transferIntentDigest: descriptor.intent.digest,
      ...context,
    }),
  }))
}
