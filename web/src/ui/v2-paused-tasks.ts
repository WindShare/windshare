import { IndexedDbPausedTaskState } from '../output/browser/indexeddb-resume-state'
import {
  ResumeStateBusyError,
  ResumeStateDiscardKind,
  type PausedTaskTraceListener,
  type ReconstructedPausedTask,
  type ResumeStateAuthority,
  type ResumeStateDiscardResult,
  type ResumeStateInventory,
  type ResumeStateRef,
} from '../output/resume/authority'
import {
  assertPausedTaskCurrentShare,
  type PausedTaskDescriptorV1,
} from '../output/resume/descriptor'
import { TransferPauseRequestedError } from '../transfer/output-session'
import {
  createTransferIntentDraft,
  type TransferIntentDraft,
  type TransferTraceEvent,
} from '../transfer/intent'
import {
  acquireBrowserResumeZipOutput,
  resumedBrowserV2OutputAuthority,
  type V2BrowserOutputWindow,
} from './v2-output'
import {
  descriptorIdentity,
  isAbortError,
  transferProgressSnapshot,
  transferTerminalSnapshot,
} from './v2-controller-state'
import {
  v2SelectionPolicyFromIntent,
  type V2JoinedBrowserShare,
} from './v2-gateway'
import type { V2ReceiverSnapshot } from './v2-model'

export interface V2PausedTaskControlPort {
  refresh(): Promise<ResumeStateInventory>
  confirmDiscard(reference: ResumeStateRef): boolean
  resume(
    reference: ResumeStateRef,
    currentShare: TransferIntentDraft,
  ): Promise<ReconstructedPausedTask>
  discard(
    reference: ResumeStateRef,
    currentShare: TransferIntentDraft,
  ): Promise<ResumeStateDiscardResult>
  removeCompleted(descriptor: PausedTaskDescriptorV1): Promise<void>
  close(): void
}

export interface V2PausedTaskControllerHost {
  joined(): V2JoinedBrowserShare | undefined
  disposed(): boolean
  regularTransferActive(): boolean
  snapshot(): V2ReceiverSnapshot
  publish(snapshot: V2ReceiverSnapshot): void
  publicError(error: unknown): string
  transferTrace(event: TransferTraceEvent): void
}

export class V2PausedTaskController {
  readonly #controls: V2PausedTaskControlPort
  readonly #host: V2PausedTaskControllerHost
  #inventory: ResumeStateInventory | undefined
  #references = new Map<string, ResumeStateRef>()
  #transfer: AbortController | undefined
  #discarding = false
  #closed = false

  constructor(controls: V2PausedTaskControlPort, host: V2PausedTaskControllerHost) {
    this.#controls = controls
    this.#host = host
  }

  active(): boolean {
    return this.#transfer !== undefined || this.#discarding
  }

  refresh(): Promise<void> {
    return this.#refresh()
  }

  publishRows(): void {
    const existing = new Map(this.#host.snapshot().pausedTasks.map((task) => [task.id, task.state]))
    const pausedTasks: V2ReceiverSnapshot['pausedTasks'][number][] = []
    for (const [id, reference] of this.#references) {
      if (this.#currentShareFor(reference) === undefined) continue
      pausedTasks.push(Object.freeze({
        id,
        backend: reference.descriptor.intent.output.backend,
        format: reference.descriptor.intent.output.format as 'directory' | 'zip',
        completedFileCount: reference.completedFileCount,
        authorizedForCurrentShare: true,
        state: existing.get(id) ?? 'ready' as const,
      }))
    }
    this.#publish({ ...this.#snapshot(), pausedTasks: Object.freeze(pausedTasks) })
  }

  resume(id: string): void {
    const joined = this.#host.joined()
    const reference = this.#references.get(id)
    if (joined === undefined || reference === undefined || this.#blocked()) return
    const currentShare = this.#currentShareFor(reference)
    if (currentShare === undefined) return
    let reconstruction: Promise<ReconstructedPausedTask>
    try {
      // Capability renewal or a save picker starts before this click stack yields.
      reconstruction = this.#controls.resume(reference, currentShare)
    } catch (error) {
      this.#fail(error)
      return
    }
    const connectivity = joined.beginDownloadConnectivity('unknown')
    this.#transfer = new AbortController()
    this.#setState(id, 'resuming')
    this.#publish({
      ...this.#snapshot(),
      phase: 'resuming',
      status: 'Reauthorizing the saved output and starting a fresh transfer run…',
      error: null,
    })
    this.#runResumedTransfer(joined, reconstruction, connectivity, id).catch(() => undefined)
  }

  discard(id: string): void {
    const reference = this.#references.get(id)
    if (reference === undefined || this.#blocked()) return
    const currentShare = this.#currentShareFor(reference)
    if (currentShare === undefined || !this.#controls.confirmDiscard(reference)) return
    let discard: ReturnType<V2PausedTaskControlPort['discard']>
    try {
      // OPFS partial-ZIP acquisition must begin in the confirmation click stack.
      discard = this.#controls.discard(reference, currentShare)
    } catch (error) {
      this.#fail(error)
      return
    }
    this.#discarding = true
    this.#setState(id, 'discarding')
    this.#publish({
      ...this.#snapshot(),
      phase: 'discarding',
      status: reference.completedFileCount > 0 &&
        reference.descriptor.intent.output.backend === 'origin-private-staging'
        ? 'Exporting completed files before discarding resumable state…'
        : 'Discarding incomplete resumable state…',
      error: null,
    })
    this.#runDiscard(discard, id).catch(() => undefined)
  }

  abortTransfer(reason: unknown): void {
    this.#transfer?.abort(reason)
  }

  close(): void {
    if (this.#closed) return
    this.#closed = true
    this.#inventory?.close()
    this.#inventory = undefined
    this.#references.clear()
    this.#controls.close()
  }

  async #runResumedTransfer(
    joined: V2JoinedBrowserShare,
    reconstruction: Promise<ReconstructedPausedTask>,
    connectivity: ReturnType<V2JoinedBrowserShare['beginDownloadConnectivity']>,
    taskId: string,
  ): Promise<void> {
    let output: ReturnType<typeof resumedBrowserV2OutputAuthority> | undefined
    try {
      const reconstructed = await reconstruction
      if (this.#host.joined() !== joined || this.#closed || this.#host.disposed()) {
        await reconstructed.session.pauseJob(new TransferPauseRequestedError('Joined share changed'))
        return
      }
      output = resumedBrowserV2OutputAuthority(reconstructed.intent, reconstructed.session)
      const job = joined.transferJob(output, connectivity, {
        selection: v2SelectionPolicyFromIntent(reconstructed.intent),
        intent: reconstructed.intent,
        transferJobId: reconstructed.run.transferJobId,
        onProgress: (progress) => this.#publish({
          ...this.#snapshot(),
          ...transferProgressSnapshot(progress),
        }),
        onMeasure: (measure) => connectivity.observeSizeClass(measure.sizeClass),
        onTrace: (event) => this.#host.transferTrace(event),
      })
      const result = await job.run(this.#transfer?.signal)
      const terminal = transferTerminalSnapshot(this.#snapshot(), result)
      if (result.settlement.kind === 'Completed') {
        await this.#controls.removeCompleted(reconstructed.descriptor)
      }
      await this.#refresh()
      this.#publish({ ...terminal, pausedTasks: this.#snapshot().pausedTasks })
    } catch (error) {
      await output?.abort(error).catch(() => undefined)
      await this.#refresh().catch(() => undefined)
      if (error instanceof ResumeStateBusyError) {
        this.#setState(taskId, 'busy')
        this.#publish({
          ...this.#snapshot(),
          phase: 'paused',
          status: 'This resumable task is active in another page.',
          error: null,
        })
      } else if (isAbortError(error)) {
        this.#publish({
          ...this.#snapshot(),
          phase: 'paused',
          status: 'Resume was cancelled; the saved checkpoint remains available.',
          error: null,
        })
      } else {
        this.#publish({
          ...this.#snapshot(),
          phase: 'needs-attention',
          status: 'The resumable task remains saved, but it could not be reopened safely.',
          error: this.#host.publicError(error),
        })
      }
    } finally {
      connectivity.close()
      this.#transfer = undefined
    }
  }

  async #runDiscard(
    discard: ReturnType<V2PausedTaskControlPort['discard']>,
    taskId: string,
  ): Promise<void> {
    try {
      const result = await discard
      await this.#refresh()
      if (result.kind === ResumeStateDiscardKind.NeedsAttention) {
        this.#setState(taskId, 'needs-attention')
        this.#publish({
          ...this.#snapshot(),
          phase: 'needs-attention',
          status: 'The output changed or discard could not be proven; resumable state was retained.',
          error: null,
        })
        return
      }
      if (result.kind === ResumeStateDiscardKind.AlreadyAbsent) {
        this.#publish({
          ...this.#snapshot(),
          phase: 'discarded',
          status: 'The resumable task had already been removed.',
          error: null,
        })
        return
      }
      this.#publish({
        ...this.#snapshot(),
        phase: 'discarded',
        status: result.exportedPartialZip
          ? `Exported ${result.preservedCompletedFiles} completed file(s), then discarded resumable state.`
          : `Discarded resumable state; ${result.preservedCompletedFiles} completed file(s) remain in the selected folder.`,
        error: null,
      })
    } catch (error) {
      await this.#refresh().catch(() => undefined)
      if (error instanceof ResumeStateBusyError) {
        this.#setState(taskId, 'busy')
        this.#publish({
          ...this.#snapshot(),
          phase: 'paused',
          status: 'This resumable task is active in another page.',
          error: null,
        })
      } else if (isAbortError(error)) {
        this.#publish({
          ...this.#snapshot(),
          phase: 'paused',
          status: 'Cancel was dismissed; resumable state and output were retained.',
          error: null,
        })
      } else {
        this.#setState(taskId, 'needs-attention')
        this.#publish({
          ...this.#snapshot(),
          phase: 'needs-attention',
          status: 'Resumable state was retained because cancel could not be completed safely.',
          error: this.#host.publicError(error),
        })
      }
    } finally {
      this.#discarding = false
    }
  }

  async #refresh(): Promise<void> {
    const inventory = await this.#controls.refresh()
    if (this.#closed || this.#host.disposed()) {
      inventory.close()
      if (this.#closed) this.#controls.close()
      return
    }
    this.#inventory?.close()
    this.#inventory = inventory
    this.#references.clear()
    for (const reference of inventory.tasks) {
      const id = reference.descriptor.intent.digest
      if (this.#references.has(id)) {
        throw new Error('Paused-task inventory contains a duplicate durable intent')
      }
      this.#references.set(id, reference)
    }
    this.publishRows()
  }

  #setState(id: string, state: V2ReceiverSnapshot['pausedTasks'][number]['state']): void {
    this.#publish({
      ...this.#snapshot(),
      pausedTasks: Object.freeze(this.#snapshot().pausedTasks.map((task) =>
        task.id === id ? Object.freeze({ ...task, state }) : task)),
    })
  }

  #currentShareFor(reference: ResumeStateRef): TransferIntentDraft | undefined {
    const joined = this.#host.joined()
    if (joined === undefined) return undefined
    try {
      const current = createTransferIntentDraft({
        shareInstance: descriptorIdentity(
          joined.descriptor.shareInstanceId,
          joined.descriptor.shareInstance,
          joined.recoveryIdentity,
        ),
        syntheticRoot: descriptorIdentity(
          joined.descriptor.syntheticRootId,
          joined.descriptor.syntheticRoot,
          'synthetic-root',
        ),
        selection: reference.descriptor.intent.selection,
      })
      assertPausedTaskCurrentShare(reference.descriptor, current)
      return current
    } catch {
      return undefined
    }
  }

  #blocked(): boolean {
    return this.active() || this.#host.regularTransferActive()
  }

  #snapshot(): V2ReceiverSnapshot {
    return this.#host.snapshot()
  }

  #publish(snapshot: V2ReceiverSnapshot): void {
    this.#host.publish(snapshot)
  }

  #fail(error: unknown): void {
    this.#publish({
      ...this.#snapshot(),
      phase: 'failed',
      status: 'The receiver stopped safely.',
      error: this.#host.publicError(error),
    })
  }
}

interface BrowserPausedTaskWindow extends V2BrowserOutputWindow {
  confirm(message?: string): boolean
}

interface CloseableResumeStateAuthority extends ResumeStateAuthority {
  removeCompleted(descriptor: PausedTaskDescriptorV1): Promise<void>
  close(): void
}

export interface BrowserV2PausedTaskControlOptions {
  readonly openAuthority?: () => Promise<CloseableResumeStateAuthority>
  readonly windowPort?: BrowserPausedTaskWindow
  readonly onTrace?: PausedTaskTraceListener
}

export class BrowserV2PausedTaskControlPort implements V2PausedTaskControlPort {
  readonly #openAuthority: () => Promise<CloseableResumeStateAuthority>
  readonly #window: BrowserPausedTaskWindow
  #authority: CloseableResumeStateAuthority | undefined
  #inventory: ResumeStateInventory | undefined

  constructor(options: BrowserV2PausedTaskControlOptions = {}) {
    this.#openAuthority = options.openAuthority ?? (() => IndexedDbPausedTaskState.open({
      ...(options.onTrace === undefined ? {} : { onTrace: options.onTrace }),
    }))
    this.#window = options.windowPort ??
      window as unknown as BrowserPausedTaskWindow
  }

  async refresh(): Promise<ResumeStateInventory> {
    const next = await this.#openAuthority()
    let inventory: ResumeStateInventory
    try {
      inventory = await next.listResumeState()
    } catch (error) {
      next.close()
      throw error
    }
    this.#inventory?.close()
    this.#authority?.close()
    this.#authority = next
    this.#inventory = inventory
    return inventory
  }

  confirmDiscard(reference: ResumeStateRef): boolean {
    const backend = reference.descriptor.intent.output.backend
    const completed = reference.completedFileCount
    let completion = 'No completed files were recorded.'
    if (completed > 0) {
      completion = backend === 'origin-private-staging'
        ? `${completed} completed file(s) will be exported as a partial ZIP first.`
        : `${completed} completed file(s) will remain in the selected folder.`
    }
    return this.#window.confirm(
      `Cancel this resumable transfer? ${completion} Incomplete checkpoint data will be discarded.`,
    )
  }

  resume(
    reference: ResumeStateRef,
    currentShare: TransferIntentDraft,
  ): Promise<ReconstructedPausedTask> {
    const authority = this.#requireAuthority()
    try {
      return authority.resume(reference, {
        currentShare,
        ...(reference.descriptor.intent.output.backend === 'origin-private-staging'
          ? {
              acquireOriginPrivateOutput: () =>
                acquireBrowserResumeZipOutput('windshare-resumed.zip', this.#window),
            }
          : {}),
      })
    } catch (error) {
      return Promise.reject(error)
    }
  }

  discard(
    reference: ResumeStateRef,
    currentShare: TransferIntentDraft,
  ): Promise<ResumeStateDiscardResult> {
    const authority = this.#requireAuthority()
    try {
      return authority.discard(reference, {
        currentShare,
        ...(reference.descriptor.intent.output.backend === 'origin-private-staging' &&
            reference.completedFileCount > 0
          ? {
              acquireOriginPrivateOutput: () =>
                acquireBrowserResumeZipOutput('windshare-completed-partial.zip', this.#window),
            }
          : {}),
      })
    } catch (error) {
      return Promise.reject(error)
    }
  }

  removeCompleted(descriptor: PausedTaskDescriptorV1): Promise<void> {
    return this.#requireAuthority().removeCompleted(descriptor)
  }

  close(): void {
    this.#inventory?.close()
    this.#inventory = undefined
    this.#authority?.close()
    this.#authority = undefined
  }

  #requireAuthority(): CloseableResumeStateAuthority {
    if (this.#authority === undefined) {
      throw new DOMException('Paused-task controls have not loaded their inventory', 'InvalidStateError')
    }
    return this.#authority
  }
}
