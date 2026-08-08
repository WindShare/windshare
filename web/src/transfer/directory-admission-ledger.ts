import {
  DIRECTORY_ADMISSION_SECRET_BYTES,
  DirectoryAdmissionBindingError,
  createDirectoryAdmission,
  createDirectoryAdmissionSecret,
  finalizedDirectorySettlement,
  isImmediateChildPath,
  isolatedDirectorySettlement,
  sameDirectoryAdmission,
  sameOutputPath,
  snapshotDirectoryAdmission,
  snapshotDirectoryAdmissionScope,
  snapshotOutputDirectoryAdmission,
  validateDirectoryAdmissionBinding,
  type DirectoryAdmission,
  type DirectoryAdmissionScope,
  type DirectorySettlement,
  type OutputDirectoryAdmission,
} from './directory-admission'
import {
  OutputBudgetExceededError,
  OutputDirectoryMutationError,
  type OutputFile,
  snapshotOutputFile,
} from './output-session'
import { FaultScope, OutputFaultCode, outputFault } from './fault'

export const MAXIMUM_OUTPUT_DIRECTORY_ADMISSIONS = 1_000_000
export const MAXIMUM_OUTPUT_DIRECTORY_ADMISSION_BYTES = 64 * 1024 * 1024
const TEXT_ENCODER = new TextEncoder()

export interface DirectoryAdmissionLedgerOptions {
  /** Deterministic injection is intended for protocol vectors and unit tests. */
  readonly secret?: Uint8Array<ArrayBufferLike>
  readonly maximumAdmissions?: number
  readonly maximumMetadataBytes?: number
}

export type DirectoryAdmissionMaterializer = (
  directory: OutputDirectoryAdmission,
  signal: AbortSignal,
) => Promise<void>

interface DirectoryClaim {
  readonly key: string
  readonly request: OutputDirectoryAdmission
  readonly admission: DirectoryAdmission
  readonly parent: DirectoryClaim | undefined
  state: 'admitted' | 'settling' | 'settled'
  activeDescendants: number
  directUnsettledChildren: number
  change: ClaimChange
  settlement?: DirectorySettlement
}

interface ClaimChange {
  readonly promise: Promise<void>
  readonly resolve: () => void
}

export interface DirectoryFileMutationLease {
  readonly file: OutputFile
  release(): void
}

/** Owns bounded, session-local catalog-generation authority for every output backend. */
export class DirectoryAdmissionLedger {
  readonly #scope: DirectoryAdmissionScope
  readonly #admissionSecret: Uint8Array<ArrayBuffer>
  readonly #claimsByKey = new Map<string, DirectoryClaim>()
  readonly #claimsByToken = new Map<string, DirectoryClaim>()
  readonly #pendingAdmissions = new Map<string, Promise<DirectoryAdmission>>()
  readonly #pendingFinalizations = new Map<string, Promise<DirectorySettlement>>()
  readonly #bindingByDirectory = new Map<string, string>()
  readonly #bindingByPath = new Map<string, string>()
  readonly #maximumAdmissions: number
  readonly #maximumMetadataBytes: number
  #reservedMetadataBytes = 0

  constructor(scope: DirectoryAdmissionScope, options: DirectoryAdmissionLedgerOptions = {}) {
    this.#scope = snapshotDirectoryAdmissionScope(scope)
    this.#admissionSecret = options.secret === undefined
      ? createDirectoryAdmissionSecret()
      : admissionSecret(options.secret)
    this.#maximumAdmissions = boundedAdmissionLimit(
      options.maximumAdmissions,
      MAXIMUM_OUTPUT_DIRECTORY_ADMISSIONS,
      'directory admissions',
    )
    this.#maximumMetadataBytes = boundedAdmissionLimit(
      options.maximumMetadataBytes,
      MAXIMUM_OUTPUT_DIRECTORY_ADMISSION_BYTES,
      'directory admission metadata bytes',
    )
  }

  async admitDirectory(
    input: OutputDirectoryAdmission,
    signal: AbortSignal,
    materialize?: DirectoryAdmissionMaterializer,
  ): Promise<DirectoryAdmission> {
    signal.throwIfAborted()
    const request = snapshotOutputDirectoryAdmission(input)
    const parent = this.#validateParent(request)
    const key = admissionKey(request)
    const existing = this.#claimsByKey.get(key)
    if (existing !== undefined) return existing.admission
    const pending = this.#pendingAdmissions.get(key)
    if (pending !== undefined) return pending

    const reservation = this.#reserveBinding(request, key)
    let mutation: { release(): void }
    try {
      mutation = this.#beginDescendantMutation(parent, false)
    } catch (error) {
      this.#releaseBindingReservation(reservation)
      throw error
    }

    const operation = this.#admit(request, key, parent, signal, materialize)
    this.#pendingAdmissions.set(key, operation)
    try {
      return await operation
    } catch (error) {
      this.#releaseBindingReservation(reservation)
      throw error
    } finally {
      mutation.release()
      if (this.#pendingAdmissions.get(key) === operation) this.#pendingAdmissions.delete(key)
    }
  }

  async #admit(
    request: OutputDirectoryAdmission,
    key: string,
    parent: DirectoryClaim | undefined,
    signal: AbortSignal,
    materialize?: DirectoryAdmissionMaterializer,
  ): Promise<DirectoryAdmission> {
    signal.throwIfAborted()
    const admission = validateDirectoryAdmissionBinding(
      this.#scope,
      request,
      await createDirectoryAdmission(this.#admissionSecret, this.#scope, request),
    )
    const tokenOwner = this.#claimsByToken.get(admission.token)
    if (tokenOwner !== undefined && tokenOwner.key !== key) {
      throw new DirectoryAdmissionBindingError(
        'output session reused a directory admission token for a different binding',
      )
    }
    // Proof validation precedes irreversible materialization, but callers see
    // the proof only after the backend has accepted the directory.
    await materialize?.(request, signal)
    // Materialization is the commit point. A late cancellation stops future
    // orchestration but cannot erase the proof for state the backend accepted.
    const claim: DirectoryClaim = {
      key,
      request,
      admission,
      parent,
      state: 'admitted',
      activeDescendants: 0,
      directUnsettledChildren: 0,
      change: claimChange(),
    }
    this.#claimsByKey.set(key, claim)
    this.#claimsByToken.set(admission.token, claim)
    if (parent !== undefined) {
      parent.directUnsettledChildren += 1
      this.#notify(parent)
    }
    return admission
  }

  #validateParent(request: OutputDirectoryAdmission): DirectoryClaim | undefined {
    if (request.parentAdmission === undefined) return undefined
    const parent = this.#claimsByToken.get(request.parentAdmission.token)
    if (parent === undefined || !sameDirectoryAdmission(parent.admission, request.parentAdmission)) {
      throw new DirectoryAdmissionBindingError(
        'directory admission parent was not admitted by this output session',
      )
    }
    if (!isImmediateChildPath(parent.admission.path, request.path)) {
      throw new DirectoryAdmissionBindingError(
        'directory admission path is not an immediate child of its admitted parent',
      )
    }
    return parent
  }

  #reserveBinding(request: OutputDirectoryAdmission, key: string): {
    readonly fresh: boolean
    readonly key: string
    readonly pathKey: string
    readonly metadataBytes: number
  } {
    const pathKey = JSON.stringify(request.path)
    const previousDirectoryBinding = this.#bindingByDirectory.get(request.directoryId)
    const previousPathBinding = this.#bindingByPath.get(pathKey)
    if ((previousDirectoryBinding !== undefined && previousDirectoryBinding !== key) ||
        (previousPathBinding !== undefined && previousPathBinding !== key)) {
      throw new DirectoryAdmissionBindingError(
        'directory admission conflicts with an existing generation or canonical path binding',
      )
    }
    const fresh = previousDirectoryBinding === undefined && previousPathBinding === undefined
    const metadataBytes = fresh ? directoryAdmissionMetadataBytes(request, key, pathKey) : 0
    if (fresh) this.#reserveMetadata(metadataBytes)
    this.#bindingByDirectory.set(request.directoryId, key)
    this.#bindingByPath.set(pathKey, key)
    return { fresh, key, pathKey, metadataBytes }
  }

  #releaseBindingReservation(reservation: {
    readonly fresh: boolean
    readonly key: string
    readonly pathKey: string
    readonly metadataBytes: number
  }): void {
    if (!reservation.fresh || this.#claimsByKey.has(reservation.key)) return
    for (const [directoryId, key] of this.#bindingByDirectory) {
      if (key === reservation.key) this.#bindingByDirectory.delete(directoryId)
    }
    if (this.#bindingByPath.get(reservation.pathKey) === reservation.key) {
      this.#bindingByPath.delete(reservation.pathKey)
    }
    this.#reservedMetadataBytes -= reservation.metadataBytes
  }

  #reserveMetadata(reservationBytes: number): void {
    if (this.#bindingByDirectory.size >= this.#maximumAdmissions) {
      throw new OutputBudgetExceededError(
        'directory-admission-count',
        BigInt(this.#maximumAdmissions),
        BigInt(this.#bindingByDirectory.size + 1),
      )
    }
    if (reservationBytes > this.#maximumMetadataBytes - this.#reservedMetadataBytes) {
      throw new OutputBudgetExceededError(
        'directory-admission-metadata-bytes',
        BigInt(this.#maximumMetadataBytes),
        BigInt(this.#reservedMetadataBytes + reservationBytes),
      )
    }
    this.#reservedMetadataBytes += reservationBytes
  }

  validateFileParent(file: OutputFile): OutputFile {
    const snapshot = snapshotOutputFile(file)
    this.#fileParent(snapshot, true)
    return snapshot
  }

  acquireFileMutation(file: OutputFile): DirectoryFileMutationLease {
    const snapshot = snapshotOutputFile(file)
    const parent = this.#fileParent(snapshot, true)
    const mutation = this.#beginDescendantMutation(parent, false)
    let released = false
    return Object.freeze({
      file: snapshot,
      release: () => {
        if (released) return
        released = true
        mutation.release()
      },
    })
  }

  finalizeDirectory(
    input: DirectoryAdmission,
    signal: AbortSignal,
    finalize?: DirectoryAdmissionMaterializer,
    closing?: AbortSignal,
  ): Promise<DirectorySettlement> {
    const claim = this.#claimForAdmission(input)
    if (claim.settlement !== undefined) return Promise.resolve(claim.settlement)
    const pending = this.#pendingFinalizations.get(claim.key)
    if (pending !== undefined) return pending
    signal.throwIfAborted()
    if (claim.state === 'settled') {
      throw new DirectoryAdmissionBindingError(
        'settled directory claim is missing its terminal settlement',
      )
    }

    // Sealing is synchronous with receipt validation. No descendant call can
    // slip through the first await and gain authority behind finalization.
    claim.state = 'settling'
    this.#notify(claim)
    const mutation = this.#beginDescendantMutation(claim.parent, true)
    const operation = this.#finalize(claim, signal, finalize, closing)
    this.#pendingFinalizations.set(claim.key, operation)
    operation.finally(() => {
      mutation.release()
      if (this.#pendingFinalizations.get(claim.key) === operation) {
        this.#pendingFinalizations.delete(claim.key)
      }
    }).catch(() => undefined)
    return operation
  }

  async #finalize(
    claim: DirectoryClaim,
    signal: AbortSignal,
    finalize?: DirectoryAdmissionMaterializer,
    closing?: AbortSignal,
  ): Promise<DirectorySettlement> {
    await this.#waitForDescendantDrain(claim, signal, closing)
    if (claim.directUnsettledChildren !== 0) {
      throw new DirectoryAdmissionBindingError(
        'directory finalization requires every direct child directory to be settled',
      )
    }
    let settlement: DirectorySettlement
    try {
      await finalize?.(claim.request, signal)
      settlement = finalizedDirectorySettlement(claim.admission)
    } catch (error) {
      if (!(error instanceof OutputDirectoryMutationError) || error.sessionCompromised) throw error
      settlement = isolatedDirectorySettlement(
        claim.admission,
        outputFault(FaultScope.DirectoryLocal, OutputFaultCode.DirectoryMetadata),
      )
    }
    if (claim.parent !== undefined) {
      if (claim.parent.directUnsettledChildren <= 0) {
        throw new DirectoryAdmissionBindingError(
          'directory settlement violated its parent child-settlement invariant',
        )
      }
      claim.parent.directUnsettledChildren -= 1
      this.#notify(claim.parent)
    }
    claim.state = 'settled'
    claim.settlement = settlement
    this.#notify(claim)
    return settlement
  }

  async #waitForDescendantDrain(
    claim: DirectoryClaim,
    signal: AbortSignal,
    closing: AbortSignal | undefined,
  ): Promise<void> {
    while (claim.activeDescendants !== 0) {
      await waitForClaimChange(claim.change.promise, signal, closing)
    }
  }

  #fileParent(file: OutputFile, requireMutable: boolean): DirectoryClaim {
    const parentAdmission = file.parentAdmission
    const parent = parentAdmission === undefined
      ? undefined
      : this.#claimsByToken.get(parentAdmission.token)
    if (parent === undefined || parentAdmission === undefined ||
        !sameDirectoryAdmission(parent.admission, parentAdmission) ||
        !sameOutputPath(parent.admission.path, file.path.slice(0, -1))) {
      throw new DirectoryAdmissionBindingError(
        'output file references a missing, forged, or mismatched parent admission, or its parent is sealed',
      )
    }
    if (requireMutable) {
      for (let ancestor: DirectoryClaim | undefined = parent;
        ancestor !== undefined;
        ancestor = ancestor.parent) {
        if (ancestor.state !== 'admitted') {
          throw new DirectoryAdmissionBindingError(
            'output file references a missing, forged, or mismatched parent admission, or its parent is sealed',
          )
        }
      }
    }
    return parent
  }

  #beginDescendantMutation(
    parent: DirectoryClaim | undefined,
    allowSettling: boolean,
  ): { release(): void } {
    const ancestors: DirectoryClaim[] = []
    for (let ancestor = parent; ancestor !== undefined; ancestor = ancestor.parent) {
      if (ancestor.state !== 'admitted' && !(allowSettling && ancestor.state === 'settling')) {
        throw new DirectoryAdmissionBindingError(
          'directory descendant mutation references a sealed or settled admission',
        )
      }
      ancestors.push(ancestor)
    }
    for (const ancestor of ancestors) {
      ancestor.activeDescendants += 1
      this.#notify(ancestor)
    }
    let released = false
    return {
      release: () => {
        if (released) return
        released = true
        for (const ancestor of ancestors) {
          ancestor.activeDescendants -= 1
          this.#notify(ancestor)
        }
      },
    }
  }

  #notify(claim: DirectoryClaim): void {
    const previous = claim.change
    claim.change = claimChange()
    previous.resolve()
  }

  #claimForAdmission(input: DirectoryAdmission): DirectoryClaim {
    const admission = snapshotDirectoryAdmission(input)
    const claim = this.#claimsByToken.get(admission.token)
    if (claim === undefined || !sameDirectoryAdmission(claim.admission, admission)) {
      throw new DirectoryAdmissionBindingError(
        'directory finalization references a forged or foreign admission',
      )
    }
    validateDirectoryAdmissionBinding(this.#scope, claim.request, admission)
    return claim
  }
}

function claimChange(): ClaimChange {
  let resolve = (): void => undefined
  const promise = new Promise<void>((complete) => { resolve = complete })
  return { promise, resolve }
}

async function waitForClaimChange(
  changed: Promise<void>,
  signal: AbortSignal,
  closing: AbortSignal | undefined,
): Promise<void> {
  signal.throwIfAborted()
  closing?.throwIfAborted()
  const abort = abortPromise(signal)
  const close = closing === undefined ? undefined : abortPromise(closing)
  try {
    await Promise.race(close === undefined ? [changed, abort.promise] : [changed, abort.promise, close.promise])
  } finally {
    abort.detach()
    close?.detach()
  }
}

function abortPromise(signal: AbortSignal): { readonly promise: Promise<never>; readonly detach: () => void } {
  let rejectPromise: ((reason: unknown) => void) | undefined
  const onAbort = (): void => rejectPromise?.(signal.reason ?? new DOMException('Operation aborted', 'AbortError'))
  const promise = new Promise<never>((_resolve, reject) => {
    rejectPromise = reject
    signal.addEventListener('abort', onAbort, { once: true })
  })
  if (signal.aborted) onAbort()
  return {
    promise,
    detach: () => signal.removeEventListener('abort', onAbort),
  }
}

function admissionSecret(value: Uint8Array<ArrayBufferLike>): Uint8Array<ArrayBuffer> {
  if (!(value instanceof Uint8Array) || value.byteLength !== DIRECTORY_ADMISSION_SECRET_BYTES ||
      value.every((byte) => byte === 0)) {
    throw new DirectoryAdmissionBindingError(
      `directory admission secret must be a non-zero ${DIRECTORY_ADMISSION_SECRET_BYTES}-byte value`,
    )
  }
  return Uint8Array.from(value)
}

function directoryAdmissionMetadataBytes(
  request: OutputDirectoryAdmission,
  key: string,
  pathKey: string,
): number {
  // Caller-controlled retained strings are charged exactly; engine overhead is
  // independently bounded by the admission-count limit.
  return TEXT_ENCODER.encode(
    `${key}\0${pathKey}\0${request.directoryId}\0${request.generation}\0${request.parentAdmission?.token ?? ''}`,
  ).byteLength
}

function boundedAdmissionLimit(value: number | undefined, maximum: number, label: string): number {
  const limit = value ?? maximum
  if (!Number.isSafeInteger(limit) || limit <= 0 || limit > maximum) {
    throw new RangeError(`${label} must be between 1 and ${maximum}`)
  }
  return limit
}

function admissionKey(input: OutputDirectoryAdmission): string {
  return JSON.stringify([
    input.directoryId,
    input.generation,
    input.path,
    input.parentAdmission?.token ?? null,
    modifiedTimeKey(input),
  ])
}

function modifiedTimeKey(input: OutputDirectoryAdmission): readonly unknown[] | null {
  if (input.modifiedTime === undefined) return null
  return [
    input.modifiedTime.seconds.toString(),
    input.modifiedTime.nanoseconds,
    input.modifiedTime.precision,
  ]
}
