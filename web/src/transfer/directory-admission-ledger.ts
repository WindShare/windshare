import {
  DIRECTORY_ADMISSION_SECRET_BYTES,
  DirectoryAdmissionBindingError,
  OutputBudgetExceededError,
  createDirectoryAdmission,
  createDirectoryAdmissionSecret,
  isImmediateChildPath,
  sameModifiedTime,
  sameOutputPath,
  snapshotOutputDirectoryAdmission,
  snapshotOutputDirectory,
  snapshotOutputFile,
  validateDirectoryAdmissionBinding,
  type DirectoryAdmission,
  type OutputDirectoryAdmission,
  type OutputDirectory,
  type OutputFile,
} from './output-session'

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

/** Owns bounded, session-local catalog-generation authority for every output backend. */
export class DirectoryAdmissionLedger {
  readonly #admissionSecret: Uint8Array<ArrayBuffer>
  readonly #admissions = new Map<string, DirectoryAdmission>()
  readonly #admissionsByToken = new Map<
    string,
    { readonly key: string; readonly admission: DirectoryAdmission }
  >()
  readonly #pendingAdmissions = new Map<string, Promise<DirectoryAdmission>>()
  readonly #bindingByDirectory = new Map<string, string>()
  readonly #bindingByPath = new Map<string, string>()
  readonly #maximumAdmissions: number
  readonly #maximumMetadataBytes: number
  #reservedMetadataBytes = 0

  constructor(options: DirectoryAdmissionLedgerOptions = {}) {
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
    this.#validateParent(request)
    const key = admissionKey(request)
    const reservation = this.#reserveBinding(request, key)
    const existing = this.#admissions.get(key)
    if (existing !== undefined) return existing
    const pending = this.#pendingAdmissions.get(key)
    if (pending !== undefined) return pending

    const operation = this.#admit(request, key, signal, materialize)
    this.#pendingAdmissions.set(key, operation)
    try {
      return await operation
    } catch (error) {
      this.#releaseBindingReservation(reservation)
      throw error
    } finally {
      if (this.#pendingAdmissions.get(key) === operation) this.#pendingAdmissions.delete(key)
    }
  }

  async #admit(
    request: OutputDirectoryAdmission,
    key: string,
    signal: AbortSignal,
    materialize?: DirectoryAdmissionMaterializer,
  ): Promise<DirectoryAdmission> {
    signal.throwIfAborted()
    const admission = validateDirectoryAdmissionBinding(
      request,
      await createDirectoryAdmission(request, this.#admissionSecret),
    )
    const tokenOwner = this.#admissionsByToken.get(admission.token)
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
    this.#admissions.set(key, admission)
    this.#admissionsByToken.set(admission.token, { key, admission })
    return admission
  }

  #validateParent(request: OutputDirectoryAdmission): void {
    if (request.parentAdmission === undefined) return
    const parent = this.#admissionsByToken.get(request.parentAdmission.token)?.admission
    if (parent === undefined || !sameDirectoryAdmission(parent, request.parentAdmission)) {
      throw new DirectoryAdmissionBindingError(
        'directory admission parent was not admitted by this output session',
      )
    }
    if (!isImmediateChildPath(parent.path, request.path)) {
      throw new DirectoryAdmissionBindingError(
        'directory admission path is not an immediate child of its admitted parent',
      )
    }
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
    if (!reservation.fresh || this.#admissions.has(reservation.key)) return
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
    const parentAdmission = snapshot.parentAdmission
    const parent = parentAdmission === undefined
      ? undefined
      : this.#admissionsByToken.get(parentAdmission.token)?.admission
    if (parent === undefined || parentAdmission === undefined ||
        !sameDirectoryAdmission(parent, parentAdmission) ||
        !sameOutputPath(parent.path, snapshot.path.slice(0, -1))) {
      throw new DirectoryAdmissionBindingError(
        'output file references a missing, forged, or mismatched parent admission',
      )
    }
    return snapshot
  }

  validateDirectoryFinalization(input: OutputDirectory): OutputDirectory {
    const directory = snapshotOutputDirectory(input)
    this.#validateParent(directory)
    const admitted = this.#admissions.get(admissionKey(directory))
    if (admitted === undefined) {
      throw new DirectoryAdmissionBindingError(
        'output directory finalization references an unadmitted or rebound generation',
      )
    }
    validateDirectoryAdmissionBinding(directory, admitted)
    return directory
  }
}

function admissionSecret(value: Uint8Array<ArrayBufferLike>): Uint8Array<ArrayBuffer> {
  if (!(value instanceof Uint8Array) || value.byteLength !== DIRECTORY_ADMISSION_SECRET_BYTES) {
    throw new DirectoryAdmissionBindingError(
      `directory admission secret must be exactly ${DIRECTORY_ADMISSION_SECRET_BYTES} bytes`,
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

function sameDirectoryAdmission(left: DirectoryAdmission, right: DirectoryAdmission): boolean {
  return left.token === right.token &&
    left.directoryId === right.directoryId &&
    left.generation === right.generation &&
    sameOutputPath(left.path, right.path) &&
    left.parentToken === right.parentToken &&
    sameModifiedTime(left, right)
}

function modifiedTimeKey(input: OutputDirectoryAdmission): readonly unknown[] | null {
  if (input.modifiedTime === undefined) return null
  return [
    input.modifiedTime.seconds.toString(),
    input.modifiedTime.nanoseconds,
    input.modifiedTime.precision,
  ]
}
