import {
  BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS,
  DEFAULT_PORTABLE_ARTIFACT_LIMIT,
  DEFAULT_PORTABLE_ASSEMBLY_PART_BYTES,
  DEFAULT_PORTABLE_MAXIMUM_PARTS,
  validateReceiveIntent,
  type ReceiveIntent,
} from '../../transfer/intent'
import {
  assertIssuedPortableArtifactAdmission,
  type PortableArtifactAdmission,
} from './admission'

export {
  issuePortableArtifactAdmission,
  type PortableArtifactAdmission,
  type PortableSealedArtifactEvidence,
} from './admission'

const MEBIBYTE_BYTES = 1024 * 1024

export const PORTABLE_HANDOFF_MAXIMUM_BYTES = Number(DEFAULT_PORTABLE_ARTIFACT_LIMIT)
export const PORTABLE_HANDOFF_PART_BYTES = Number(DEFAULT_PORTABLE_ASSEMBLY_PART_BYTES)
export const PORTABLE_HANDOFF_MAXIMUM_PARTS = Number(DEFAULT_PORTABLE_MAXIMUM_PARTS)
export const BROWSER_HANDOFF_OBJECT_URL_LEASE_MS = Number(
  BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS,
)

export type ExternalHandoffFailureReason = 'target-unavailable'
export type PortableRestartReason = 'portable-aborted' | 'preparation-invalidated'

export interface PortableAssemblySnapshot {
  readonly bufferedBytes: number
  readonly retainedParts: number
  readonly rejectedWriteBytes: number
}

export interface DownloadStarted {
  readonly kind: 'download-started'
  readonly suggestedName: string
  // Portable results omit this field because no durable recovery state exists.
  readonly retryableUntil?: number
}

export interface PortableHandoffContext {
  readonly attemptKind: 'portable'
  readonly operationId: string
  readonly attemptId: string
}

export interface PackagedArtifactHandoffContext {
  readonly attemptKind: 'workspace'
  readonly operationId: string
  readonly attemptId: string
  readonly packageDigest: string
  readonly retryableUntil: number
}

export type BrowserHandoffContext =
  | PortableHandoffContext
  | PackagedArtifactHandoffContext

export interface BrowserHandoffRequest {
  readonly context: BrowserHandoffContext
  // File is a Blob subtype, allowing a later sealed OPFS package to use this port
  // without introducing a publication attempt into ReceiveIntent.
  readonly source: Blob
  readonly exactBytes: bigint
  readonly suggestedName: string
  readonly objectUrlLeaseMilliseconds: number
}

export type BrowserHandoffTraceEvent =
  | Readonly<{
      name: 'receive.handoff.started'
      operationId: string
      attemptKind: BrowserHandoffContext['attemptKind']
      attemptId: string
      packageDigestPresent: boolean
      packageDigest?: string
      objectUrlLeaseMilliseconds: number
    }>
  | Readonly<{
      name: 'receive.handoff.download_started'
      operationId: string
      attemptKind: BrowserHandoffContext['attemptKind']
      attemptId: string
      packageDigestPresent: boolean
      packageDigest?: string
      retryableUntilPresent: boolean
      retryableUntilMilliseconds?: number
    }>
  | Readonly<{
      name: 'receive.handoff.not_started'
      operationId: string
      attemptKind: BrowserHandoffContext['attemptKind']
      attemptId: string
      externalAttemptReason: ExternalHandoffFailureReason
    }>

export type BrowserHandoffTraceListener = (event: BrowserHandoffTraceEvent) => void

export interface BrowserHandoffPublisher {
  handoff(request: BrowserHandoffRequest): DownloadStarted
}

export interface BrowserHandoffStarted {
  readonly result: DownloadStarted
  readonly urlLeaseStartedAt: number
  readonly urlLeaseEndsAt: number
}

export interface TimedBrowserHandoffPublisher extends BrowserHandoffPublisher {
  handoffWithLease(request: BrowserHandoffRequest): BrowserHandoffStarted
}

export interface ObjectUrlLease {
  cancel(): void
}

export interface BrowserHandoffAnchor {
  download: string
  href: string
  hidden: string | boolean
  click(): void
  remove(): void
}

export interface BrowserHandoffPorts {
  readonly createObjectUrl: (source: Blob) => string
  readonly revokeObjectUrl: (objectUrl: string) => void
  readonly createAnchor: () => BrowserHandoffAnchor
  readonly appendAnchor: (anchor: BrowserHandoffAnchor) => void
  readonly scheduleObjectUrlLease: (
    revoke: () => void,
    durationMilliseconds: number,
  ) => ObjectUrlLease
  readonly now?: () => number
  readonly trace?: BrowserHandoffTraceListener
}

export interface BrowserHandoffWindow {
  readonly URL: Pick<typeof URL, 'createObjectURL' | 'revokeObjectURL'>
  readonly document: Document
  readonly setTimeout: Window['setTimeout']
  readonly clearTimeout: Window['clearTimeout']
}

export interface PortableHandoffWindow extends BrowserHandoffWindow {
  readonly Blob: typeof Blob
  readonly WritableStream: typeof WritableStream
}

export interface PortableAssemblyPorts {
  readonly Blob: typeof Blob
  readonly WritableStream: typeof WritableStream
  readonly observeAssembly?: (snapshot: PortableAssemblySnapshot) => void
}

export interface PortableHandoffSession {
  readonly writable: WritableStream<Uint8Array<ArrayBuffer>>
  readonly result: Promise<DownloadStarted>
  readonly exactArtifactBytes: bigint
  readonly maximumArtifactBytes: bigint
}

export class BrowserHandoffNotStartedError extends Error {
  readonly externalAttemptReason: ExternalHandoffFailureReason = 'target-unavailable'

  constructor() {
    super('Browser handoff did not start')
    this.name = 'BrowserHandoffNotStartedError'
  }
}

export class PortableHandoffError extends Error {
  readonly restartReason: PortableRestartReason

  constructor(restartReason: PortableRestartReason) {
    super(restartReason === 'portable-aborted'
      ? 'Portable handoff was aborted before download started'
      : 'Portable artifact no longer matches its sealed preparation')
    this.name = 'PortableHandoffError'
    this.restartReason = restartReason
  }
}

export function browserSupportsObjectUrlHandoff(
  windowPort: Partial<BrowserHandoffWindow>,
): boolean {
  return typeof windowPort.URL?.createObjectURL === 'function' &&
    typeof windowPort.URL?.revokeObjectURL === 'function' &&
    typeof windowPort.document?.createElement === 'function' &&
    windowPort.document.documentElement !== null &&
    typeof windowPort.setTimeout === 'function' &&
    typeof windowPort.clearTimeout === 'function'
}

export function browserSupportsPortableHandoff(
  windowPort: Partial<PortableHandoffWindow>,
): boolean {
  return browserSupportsObjectUrlHandoff(windowPort) &&
    typeof windowPort.Blob === 'function' &&
    typeof windowPort.WritableStream === 'function'
}

export function createBrowserHandoffPublisher(
  ports: BrowserHandoffPorts,
): TimedBrowserHandoffPublisher {
  const handoffWithLease = (
    request: BrowserHandoffRequest,
  ): BrowserHandoffStarted => publishBrowserHandoff(ports, request)
  return Object.freeze({
    handoff(request: BrowserHandoffRequest): DownloadStarted {
      return handoffWithLease(request).result
    },
    handoffWithLease,
  })
}

export function createWindowBrowserHandoffPublisher(
  windowPort: BrowserHandoffWindow,
  traceListener?: BrowserHandoffTraceListener,
): TimedBrowserHandoffPublisher {
  if (!browserSupportsObjectUrlHandoff(windowPort)) {
    throw new DOMException('Browser handoff is unavailable', 'NotSupportedError')
  }
  return createBrowserHandoffPublisher({
    createObjectUrl: (source) => windowPort.URL.createObjectURL(source),
    revokeObjectUrl: (objectUrl) => windowPort.URL.revokeObjectURL(objectUrl),
    createAnchor: () => windowPort.document.createElement('a'),
    appendAnchor: (anchor) => windowPort.document.documentElement.append(anchor as unknown as Node),
    scheduleObjectUrlLease: (revoke, durationMilliseconds) => {
      const timeout = windowPort.setTimeout(revoke, durationMilliseconds)
      return Object.freeze({
        cancel: () => windowPort.clearTimeout(timeout),
      })
    },
    now: () => Date.now(),
    ...(traceListener === undefined ? {} : { trace: traceListener }),
  })
}

export async function openPortableHandoff(input: Readonly<{
  intent: ReceiveIntent
  admission: PortableArtifactAdmission
  attemptId: string
  publisher: BrowserHandoffPublisher
  assembly: PortableAssemblyPorts
}>): Promise<PortableHandoffSession> {
  const intent = await validateReceiveIntent(input.intent)
  if (intent.plan.kind !== 'portable-handoff') {
    throw new TypeError('Portable handoff requires an explicit portable materialization plan')
  }
  if (intent.artifact.kind === 'directory-tree') {
    throw new TypeError('Portable handoff cannot materialize a directory artifact')
  }
  const admission = assertIssuedPortableArtifactAdmission(input.admission, {
    receiveIntentDigest: intent.digest,
    artifactDigest: intent.artifact.digest,
    artifactKind: intent.artifact.kind,
  })

  const binding = intent.plan.portable
  if (binding.maximumArtifactBytes !== DEFAULT_PORTABLE_ARTIFACT_LIMIT ||
      binding.assemblyPartBytes !== DEFAULT_PORTABLE_ASSEMBLY_PART_BYTES ||
      binding.maximumParts !== DEFAULT_PORTABLE_MAXIMUM_PARTS ||
      binding.objectUrlLeaseMilliseconds !== BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS) {
    throw new TypeError('Portable handoff binding does not use the frozen bounded policy')
  }
  if (admission.exactArtifactBytes > binding.maximumArtifactBytes) {
    throw new DOMException(portableHandoffLimitMessage(), 'NotSupportedError')
  }
  if (typeof input.attemptId !== 'string' || input.attemptId.length === 0) {
    throw new TypeError('Portable handoff requires a runtime attempt identity')
  }
  if (typeof input.assembly.Blob !== 'function' ||
      typeof input.assembly.WritableStream !== 'function') {
    throw new DOMException('Portable artifact assembly is unavailable', 'NotSupportedError')
  }

  const suggestedName = intent.artifact.suggestedName
  return createPortableAssembly({
    exactArtifactBytes: admission.exactArtifactBytes,
    maximumArtifactBytes: binding.maximumArtifactBytes,
    partBytes: binding.assemblyPartBytes,
    maximumParts: binding.maximumParts,
    objectUrlLeaseMilliseconds: binding.objectUrlLeaseMilliseconds,
    operationId: intent.operationId,
    attemptId: input.attemptId,
    suggestedName,
    mediaType: intent.artifact.kind === 'zip-archive'
      ? 'application/zip'
      : 'application/octet-stream',
    publisher: input.publisher,
    assembly: input.assembly,
  })
}

export async function openPortableBrowserHandoff(input: Readonly<{
  intent: ReceiveIntent
  admission: PortableArtifactAdmission
  attemptId: string
  windowPort: PortableHandoffWindow
  trace?: BrowserHandoffTraceListener
}>): Promise<PortableHandoffSession> {
  if (!browserSupportsPortableHandoff(input.windowPort)) {
    throw new DOMException('Portable browser handoff is unavailable', 'NotSupportedError')
  }
  return openPortableHandoff({
    intent: input.intent,
    admission: input.admission,
    attemptId: input.attemptId,
    publisher: createWindowBrowserHandoffPublisher(input.windowPort, input.trace),
    assembly: {
      Blob: input.windowPort.Blob,
      WritableStream: input.windowPort.WritableStream,
    },
  })
}

export function portableHandoffLimitMessage(): string {
  return `Portable browser handoff is limited to ${PORTABLE_HANDOFF_MAXIMUM_BYTES / MEBIBYTE_BYTES} MiB`
}

function createPortableAssembly(input: Readonly<{
  exactArtifactBytes: bigint
  maximumArtifactBytes: bigint
  partBytes: bigint
  maximumParts: bigint
  objectUrlLeaseMilliseconds: bigint
  operationId: string
  attemptId: string
  suggestedName: string
  mediaType: string
  publisher: BrowserHandoffPublisher
  assembly: PortableAssemblyPorts
}>): PortableHandoffSession {
  const exactArtifactBytes = Number(input.exactArtifactBytes)
  const partBytes = Number(input.partBytes)
  const maximumParts = Number(input.maximumParts)
  if (!Number.isSafeInteger(exactArtifactBytes) ||
      !Number.isSafeInteger(partBytes) || partBytes <= 0 ||
      !Number.isSafeInteger(maximumParts) || maximumParts <= 0 ||
      input.partBytes * input.maximumParts !== input.maximumArtifactBytes) {
    throw new TypeError('Portable assembly policy is invalid')
  }

  let parts: Uint8Array<ArrayBuffer>[] = []
  let pending: Uint8Array<ArrayBuffer> | undefined
  let pendingBytes = 0
  let bufferedBytes = 0
  let settled = false
  let resolveResult!: (result: DownloadStarted) => void
  let rejectResult!: (reason: unknown) => void
  const result = new Promise<DownloadStarted>((resolve, reject) => {
    resolveResult = resolve
    rejectResult = reject
  })
  const release = (): void => {
    parts = []
    pending = undefined
    pendingBytes = 0
    bufferedBytes = 0
    observe(input.assembly, 0, 0, 0)
  }
  const fail = (error: unknown): void => {
    release()
    if (settled) return
    settled = true
    rejectResult(error)
  }

  const writable = new input.assembly.WritableStream<Uint8Array<ArrayBuffer>>({
    write(chunk) {
      try {
        if (!(chunk instanceof Uint8Array)) {
          throw new TypeError('Portable handoff accepts only byte chunks')
        }
        if (chunk.byteLength > exactArtifactBytes - bufferedBytes) {
          observe(input.assembly, bufferedBytes, retainedPartCount(parts, pending), chunk.byteLength)
          throw new PortableHandoffError('preparation-invalidated')
        }

        let consumed = 0
        while (consumed < chunk.byteLength) {
          pending ??= new Uint8Array(partBytes)
          const copied = Math.min(
            chunk.byteLength - consumed,
            partBytes - pendingBytes,
          )
          pending.set(chunk.subarray(consumed, consumed + copied), pendingBytes)
          consumed += copied
          pendingBytes += copied
          if (pendingBytes === partBytes) {
            parts.push(pending)
            pending = undefined
            pendingBytes = 0
          }
        }
        bufferedBytes += chunk.byteLength
        observe(input.assembly, bufferedBytes, retainedPartCount(parts, pending), 0)
      } catch (error) {
        fail(error)
        throw error
      }
    },
    close() {
      try {
        if (bufferedBytes !== exactArtifactBytes) {
          throw new PortableHandoffError('preparation-invalidated')
        }
        if (pending !== undefined && pendingBytes > 0) {
          parts.push(pending.slice(0, pendingBytes))
        }
        if (parts.length > maximumParts) {
          throw new PortableHandoffError('preparation-invalidated')
        }

        const source = new input.assembly.Blob([...parts], { type: input.mediaType })
        if (BigInt(source.size) !== input.exactArtifactBytes) {
          throw new PortableHandoffError('preparation-invalidated')
        }
        release()
        const handoff = input.publisher.handoff({
          context: {
            attemptKind: 'portable',
            operationId: input.operationId,
            attemptId: input.attemptId,
          },
          source,
          exactBytes: input.exactArtifactBytes,
          suggestedName: input.suggestedName,
          objectUrlLeaseMilliseconds: Number(input.objectUrlLeaseMilliseconds),
        })
        if (handoff.kind !== 'download-started' ||
            handoff.suggestedName !== input.suggestedName ||
            handoff.retryableUntil !== undefined) {
          throw new TypeError('Portable publisher returned an invalid terminal result')
        }
        settled = true
        resolveResult(handoff)
      } catch (error) {
        fail(error)
        throw error
      }
    },
    abort() {
      const error = new PortableHandoffError('portable-aborted')
      fail(error)
    },
  })

  return Object.freeze({
    writable,
    result,
    exactArtifactBytes: input.exactArtifactBytes,
    maximumArtifactBytes: input.maximumArtifactBytes,
  })
}

function publishBrowserHandoff(
  ports: BrowserHandoffPorts,
  request: BrowserHandoffRequest,
): BrowserHandoffStarted {
  validateHandoffRequest(request)
  trace(ports.trace, handoffStartedTrace(request))

  let anchor: BrowserHandoffAnchor | undefined
  let lease: ObjectUrlLease | undefined
  let objectUrl: string | undefined
  let urlLeaseStartedAt: number | undefined
  let urlLeaseEndsAt: number | undefined
  let revoked = false
  const revoke = (): void => {
    if (revoked || objectUrl === undefined) return
    revoked = true
    try {
      ports.revokeObjectUrl(objectUrl)
    } catch {
      // Revocation is best-effort after the finite lease. It cannot change an
      // already-started browser handoff into a claimed failure.
    }
  }

  try {
    objectUrl = ports.createObjectUrl(request.source)
    if (typeof objectUrl !== 'string' || objectUrl.length === 0) {
      throw new BrowserHandoffNotStartedError()
    }
    urlLeaseStartedAt = handoffClockNow(ports)
    urlLeaseEndsAt = checkedObjectUrlLeaseEnd(
      urlLeaseStartedAt,
      request.objectUrlLeaseMilliseconds,
    )
    lease = ports.scheduleObjectUrlLease(revoke, request.objectUrlLeaseMilliseconds)
    if (lease === null || typeof lease.cancel !== 'function') {
      throw new BrowserHandoffNotStartedError()
    }
    anchor = ports.createAnchor()
    anchor.download = request.suggestedName
    anchor.href = objectUrl
    anchor.hidden = true
    ports.appendAnchor(anchor)
  } catch {
    try {
      lease?.cancel()
    } catch {
      // A failed cancellation is followed by idempotent immediate revocation.
    }
    revoke()
    removeAnchor(anchor)
    trace(ports.trace, {
      name: 'receive.handoff.not_started',
      operationId: request.context.operationId,
      attemptKind: request.context.attemptKind,
      attemptId: request.context.attemptId,
      externalAttemptReason: 'target-unavailable',
    })
    throw new BrowserHandoffNotStartedError()
  }

  // Invocation is the synchronous no-return boundary. A thrown DOM adapter
  // error cannot prove that the browser did not start the handoff.
  try {
    anchor!.click()
  } catch {
    // The finite URL lease remains authoritative after the boundary.
  }
  removeAnchor(anchor)
  const result: DownloadStarted = request.context.attemptKind === 'workspace'
    ? Object.freeze({
        kind: 'download-started' as const,
        suggestedName: request.suggestedName,
        retryableUntil: request.context.retryableUntil,
      })
    : Object.freeze({
        kind: 'download-started' as const,
        suggestedName: request.suggestedName,
      })
  trace(ports.trace, handoffDownloadStartedTrace(request))
  return Object.freeze({
    result,
    urlLeaseStartedAt: urlLeaseStartedAt!,
    urlLeaseEndsAt: urlLeaseEndsAt!,
  })
}

function handoffClockNow(ports: BrowserHandoffPorts): number {
  const now = (ports.now ?? Date.now)()
  if (!Number.isSafeInteger(now) || now < 0) {
    throw new TypeError('Browser handoff clock is invalid')
  }
  return now
}

function checkedObjectUrlLeaseEnd(startedAt: number, durationMilliseconds: number): number {
  const endsAt = startedAt + durationMilliseconds
  if (!Number.isSafeInteger(endsAt) || endsAt < startedAt) {
    throw new RangeError('Browser handoff URL lease end overflows')
  }
  return endsAt
}

function validateHandoffRequest(request: BrowserHandoffRequest): void {
  if (request.source === null || typeof request.source !== 'object' ||
      !Number.isSafeInteger(request.source.size) ||
      typeof request.exactBytes !== 'bigint' ||
      request.exactBytes < 0n ||
      BigInt(request.source.size) !== request.exactBytes) {
    throw new TypeError('Browser handoff source length is invalid')
  }
  if (typeof request.suggestedName !== 'string' || request.suggestedName.length === 0) {
    throw new TypeError('Browser handoff requires a suggested name')
  }
  if (request.objectUrlLeaseMilliseconds !== BROWSER_HANDOFF_OBJECT_URL_LEASE_MS) {
    throw new TypeError('Browser handoff requires the frozen finite object URL lease')
  }
  if (request.context.operationId.length === 0 || request.context.attemptId.length === 0) {
    throw new TypeError('Browser handoff requires stable operation and attempt identities')
  }
  if (request.context.attemptKind === 'portable') return
  if (request.context.packageDigest.length === 0 ||
      !Number.isSafeInteger(request.context.retryableUntil) ||
      request.context.retryableUntil < 0) {
    throw new TypeError('Packaged artifact handoff context is invalid')
  }
}

function handoffStartedTrace(
  request: BrowserHandoffRequest,
): Extract<BrowserHandoffTraceEvent, { name: 'receive.handoff.started' }> {
  const packageFields = request.context.attemptKind === 'workspace'
    ? { packageDigestPresent: true as const, packageDigest: request.context.packageDigest }
    : { packageDigestPresent: false as const }
  return Object.freeze({
    name: 'receive.handoff.started' as const,
    operationId: request.context.operationId,
    attemptKind: request.context.attemptKind,
    attemptId: request.context.attemptId,
    ...packageFields,
    objectUrlLeaseMilliseconds: request.objectUrlLeaseMilliseconds,
  })
}

function handoffDownloadStartedTrace(
  request: BrowserHandoffRequest,
): Extract<BrowserHandoffTraceEvent, { name: 'receive.handoff.download_started' }> {
  if (request.context.attemptKind === 'workspace') {
    return Object.freeze({
      name: 'receive.handoff.download_started' as const,
      operationId: request.context.operationId,
      attemptKind: request.context.attemptKind,
      attemptId: request.context.attemptId,
      packageDigestPresent: true,
      packageDigest: request.context.packageDigest,
      retryableUntilPresent: true,
      retryableUntilMilliseconds: request.context.retryableUntil,
    })
  }
  return Object.freeze({
    name: 'receive.handoff.download_started' as const,
    operationId: request.context.operationId,
    attemptKind: request.context.attemptKind,
    attemptId: request.context.attemptId,
    packageDigestPresent: false,
    retryableUntilPresent: false,
  })
}

function observe(
  ports: PortableAssemblyPorts,
  bufferedBytes: number,
  retainedParts: number,
  rejectedWriteBytes: number,
): void {
  if (ports.observeAssembly === undefined) return
  try {
    ports.observeAssembly(Object.freeze({
      bufferedBytes,
      retainedParts,
      rejectedWriteBytes,
    }))
  } catch {
    // Resource telemetry is diagnostic and cannot own delivery or terminal state.
  }
}

function retainedPartCount(
  parts: readonly Uint8Array<ArrayBuffer>[],
  pending: Uint8Array<ArrayBuffer> | undefined,
): number {
  return parts.length + (pending === undefined ? 0 : 1)
}

function removeAnchor(anchor: BrowserHandoffAnchor | undefined): void {
  try {
    anchor?.remove()
  } catch {
    // Anchor removal is local DOM cleanup and cannot alter the handoff result.
  }
}

function trace(
  listener: BrowserHandoffTraceListener | undefined,
  event: BrowserHandoffTraceEvent,
): void {
  if (listener === undefined) return
  try {
    listener(event)
  } catch {
    // Observability is diagnostic; it never owns delivery or terminal state.
  }
}
