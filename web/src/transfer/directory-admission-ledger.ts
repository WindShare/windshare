import {
  DIRECTORY_ADMISSION_SECRET_BYTES,
  DirectoryAdmissionBindingError,
  createDirectoryAdmission,
  createDirectoryAdmissionSecret,
  directoryAdmissionRetainedMetadataBytes,
  finalizedDirectorySettlement,
  isImmediateChildPath,
  isolatedDirectorySettlement,
  sameDirectoryAdmission,
  sameMaterializationPath,
  snapshotDirectoryAdmission,
  snapshotDirectoryAdmissionScope,
  snapshotMaterializationDirectory,
  snapshotMaterializationPath,
  validateDirectoryAdmissionBinding,
  type DirectoryAdmission,
  type DirectoryAdmissionScope,
  type DirectorySettlement,
  type MaterializationDirectory,
} from './directory-admission'
import { FaultScope, OutputFaultCode, outputFault } from './fault'

export const MAX_DIRECTORY_ADMISSIONS = 1_000_000
export const MAX_DIRECTORY_ADMISSION_METADATA_BYTES = 67_108_864

export type DirectoryAdmissionLimit =
  | 'directory-admission-count'
  | 'directory-admission-metadata-bytes'

export class DirectoryAdmissionLimitError extends Error {
  readonly limitClass: DirectoryAdmissionLimit
  readonly maximum: bigint
  readonly observed: bigint

  constructor(limitClass: DirectoryAdmissionLimit, maximum: bigint, observed: bigint) {
    super('directory admission exceeds its ' + limitClass + ' limit')
    this.name = 'DirectoryAdmissionLimitError'
    this.limitClass = limitClass
    this.maximum = maximum
    this.observed = observed
  }
}

export interface DirectoryAdmissionLedgerOptions {
  /** Deterministic injection is reserved for canonical replay and focused tests. */
  readonly secret?: Uint8Array<ArrayBufferLike>
  readonly maximumAdmissions?: number
  readonly maximumMetadataBytes?: number
}

export type DirectoryAdmissionMaterializer = (
  directory: MaterializationDirectory,
  signal: AbortSignal,
) => Promise<void>

export type DirectoryFinalizationDecision = 'finalized' | 'isolated-metadata-failure'

export type DirectoryFinalizer = (
  directory: MaterializationDirectory,
  signal: AbortSignal,
) => Promise<DirectoryFinalizationDecision>

export interface DirectoryFileReference {
  readonly path: readonly string[]
  readonly parentAdmission: DirectoryAdmission
}

export interface DirectoryFileMutationLease {
  readonly file: DirectoryFileReference
  release(): void
}

interface DirectoryClaim {
  readonly key: string
  readonly directory: MaterializationDirectory
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

interface BindingReservation {
  readonly fresh: boolean
  readonly key: string
  readonly pathKey: string
  readonly metadataBytes: number
}

/** Owns bounded, session-local generation authority without becoming capacity admission. */
export class DirectoryAdmissionLedger {
  readonly #scope: DirectoryAdmissionScope
  readonly #secret: Uint8Array<ArrayBuffer>
  readonly #claimsByKey = new Map<string, DirectoryClaim>()
  readonly #claimsByToken = new Map<string, DirectoryClaim>()
  readonly #pendingAdmissions = new Map<string, Promise<DirectoryAdmission>>()
  readonly #pendingFinalizations = new Map<string, Promise<DirectorySettlement>>()
  readonly #bindingByDirectory = new Map<string, string>()
  readonly #bindingByPath = new Map<string, string>()
  readonly #maximumAdmissions: number
  readonly #maximumMetadataBytes: number
  #reservedMetadataBytes = 0

  constructor(
    scope: DirectoryAdmissionScope,
    options: DirectoryAdmissionLedgerOptions = {},
  ) {
    this.#scope = snapshotDirectoryAdmissionScope(scope)
    this.#secret = options.secret === undefined
      ? createDirectoryAdmissionSecret()
      : snapshotAdmissionSecret(options.secret)
    this.#maximumAdmissions = boundedLimit(
      options.maximumAdmissions,
      MAX_DIRECTORY_ADMISSIONS,
      'directory admission count',
    )
    this.#maximumMetadataBytes = boundedLimit(
      options.maximumMetadataBytes,
      MAX_DIRECTORY_ADMISSION_METADATA_BYTES,
      'directory admission metadata',
    )
  }

  async admitDirectory(
    input: MaterializationDirectory,
    signal: AbortSignal,
    materialize?: DirectoryAdmissionMaterializer,
  ): Promise<DirectoryAdmission> {
    signal.throwIfAborted()
    const directory = snapshotMaterializationDirectory(input)
    const parent = this.#validateParent(directory)
    const key = admissionKey(directory)
    const existing = this.#claimsByKey.get(key)
    if (existing !== undefined) return existing.admission
    const pending = this.#pendingAdmissions.get(key)
    if (pending !== undefined) return pending

    const reservation = this.#reserveBinding(directory, key)
    let mutation: { release(): void }
    try {
      mutation = this.#beginDescendantMutation(parent, false)
    } catch (error) {
      this.#releaseBindingReservation(reservation)
      throw error
    }

    const operation = this.#admit(directory, key, parent, signal, materialize)
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
    directory: MaterializationDirectory,
    key: string,
    parent: DirectoryClaim | undefined,
    signal: AbortSignal,
    materialize: DirectoryAdmissionMaterializer | undefined,
  ): Promise<DirectoryAdmission> {
    signal.throwIfAborted()
    const admission = validateDirectoryAdmissionBinding(
      this.#scope,
      directory,
      await createDirectoryAdmission(this.#secret, this.#scope, directory),
    )
    const tokenOwner = this.#claimsByToken.get(admission.token)
    if (tokenOwner !== undefined && tokenOwner.key !== key) {
      throw new DirectoryAdmissionBindingError(
        'directory admission token is already bound to another claim',
      )
    }
    // The receipt is complete before mutation, but becomes visible only after
    // the materializer accepts the exact generation.
    await materialize?.(directory, signal)
    const claim: DirectoryClaim = {
      key,
      directory,
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

  #validateParent(directory: MaterializationDirectory): DirectoryClaim | undefined {
    const receipt = directory.parentAdmission
    if (receipt === undefined) return undefined
    const parent = this.#claimsByToken.get(receipt.token)
    if (parent === undefined || !sameDirectoryAdmission(parent.admission, receipt) ||
        !isImmediateChildPath(parent.admission.path, directory.path)) {
      throw new DirectoryAdmissionBindingError(
        'directory parent was not admitted by this ledger',
      )
    }
    return parent
  }

  #reserveBinding(directory: MaterializationDirectory, key: string): BindingReservation {
    const pathKey = JSON.stringify(directory.path)
    const previousDirectory = this.#bindingByDirectory.get(directory.directoryId)
    const previousPath = this.#bindingByPath.get(pathKey)
    if ((previousDirectory !== undefined && previousDirectory !== key) ||
        (previousPath !== undefined && previousPath !== key)) {
      throw new DirectoryAdmissionBindingError(
        'directory identity or canonical path is already bound to another generation',
      )
    }
    const fresh = previousDirectory === undefined && previousPath === undefined
    const metadataBytes = fresh
      ? directoryAdmissionRetainedMetadataBytes(this.#scope, directory)
      : 0
    if (fresh) this.#reserveMetadata(metadataBytes)
    this.#bindingByDirectory.set(directory.directoryId, key)
    this.#bindingByPath.set(pathKey, key)
    return { fresh, key, pathKey, metadataBytes }
  }

  #releaseBindingReservation(reservation: BindingReservation): void {
    if (!reservation.fresh || this.#claimsByKey.has(reservation.key)) return
    for (const [directoryId, key] of this.#bindingByDirectory) {
      if (key === reservation.key) this.#bindingByDirectory.delete(directoryId)
    }
    if (this.#bindingByPath.get(reservation.pathKey) === reservation.key) {
      this.#bindingByPath.delete(reservation.pathKey)
    }
    this.#reservedMetadataBytes -= reservation.metadataBytes
  }

  #reserveMetadata(metadataBytes: number): void {
    if (this.#bindingByDirectory.size >= this.#maximumAdmissions) {
      throw new DirectoryAdmissionLimitError(
        'directory-admission-count',
        BigInt(this.#maximumAdmissions),
        BigInt(this.#bindingByDirectory.size + 1),
      )
    }
    if (metadataBytes > this.#maximumMetadataBytes - this.#reservedMetadataBytes) {
      throw new DirectoryAdmissionLimitError(
        'directory-admission-metadata-bytes',
        BigInt(this.#maximumMetadataBytes),
        BigInt(this.#reservedMetadataBytes + metadataBytes),
      )
    }
    this.#reservedMetadataBytes += metadataBytes
  }

  validateFileParent(input: DirectoryFileReference): DirectoryFileReference {
    const file = snapshotDirectoryFileReference(input)
    this.#fileParent(file, true)
    return file
  }

  acquireFileMutation(input: DirectoryFileReference): DirectoryFileMutationLease {
    const file = snapshotDirectoryFileReference(input)
    const mutation = this.#beginDescendantMutation(this.#fileParent(file, true), false)
    let released = false
    return Object.freeze({
      file,
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
    finalize?: DirectoryFinalizer,
    closing?: AbortSignal,
  ): Promise<DirectorySettlement> {
    const claim = this.#claimForAdmission(input)
    if (claim.settlement !== undefined) return Promise.resolve(claim.settlement)
    const pending = this.#pendingFinalizations.get(claim.key)
    if (pending !== undefined) return pending
    signal.throwIfAborted()
    if (claim.state !== 'admitted') {
      throw new DirectoryAdmissionBindingError('directory claim is already sealing')
    }

    // State changes before the first await so no descendant gains authority
    // behind a concurrent finalization.
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
    finalize: DirectoryFinalizer | undefined,
    closing: AbortSignal | undefined,
  ): Promise<DirectorySettlement> {
    await this.#waitForDescendantDrain(claim, signal, closing)
    if (claim.directUnsettledChildren !== 0) {
      throw new DirectoryAdmissionBindingError(
        'directory finalization requires every direct child to settle first',
      )
    }
    const decision = await finalize?.(claim.directory, signal) ?? 'finalized'
    let settlement: DirectorySettlement
    switch (decision) {
      case 'finalized':
        settlement = finalizedDirectorySettlement(claim.admission)
        break
      case 'isolated-metadata-failure':
        settlement = isolatedDirectorySettlement(
          claim.admission,
          outputFault(FaultScope.DirectoryLocal, OutputFaultCode.DirectoryMetadata),
        )
        break
      default:
        throw new DirectoryAdmissionBindingError('directory finalizer returned an invalid decision')
    }
    if (claim.parent !== undefined) {
      if (claim.parent.directUnsettledChildren <= 0) {
        throw new DirectoryAdmissionBindingError(
          'directory settlement violated its parent-child invariant',
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

  #fileParent(file: DirectoryFileReference, requireMutable: boolean): DirectoryClaim {
    const parent = this.#claimsByToken.get(file.parentAdmission.token)
    if (parent === undefined ||
        !sameDirectoryAdmission(parent.admission, file.parentAdmission) ||
        !sameMaterializationPath(parent.admission.path, file.path.slice(0, -1))) {
      throw new DirectoryAdmissionBindingError(
        'file references a missing or mismatched directory receipt',
      )
    }
    if (requireMutable) {
      for (let ancestor: DirectoryClaim | undefined = parent;
        ancestor !== undefined;
        ancestor = ancestor.parent) {
        if (ancestor.state !== 'admitted') {
          throw new DirectoryAdmissionBindingError(
            'file references a sealing or settled directory',
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
          'descendant mutation references a sealed directory',
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

  #claimForAdmission(input: DirectoryAdmission): DirectoryClaim {
    const admission = snapshotDirectoryAdmission(input)
    const claim = this.#claimsByToken.get(admission.token)
    if (claim === undefined || !sameDirectoryAdmission(claim.admission, admission)) {
      throw new DirectoryAdmissionBindingError(
        'directory finalization references a forged or foreign receipt',
      )
    }
    validateDirectoryAdmissionBinding(this.#scope, claim.directory, admission)
    return claim
  }

  #notify(claim: DirectoryClaim): void {
    const previous = claim.change
    claim.change = claimChange()
    previous.resolve()
  }
}

function snapshotDirectoryFileReference(
  input: DirectoryFileReference,
): DirectoryFileReference {
  const path = snapshotMaterializationPath(input.path)
  if (path.length === 0) throw new DirectoryAdmissionBindingError('file path cannot be empty')
  return Object.freeze({
    path,
    parentAdmission: snapshotDirectoryAdmission(input.parentAdmission),
  })
}

function claimChange(): ClaimChange {
  let resolve = (): void => undefined
  const promise = new Promise<void>((complete) => {
    resolve = complete
  })
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
    await Promise.race(close === undefined
      ? [changed, abort.promise]
      : [changed, abort.promise, close.promise])
  } finally {
    abort.detach()
    close?.detach()
  }
}

function abortPromise(
  signal: AbortSignal,
): { readonly promise: Promise<never>; readonly detach: () => void } {
  let rejectPromise: ((reason: unknown) => void) | undefined
  const onAbort = (): void => {
    rejectPromise?.(signal.reason ?? new DOMException('Operation aborted', 'AbortError'))
  }
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

function snapshotAdmissionSecret(
  input: Uint8Array<ArrayBufferLike>,
): Uint8Array<ArrayBuffer> {
  if (!(input instanceof Uint8Array) ||
      input.byteLength !== DIRECTORY_ADMISSION_SECRET_BYTES ||
      input.every((byte) => byte === 0)) {
    throw new DirectoryAdmissionBindingError(
      'directory admission secret must be a non-zero 32-byte value',
    )
  }
  return Uint8Array.from(input)
}

function boundedLimit(value: number | undefined, maximum: number, label: string): number {
  const limit = value ?? maximum
  if (!Number.isSafeInteger(limit) || limit <= 0 || limit > maximum) {
    throw new RangeError(label + ' must be between 1 and ' + maximum)
  }
  return limit
}

function admissionKey(input: MaterializationDirectory): string {
  return JSON.stringify([
    input.directoryId,
    input.generation,
    input.path,
    input.parentAdmission?.token ?? null,
    modifiedTimeKey(input),
  ])
}

function modifiedTimeKey(input: MaterializationDirectory): readonly unknown[] | null {
  if (input.modifiedTime === undefined) return null
  return [
    input.modifiedTime.seconds.toString(),
    input.modifiedTime.nanoseconds,
    input.modifiedTime.precision,
  ]
}
