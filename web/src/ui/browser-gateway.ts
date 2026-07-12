import type {
  BlockSink,
  CapabilityLink,
  ChunkIndex,
  DeliveryOrder,
  TransferPlan,
} from '../contracts'
import {
  BrowserOfferChannelFactory,
  BrowserReceiverConnectivity,
  browserConnectivityClock,
  type ConnectivityClock,
  type OfferChannelFactory,
} from '../connectivity'
import { bytesToHex, createChunkOpener } from '../crypto'
import { createDownloadSink, type DownloadTarget } from '../download'
import {
  ManifestError,
  PackedLayout,
  compileTransferPlan,
  createSelection,
  openCapabilityManifest,
  validateCanonicalPath,
} from '../manifest'
import {
  PeerSessionError,
  ReceiveSession,
  ReceiveSessionPreparation,
} from '../session'
import {
  RelayReceiverConnection,
  dialRelayReceiver,
} from '../transport/relay'
import {
  BrowserOutputPreparationFailure,
  browserOutputChoices,
  prepareBrowserOutput,
  type BrowserOutputPreparer,
  type PreparedBrowserOutput,
} from './browser-output'
import {
  ReceiverPublicError,
  type JoinedShare,
  type OutputChoice,
  type OutputChoiceId,
  type ReceiverGateway,
  type ReceiverTransferObserver,
  type TransferProgress,
} from './model'

const PROGRESS_POLL_MS = 250
const MAX_RECONNECT_ATTEMPTS = 3
export const RELAY_REJOIN_RETRY_DELAY_MS = 250
const MAX_PEER_MESSAGE_CHARACTERS = 240
const WHITESPACE = /\s+/gu
const UNSAFE_PEER_CHARACTERS = /[\p{Cc}\p{Cf}\p{Zl}\p{Zp}]+/gu
const PEER_ERROR_PREFIX = /^peer reported error 0x[0-9a-f]{4}:\s*/iu

function abortLike(error: unknown): boolean {
  return (
    typeof error === 'object' &&
    error !== null &&
    'name' in error &&
    (error as { readonly name?: unknown }).name === 'AbortError'
  )
}

export function sanitizePeerTerminalMessage(error: PeerSessionError): string {
  const normalized = error.message
    .replace(UNSAFE_PEER_CHARACTERS, ' ')
    .replace(PEER_ERROR_PREFIX, '')
    .replace(WHITESPACE, ' ')
    .trim()
  const text = Array.from(normalized).slice(0, MAX_PEER_MESSAGE_CHARACTERS).join('')
  return text === '' ? 'The sender stopped the transfer.' : `The sender stopped the transfer: ${text}`
}

function publicOutputError(error: unknown): ReceiverPublicError {
  if (error instanceof ReceiverPublicError) {
    return error
  }
  if (abortLike(error)) {
    return new ReceiverPublicError('output-cancelled', 'The save prompt was canceled.')
  }
  return new ReceiverPublicError(
    'output-unavailable',
    'The selected save destination is not available.',
  )
}

function snapshotCapability(capability: CapabilityLink): CapabilityLink {
  return Object.freeze({
    suite: capability.suite,
    shareId: capability.shareId,
    readSecret: capability.readSecret.slice() as CapabilityLink['readSecret'],
    relayHints: Object.freeze([...capability.relayHints]),
  })
}

function publicTransferError(error: unknown): unknown {
  if (error instanceof ReceiverPublicError || abortLike(error)) {
    return error
  }
  if (error instanceof PeerSessionError) {
    return new ReceiverPublicError('peer-terminal', sanitizePeerTerminalMessage(error))
  }
  return new ReceiverPublicError(
    'transfer-failed',
    'The download could not be completed safely.',
  )
}

async function settleOutputPreparationFailure(error: unknown): Promise<unknown> {
  if (!(error instanceof BrowserOutputPreparationFailure)) {
    return error
  }
  try {
    await error.retryCleanup()
    return error.primary
  } catch (cleanupError) {
    // Keeping this as an aggregate prevents an abort-like creation failure from
    // being reported as a clean picker cancellation after cleanup exhaustion.
    return error.withCleanupFailure(cleanupError)
  }
}

class BrowserJoinedShare implements JoinedShare {
  readonly manifest
  readonly layout
  readonly capability: CapabilityLink
  readonly fingerprint: string
  connection: RelayReceiverConnection
  #closeTask: Promise<void> | undefined

  constructor(
    capability: CapabilityLink,
    connection: RelayReceiverConnection,
    manifest: Awaited<ReturnType<typeof openCapabilityManifest>>,
  ) {
    this.capability = capability
    this.connection = connection
    this.manifest = manifest.manifest
    this.layout = new PackedLayout(manifest.manifest)
    this.fingerprint = bytesToHex(manifest.fingerprint)
  }

  close(): Promise<void> {
    this.#closeTask ??= (async () => {
      this.capability.readSecret.fill(0)
      await this.connection.close()
    })()
    return this.#closeTask
  }
}

class TransferProgressCounter {
  readonly #plan: TransferPlan
  readonly #layout: PackedLayout
  readonly #selectedFiles: ReadonlySet<string>
  #writtenBytes = 0
  #completedBlocks = 0

  constructor(plan: TransferPlan, layout: PackedLayout) {
    this.#plan = plan
    this.#layout = layout
    this.#selectedFiles = new Set(
      plan.selectedEntries
        .filter((entry) => entry.kind === 'file')
        .map((entry) => entry.path),
    )
  }

  accepted(index: ChunkIndex): void {
    for (const range of this.#layout.chunkRanges(index)) {
      if (this.#selectedFiles.has(range.path)) {
        this.#writtenBytes += range.length
      }
    }
    this.#completedBlocks += 1
  }

  snapshot(session?: ReceiveSession): TransferProgress {
    const state = session?.snapshot()
    return Object.freeze({
      writtenBytes: Math.min(this.#writtenBytes, this.#plan.selectedBytes),
      totalBytes: this.#plan.selectedBytes,
      completedBlocks: this.#completedBlocks,
      totalBlocks: this.#plan.chunks.count,
      retryBlocks: state?.retryBlocks ?? 0,
      channels: state?.channels ?? 0,
      bufferedBlocks: state?.bufferedBlocks ?? 0,
      maxBufferedBlocks: state?.maxBufferedBlocks ?? 0,
    })
  }
}

class ObservedSink implements BlockSink {
  readonly deliveryOrder: DeliveryOrder
  readonly #sink: BlockSink
  readonly #accepted: (index: ChunkIndex) => void

  constructor(sink: BlockSink, accepted: (index: ChunkIndex) => void) {
    this.#sink = sink
    this.deliveryOrder = sink.deliveryOrder
    this.#accepted = accepted
  }

  has(index: ChunkIndex): boolean {
    return this.#sink.has(index)
  }

  async writeBlock(index: ChunkIndex, plaintext: Uint8Array): Promise<void> {
    await this.#sink.writeBlock(index, plaintext)
    this.#accepted(index)
  }

  finalize(): Promise<void> {
    return this.#sink.finalize()
  }

  abort(reason: unknown): Promise<void> {
    return this.#sink.abort(reason)
  }
}

function sinkFor(
  share: BrowserJoinedShare,
  plan: TransferPlan,
  target: DownloadTarget,
): BlockSink {
  return createDownloadSink(
    {
      plan,
      layout: share.layout,
      validatePath: validateCanonicalPath,
    },
    target,
  )
}

export interface BrowserGatewayRuntime {
  readonly dialReceiver: typeof dialRelayReceiver
  readonly openManifest: typeof openCapabilityManifest
  readonly offerFactory?: OfferChannelFactory
  readonly connectivityClock?: ConnectivityClock
}

interface NormalizedBrowserGatewayRuntime {
  readonly dialReceiver: typeof dialRelayReceiver
  readonly openManifest: typeof openCapabilityManifest
  readonly offerFactory: OfferChannelFactory
  readonly connectivityClock: ConnectivityClock
}

const DEFAULT_GATEWAY_RUNTIME: NormalizedBrowserGatewayRuntime = Object.freeze({
  dialReceiver: dialRelayReceiver,
  openManifest: openCapabilityManifest,
  offerFactory: new BrowserOfferChannelFactory(),
  connectivityClock: browserConnectivityClock,
})

function requireBrowserShare(share: JoinedShare): BrowserJoinedShare {
  if (!(share instanceof BrowserJoinedShare)) {
    throw new TypeError('Browser gateway received a foreign joined-share capability')
  }
  return share
}

export class BrowserReceiverGateway implements ReceiverGateway {
  readonly outputChoices: readonly OutputChoice[]
  readonly #prepareOutput: BrowserOutputPreparer
  readonly #runtime: NormalizedBrowserGatewayRuntime

  constructor(
    prepareOutput: BrowserOutputPreparer = prepareBrowserOutput,
    outputChoices: readonly OutputChoice[] = browserOutputChoices(),
    runtime: BrowserGatewayRuntime = DEFAULT_GATEWAY_RUNTIME,
  ) {
    this.#prepareOutput = prepareOutput
    this.outputChoices = Object.freeze([...outputChoices])
    this.#runtime = Object.freeze({
      dialReceiver: runtime.dialReceiver,
      openManifest: runtime.openManifest,
      offerFactory: runtime.offerFactory ?? DEFAULT_GATEWAY_RUNTIME.offerFactory,
      connectivityClock: runtime.connectivityClock ?? DEFAULT_GATEWAY_RUNTIME.connectivityClock,
    })
  }

  async join(capability: CapabilityLink, signal: AbortSignal): Promise<JoinedShare> {
    const ownedCapability = snapshotCapability(capability)
    const relayUrl = ownedCapability.relayHints[0]
    if (relayUrl === undefined || relayUrl === '') {
      ownedCapability.readSecret.fill(0)
      throw new ReceiverPublicError(
        'missing-relay',
        'This share link does not include a relay address.',
      )
    }
    let connection: RelayReceiverConnection | undefined
    try {
      connection = await this.#runtime.dialReceiver(
        { relayUrl, shareId: ownedCapability.shareId },
        signal,
      )
      const opened = await this.#runtime.openManifest(
        ownedCapability,
        connection.sealedManifest,
      )
      return new BrowserJoinedShare(ownedCapability, connection, opened)
    } catch (error) {
      ownedCapability.readSecret.fill(0)
      await connection?.close().catch(() => undefined)
      if (abortLike(error)) {
        throw error
      }
      if (error instanceof ManifestError) {
        throw new ReceiverPublicError(
          'invalid-capability',
          'The share link or separate key is invalid.',
        )
      }
      if (error instanceof ReceiverPublicError) {
        throw error
      }
      throw new ReceiverPublicError(
        'connection-failed',
        'Could not connect to this share.',
      )
    }
  }

  async compileSelection(
    joined: JoinedShare,
    selectors: readonly string[] | null,
    signal: AbortSignal,
  ): Promise<TransferPlan> {
    signal.throwIfAborted()
    const share = requireBrowserShare(joined)
    const plan = await compileTransferPlan(share.layout, createSelection(selectors))
    signal.throwIfAborted()
    return plan
  }

  start(
    joined: JoinedShare,
    plan: TransferPlan,
    outputChoice: OutputChoiceId,
    observer: ReceiverTransferObserver,
    signal: AbortSignal,
  ): Promise<void> {
    // This must be the first externally observable operation: picker activation
    // and every future D4 offer/ICE action remain behind the same click boundary.
    let outputRequest: Promise<PreparedBrowserOutput>
    try {
      outputRequest = this.#prepareOutput(plan, outputChoice)
    } catch (error) {
      outputRequest = Promise.reject(error)
    }
    return this.#runTransfer(joined, plan, observer, signal, outputRequest)
  }

  async #runTransfer(
    joined: JoinedShare,
    plan: TransferPlan,
    observer: ReceiverTransferObserver,
    externalSignal: AbortSignal,
    outputRequest: Promise<PreparedBrowserOutput>,
  ): Promise<void> {
    let output: PreparedBrowserOutput
    try {
      output = await outputRequest
    } catch (error) {
      throw publicOutputError(await settleOutputPreparationFailure(error))
    }

    let share: BrowserJoinedShare | undefined
    let lifetime: AbortController | undefined
    let abortFromCaller: (() => void) | undefined
    let poll: ReturnType<typeof setInterval> | undefined
    let monitor: Promise<void> | undefined
    let connectivity: BrowserReceiverConnectivity | undefined
    let sessionPreparation: ReceiveSessionPreparation | undefined
    try {
      // Output acquisition is the ownership boundary. Validation belongs inside
      // it because even a foreign capability must not orphan picker-created data.
      const validatedShare = requireBrowserShare(joined)
      share = validatedShare
      const activeLifetime = new AbortController()
      lifetime = activeLifetime
      const callerAbort = () => activeLifetime.abort(externalSignal.reason)
      abortFromCaller = callerAbort
      if (externalSignal.aborted) {
        callerAbort()
      } else {
        externalSignal.addEventListener('abort', callerAbort, { once: true })
      }

      activeLifetime.signal.throwIfAborted()
      const counter = new TransferProgressCounter(plan, validatedShare.layout)
      const sessionHolder: { value: ReceiveSession | undefined } = { value: undefined }
      const opener = await createChunkOpener(
        validatedShare.capability.suite,
        validatedShare.capability.readSecret,
        validatedShare.manifest.chunkSize,
      )
      activeLifetime.signal.throwIfAborted()
      sessionPreparation = output.transferTarget(
        (target) => new ReceiveSessionPreparation(
          new ObservedSink(sinkFor(validatedShare, plan, target), (index) => {
            counter.accepted(index)
            observer.progress(counter.snapshot(sessionHolder.value))
          }),
        ),
      )
      sessionPreparation.prepare(plan, opener, {
        maxBlockBytes: opener.maxSealedBytes,
      })
      connectivity = new BrowserReceiverConnectivity(
        sessionPreparation,
        this.#runtime.offerFactory,
        {
          clock: this.#runtime.connectivityClock,
        },
      )
      await connectivity.start(
        plan.selectedBytes,
        validatedShare.connection.channel,
        activeLifetime.signal,
      )
      // Observer code may synchronously cancel or throw. Transfer first so that
      // ReceiveSession is already the sole sink cleanup owner at that boundary.
      const started = sessionPreparation.transfer(activeLifetime.signal)
      const { session } = started
      sessionHolder.value = session
      observer.started(counter.snapshot(session))
      poll = setInterval(() => observer.progress(counter.snapshot(session)), PROGRESS_POLL_MS)
      monitor = this.#monitorConnection(
        validatedShare,
        session,
        connectivity,
        counter,
        observer,
        activeLifetime,
      ).catch((error: unknown) => {
        if (!activeLifetime.signal.aborted) {
          activeLifetime.abort(error)
        }
      })
      await started.completion
      activeLifetime.abort(new DOMException('Transfer session settled', 'AbortError'))
      await validatedShare.close().catch(() => undefined)
      await monitor
      await output.commit()
    } catch (error) {
      lifetime?.abort(error)
      let failure = sessionPreparation === undefined
        ? error
        : await sessionPreparation.settleFailure(error)
      try {
        await output.abort(error)
      } catch (cleanupError) {
        failure = new AggregateError(
          [failure, cleanupError],
          'Transfer failed and prepared output cleanup also failed',
          { cause: error },
        )
      }
      throw publicTransferError(failure)
    } finally {
      if (poll !== undefined) {
        clearInterval(poll)
      }
      if (abortFromCaller !== undefined) {
        externalSignal.removeEventListener('abort', abortFromCaller)
      }
      lifetime?.abort(new DOMException('Transfer stopped', 'AbortError'))
      await connectivity?.close().catch(() => undefined)
      // Closing before and after the monitor settles covers the narrow race in
      // which a replacement connection completes while cancellation propagates.
      await share?.close().catch(() => undefined)
      await monitor?.catch(() => undefined)
      await share?.close().catch(() => undefined)
    }
  }

  async #monitorConnection(
    share: BrowserJoinedShare,
    session: ReceiveSession,
    connectivity: BrowserReceiverConnectivity,
    counter: TransferProgressCounter,
    observer: ReceiverTransferObserver,
    lifetime: AbortController,
  ): Promise<void> {
    while (!lifetime.signal.aborted && session.state === 'running') {
      const disconnected = share.connection
      await disconnected.done
      if (lifetime.signal.aborted || session.state !== 'running') {
        return
      }

      let replacement: RelayReceiverConnection
      try {
        replacement = await this.#restoreConnection(
          share,
          disconnected,
          observer,
          lifetime.signal,
        )
      } catch (error) {
        if (
          error instanceof ReceiverPublicError &&
          error.code === 'connection-failed' &&
          connectivity.peerAvailable
        ) {
          await this.#runtime.connectivityClock.sleep(
            RELAY_REJOIN_RETRY_DELAY_MS,
            lifetime.signal,
          )
          continue
        }
        throw error
      }
      if (lifetime.signal.aborted || session.state !== 'running') {
        await replacement.close().catch(() => undefined)
        return
      }
      share.connection = replacement
      connectivity.replaceRelay(replacement.channel)
      observer.reconnected(counter.snapshot(session))
    }
  }

  async #restoreConnection(
    share: BrowserJoinedShare,
    disconnected: RelayReceiverConnection,
    observer: ReceiverTransferObserver,
    signal: AbortSignal,
  ): Promise<RelayReceiverConnection> {
    for (let attempt = 1; attempt <= MAX_RECONNECT_ATTEMPTS; attempt += 1) {
      signal.throwIfAborted()
      observer.reconnecting(attempt)
      let replacement: RelayReceiverConnection | undefined
      try {
        replacement = await disconnected.rejoin(signal)
        signal.throwIfAborted()
        const opened = await this.#runtime.openManifest(
          share.capability,
          replacement.sealedManifest,
        )
        signal.throwIfAborted()
        if (bytesToHex(opened.fingerprint) !== share.fingerprint) {
          throw new ReceiverPublicError(
            'manifest-changed',
            'The share changed while reconnecting, so the download was stopped.',
          )
        }
        return replacement
      } catch (error) {
        await replacement?.close().catch(() => undefined)
        if (error instanceof ManifestError) {
          throw new ReceiverPublicError(
            'manifest-changed',
            'The share manifest was invalid after reconnecting, so the download was stopped.',
          )
        }
        if (error instanceof ReceiverPublicError || abortLike(error)) {
          throw error
        }
        if (attempt < MAX_RECONNECT_ATTEMPTS) {
          await this.#runtime.connectivityClock.sleep(
            RELAY_REJOIN_RETRY_DELAY_MS,
            signal,
          )
        }
      }
    }
    throw new ReceiverPublicError(
      'connection-failed',
      'The share connection could not be restored.',
    )
  }
}
