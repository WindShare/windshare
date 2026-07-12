import type { CapabilityLink, TransferPlan } from '../contracts'
import { mergeCapabilityLink, parseCapabilityLink } from '../crypto'
import type { InitialCapability, NavigationPort } from './capability-source'
import { consumeLocationCapability } from './capability-source'
import {
  EntrySelectionModel,
  ReceiverPublicError,
  emptyProgress,
  type JoinedShare,
  type OutputChoiceId,
  type ReceiverGateway,
  type ReceiverPhase,
  type ReceiverSnapshot,
  type ReceiverTransferObserver,
  type TransferProgress,
} from './model'
import { SelectionPageWindow } from './selection-window'

type SnapshotListener = () => void

const INITIAL_STATUS = 'Preparing the receiver.'
const INVALID_KEY_MESSAGE = 'The separate key is invalid.'
const JOIN_FAILURE_MESSAGE = 'Could not connect to this share.'
const PLAN_FAILURE_MESSAGE = 'Could not prepare the selected files.'
const TRANSFER_FAILURE_MESSAGE = 'The download could not be completed.'

function immutableChoices(gateway: ReceiverGateway) {
  return Object.freeze(gateway.outputChoices.map((choice) => Object.freeze({ ...choice })))
}

function selectedOutputChoice(gateway: ReceiverGateway): OutputChoiceId {
  return gateway.outputChoices.find((choice) => choice.available)?.id ?? 'download'
}

function phaseStatus(phase: ReceiverPhase): string {
  switch (phase) {
    case 'awaiting-key':
      return 'Enter the separate key to open this share.'
    case 'joining':
      return 'Connecting to the share.'
    case 'planning':
      return 'Preparing the selected files.'
    case 'ready':
      return 'Choose what to save, then start the download.'
    case 'preparing-output':
      return 'Waiting for a save location.'
    case 'transferring':
      return 'Downloading selected files.'
    case 'reconnecting':
      return 'The connection was interrupted. Reconnecting.'
    case 'aborting':
      return 'Stopping the download and cleaning up partial output.'
    case 'completed':
      return 'Download complete.'
    case 'failed':
      return 'Download failed.'
    case 'aborted':
      return 'Download stopped and partial output cleaned up.'
  }
}

function publicMessage(error: unknown, fallback: string): string {
  return error instanceof ReceiverPublicError ? error.message : fallback
}

function isOutputRecovery(error: unknown): boolean {
  return (
    error instanceof ReceiverPublicError &&
    (error.code === 'output-cancelled' || error.code === 'output-unavailable')
  )
}

function isAbortError(error: unknown): boolean {
  return (
    typeof error === 'object' &&
    error !== null &&
    'name' in error &&
    (error as { readonly name?: unknown }).name === 'AbortError'
  )
}

function mergeSubmittedCapability(bareUrl: string, keyInput: string): CapabilityLink {
  const capability = mergeCapabilityLink(bareUrl, keyInput)
  const trimmed = keyInput.trim()
  if (trimmed.indexOf('#') <= 0) {
    return capability
  }

  let suppliedLink: CapabilityLink | undefined
  try {
    suppliedLink = parseCapabilityLink(trimmed)
    if (suppliedLink.shareId !== capability.shareId) {
      throw new TypeError('The supplied link names a different share')
    }
    return capability
  } catch (error) {
    capability.readSecret.fill(0)
    throw error
  } finally {
    suppliedLink?.readSecret.fill(0)
  }
}

export class ReceiverController {
  readonly #gateway: ReceiverGateway
  readonly #listeners = new Set<SnapshotListener>()
  readonly #lifetime = new AbortController()
  readonly #selectionWindow = new SelectionPageWindow()

  #snapshot: ReceiverSnapshot
  #bareUrl: string | undefined
  #share: JoinedShare | undefined
  #selectionModel: EntrySelectionModel | undefined
  #selection: readonly boolean[] = Object.freeze([])
  #plan: TransferPlan | undefined
  #selectionRevision = 0
  #compiledRevision = -1
  #compileRunning = false
  #generation = 0
  #transferRun = 0
  #transferAbort: AbortController | undefined
  #transferTask: Promise<void> | undefined
  #userAborted = false

  constructor(gateway: ReceiverGateway) {
    this.#gateway = gateway
    const outputChoices = immutableChoices(gateway)
    this.#snapshot = Object.freeze({
      phase: 'joining',
      status: INITIAL_STATUS,
      error: null,
      entries: Object.freeze([]),
      manifestEntryCount: 0,
      selectionPageIndex: 0,
      selectionPageCount: 0,
      selectedBytes: 0,
      selectedEntryCount: 0,
      outputChoices,
      outputChoice: selectedOutputChoice(gateway),
      progress: emptyProgress(),
      reconnectAttempt: 0,
      canStart: false,
    })
  }

  readonly subscribe = (listener: SnapshotListener): (() => void) => {
    this.#listeners.add(listener)
    return () => this.#listeners.delete(listener)
  }

  readonly getSnapshot = (): ReceiverSnapshot => this.#snapshot

  initialize(initial: InitialCapability): void {
    const generation = ++this.#generation
    if (initial.kind === 'invalid') {
      this.#publish({
        phase: 'failed',
        status: phaseStatus('failed'),
        error: initial.message,
      })
      return
    }
    if (initial.kind === 'needs-key') {
      this.#bareUrl = initial.bareUrl
      this.#publish({
        phase: 'awaiting-key',
        status: phaseStatus('awaiting-key'),
        error: null,
      })
      return
    }
    this.#beginJoin(initial.capability, generation)
  }

  submitKey(keyInput: string): void {
    if (this.#snapshot.phase !== 'awaiting-key' || this.#bareUrl === undefined) {
      return
    }
    const bareUrl = this.#bareUrl
    let capability
    try {
      capability = mergeSubmittedCapability(bareUrl, keyInput)
    } catch {
      this.#publish({ error: INVALID_KEY_MESSAGE })
      return
    }
    const generation = ++this.#generation
    this.#beginJoin(capability, generation, bareUrl)
  }

  toggleSelection(path: string): void {
    if (
      this.#selectionModel === undefined ||
      (this.#snapshot.phase !== 'ready' && this.#snapshot.phase !== 'planning')
    ) {
      return
    }
    this.#selection = this.#selectionModel.toggle(this.#selection, path)
    this.#selectionRevision += 1
    this.#plan = undefined
    this.#publishSelection('planning', null)
    this.#scheduleCompile()
  }

  chooseOutput(choice: OutputChoiceId): void {
    if (
      this.#snapshot.phase !== 'ready' &&
      this.#snapshot.phase !== 'planning'
    ) {
      return
    }
    const available = this.#snapshot.outputChoices.some(
      (option) => option.id === choice && option.available,
    )
    if (available) {
      this.#publish({ outputChoice: choice, error: null })
    }
  }

  showSelectionPage(pageIndex: number): void {
    const model = this.#selectionModel
    if (
      model === undefined ||
      (this.#snapshot.phase !== 'ready' && this.#snapshot.phase !== 'planning')
    ) {
      return
    }
    const window = this.#selectionWindow.moveTo(model, this.#selection, pageIndex)
    if (window !== undefined) this.#publish(window)
  }

  /** Called directly from the click handler; it performs no await before `start`. */
  startDownload(): void {
    const share = this.#share
    const plan = this.#plan
    if (
      this.#snapshot.phase !== 'ready' ||
      share === undefined ||
      plan === undefined ||
      plan.selectedEntries.length === 0
    ) {
      return
    }

    const run = ++this.#transferRun
    const abort = new AbortController()
    this.#transferAbort = abort
    this.#userAborted = false
    const progress = Object.freeze({
      ...emptyProgress(),
      totalBytes: plan.selectedBytes,
      totalBlocks: plan.chunks.count,
    })
    this.#publish({
      phase: 'preparing-output',
      status: phaseStatus('preparing-output'),
      error: null,
      progress,
      reconnectAttempt: 0,
      canStart: false,
    })

    const observer = this.#observer(run)
    let task: Promise<void>
    try {
      task = this.#gateway.start(
        share,
        plan,
        this.#snapshot.outputChoice,
        observer,
        abort.signal,
      )
    } catch (error) {
      this.#finishTransfer(run, error)
      return
    }
    this.#transferTask = Promise.resolve(task)
    this.#transferTask.then(
      () => this.#finishTransfer(run),
      (error: unknown) => this.#finishTransfer(run, error),
    )
  }

  abortDownload(): void {
    if (
      this.#snapshot.phase !== 'preparing-output' &&
      this.#snapshot.phase !== 'transferring' &&
      this.#snapshot.phase !== 'reconnecting'
    ) {
      return
    }
    this.#userAborted = true
    this.#publish({
      phase: 'aborting',
      status: phaseStatus('aborting'),
      error: null,
      canStart: false,
    })
    this.#transferAbort?.abort(new DOMException('Download stopped by the user', 'AbortError'))
  }

  async dispose(): Promise<void> {
    ++this.#generation
    ++this.#transferRun
    this.#lifetime.abort(new DOMException('Receiver closed', 'AbortError'))
    this.#transferAbort?.abort(new DOMException('Receiver closed', 'AbortError'))
    await this.#transferTask?.catch(() => undefined)
    await this.#share?.close().catch(() => undefined)
    this.#listeners.clear()
  }

  #beginJoin(
    capability: Parameters<ReceiverGateway['join']>[0],
    generation: number,
    retryBareUrl?: string,
  ): void {
    this.#publish({
      phase: 'joining',
      status: phaseStatus('joining'),
      error: null,
      canStart: false,
    })
    let joinTask: Promise<JoinedShare>
    try {
      joinTask = this.#gateway.join(capability, this.#lifetime.signal)
    } catch (error) {
      this.#joinFailed(error, generation, retryBareUrl)
      return
    } finally {
      // The gateway contract snapshots its owned copy before returning. Destroying
      // this parsing buffer here prevents a later rejection from retaining a second
      // secret-bearing object in the controller's promise closure.
      capability.readSecret.fill(0)
    }
    joinTask.then(
      (share) => this.#joined(share, generation),
      (error: unknown) => this.#joinFailed(error, generation, retryBareUrl),
    )
  }

  #joinFailed(error: unknown, generation: number, retryBareUrl?: string): void {
    if (generation !== this.#generation || this.#lifetime.signal.aborted) {
      return
    }
    const retryingSeparateKey = retryBareUrl !== undefined
    if (retryingSeparateKey) {
      // Retain only the public bare link. A retry must require fresh key entry,
      // never a raw string or an abandoned decoded secret from the failed join.
      this.#bareUrl = retryBareUrl
    }
    const phase = retryingSeparateKey ? 'awaiting-key' : 'failed'
    this.#publish({
      phase,
      status: phaseStatus(phase),
      error: publicMessage(error, JOIN_FAILURE_MESSAGE),
      canStart: false,
    })
  }

  #joined(share: JoinedShare, generation: number): void {
    if (generation !== this.#generation || this.#lifetime.signal.aborted) {
      share.close().catch(() => undefined)
      return
    }
    this.#bareUrl = undefined
    this.#share = share
    this.#selectionModel = new EntrySelectionModel(share.manifest.entries)
    this.#selection = this.#selectionModel.defaultSelection()
    this.#selectionWindow.reset()
    this.#selectionRevision += 1
    this.#plan = undefined
    this.#publishSelection('planning', null)
    this.#scheduleCompile()
  }

  #scheduleCompile(): void {
    if (this.#compileRunning) {
      return
    }
    queueMicrotask(() => {
      this.#drainCompiles().catch(() => undefined)
    })
  }

  async #drainCompiles(): Promise<void> {
    if (this.#compileRunning) {
      return
    }
    this.#compileRunning = true
    const generation = this.#generation
    try {
      while (
        this.#compiledRevision !== this.#selectionRevision &&
        generation === this.#generation
      ) {
        if (!(await this.#compileRevision(generation))) {
          return
        }
      }
    } catch (error) {
      if (generation === this.#generation && !this.#lifetime.signal.aborted) {
        const share = this.#share
        this.#share = undefined
        share?.close().catch(() => undefined)
        this.#publish({
          phase: 'failed',
          status: phaseStatus('failed'),
          error: publicMessage(error, PLAN_FAILURE_MESSAGE),
          canStart: false,
        })
      }
    } finally {
      this.#compileRunning = false
      if (
        generation === this.#generation &&
        this.#compiledRevision !== this.#selectionRevision &&
        this.#snapshot.phase === 'planning'
      ) {
        this.#scheduleCompile()
      }
    }
  }

  async #compileRevision(generation: number): Promise<boolean> {
    const revision = this.#selectionRevision
    const model = this.#selectionModel
    const share = this.#share
    if (model === undefined || share === undefined) {
      return false
    }
    const plan = await this.#gateway.compileSelection(
      share,
      model.selectors(this.#selection),
      this.#lifetime.signal,
    )
    if (generation !== this.#generation || this.#lifetime.signal.aborted) {
      return false
    }
    this.#compiledRevision = revision
    if (revision !== this.#selectionRevision) {
      return true
    }
    if (plan.selectedBytes !== model.selectedBytes(this.#selection)) {
      throw new Error('Compiled plan does not match the visible selection')
    }
    this.#plan = plan
    this.#publishSelection('ready', null)
    return true
  }

  #observer(run: number): ReceiverTransferObserver {
    return Object.freeze({
      started: (progress: TransferProgress) => {
        if (run === this.#transferRun) {
          this.#publish({
            phase: 'transferring',
            status: phaseStatus('transferring'),
            progress: Object.freeze({ ...progress }),
            reconnectAttempt: 0,
          })
        }
      },
      progress: (progress: TransferProgress) => {
        if (run === this.#transferRun) {
          this.#publish({ progress: Object.freeze({ ...progress }) })
        }
      },
      reconnecting: (attempt: number) => {
        if (run === this.#transferRun) {
          this.#publish({
            phase: 'reconnecting',
            status: phaseStatus('reconnecting'),
            reconnectAttempt: attempt,
          })
        }
      },
      reconnected: (progress: TransferProgress) => {
        if (run === this.#transferRun) {
          this.#publish({
            phase: 'transferring',
            status: phaseStatus('transferring'),
            progress: Object.freeze({ ...progress }),
            reconnectAttempt: 0,
          })
        }
      },
    })
  }

  #finishTransfer(run: number, error?: unknown): void {
    if (run !== this.#transferRun) {
      return
    }
    this.#transferRun += 1
    this.#transferTask = undefined
    this.#transferAbort = undefined
    const userAborted = this.#userAborted
    this.#userAborted = false

    if (
      userAborted &&
      (error === undefined || isAbortError(error) || isOutputRecovery(error))
    ) {
      this.#publish({
        phase: 'aborted',
        status: phaseStatus('aborted'),
        error: null,
        reconnectAttempt: 0,
        canStart: false,
      })
      return
    }
    if (error === undefined) {
      this.#publish({
        phase: 'completed',
        status: phaseStatus('completed'),
        error: null,
        reconnectAttempt: 0,
        canStart: false,
      })
      return
    }
    if (isOutputRecovery(error)) {
      this.#publishSelection('ready', publicMessage(error, TRANSFER_FAILURE_MESSAGE))
      return
    }
    this.#publish({
      phase: 'failed',
      status: phaseStatus('failed'),
      error: publicMessage(error, TRANSFER_FAILURE_MESSAGE),
      reconnectAttempt: 0,
      canStart: false,
    })
  }

  #publishSelection(phase: 'planning' | 'ready', error: string | null): void {
    const model = this.#selectionModel
    const selectionWindow = this.#selectionWindow.snapshot(model, this.#selection)
    const selectedEntryCount = model?.selectedEntryCount(this.#selection) ?? 0
    const outputAvailable = this.#snapshot.outputChoices.some(
      (choice) => choice.id === this.#snapshot.outputChoice && choice.available,
    )
    this.#publish({
      phase,
      status: phaseStatus(phase),
      error,
      ...selectionWindow,
      selectedBytes: model?.selectedBytes(this.#selection) ?? 0,
      selectedEntryCount,
      progress: emptyProgress(),
      reconnectAttempt: 0,
      canStart: phase === 'ready' && selectedEntryCount > 0 && outputAvailable,
    })
  }

  #publish(patch: Partial<ReceiverSnapshot>): void {
    this.#snapshot = Object.freeze({ ...this.#snapshot, ...patch })
    for (const listener of this.#listeners) {
      listener()
    }
  }
}

export function createReceiverController(
  gateway: ReceiverGateway,
  navigation: NavigationPort,
): ReceiverController {
  const initial = consumeLocationCapability(navigation)
  const controller = new ReceiverController(gateway)
  controller.initialize(initial)
  return controller
}
