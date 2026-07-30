export type TrustedZipFailureKind =
  | 'invalid-archive'
  | 'archive-entry-limit'
  | 'archive-expanded-byte-limit'
  | 'archive-path'

export class TrustedZipFailure extends Error {
  readonly kind: TrustedZipFailureKind
  readonly observedEntryCount?: number
  readonly observedExpandedBytes?: number

  constructor(
    kind: TrustedZipFailureKind,
    message: string,
    evidence: {
      readonly observedEntryCount?: number
      readonly observedExpandedBytes?: number
      readonly cause?: unknown
    } = {},
  ) {
    super(message, evidence.cause === undefined ? undefined : { cause: evidence.cause })
    this.name = 'TrustedZipFailure'
    this.kind = kind
    if (evidence.observedEntryCount !== undefined) {
      this.observedEntryCount = evidence.observedEntryCount
    }
    if (evidence.observedExpandedBytes !== undefined) {
      this.observedExpandedBytes = evidence.observedExpandedBytes
    }
  }
}

/** The guard implements this against an already-opened, identity-checked handle. */
export interface ArchiveByteSource {
  readonly byteLength: number
  readExactly(offset: number, length: number): Promise<Uint8Array>
}

export interface TrustedZipLimits {
  readonly maximumEntries: number
  readonly maximumExpandedBytes: number
  readonly maximumPathBytes: number
}

export interface TrustedZipEntry {
  readonly path: string
  readonly directory: boolean
  readonly compressedBytes: number
  readonly expandedBytes: number
}

/**
 * The parser owns stream completion and integrity checks so a visitor cannot
 * accidentally turn a partial entry read into scan authority.
 */
export interface TrustedZipEntryVisitor {
  start(entry: TrustedZipEntry): void | Promise<void>
  chunk(entry: TrustedZipEntry, bytes: Uint8Array): void | Promise<void>
  end(entry: TrustedZipEntry): void | Promise<void>
}

export interface TrustedZipScanSummary {
  readonly entryCount: number
  readonly expandedBytes: number
  readonly archiveBaseOffset: number
}
