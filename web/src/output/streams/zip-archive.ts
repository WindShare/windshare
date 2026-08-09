import type {
  SealedZipLayoutPlanV1,
  ZipLayoutLedgerV1,
} from '../zip-layout/layout'
import type { ZipEntryPlanV1 } from '../zip-layout/policy'

export type ZipArchiveLayoutAuthority =
  | Readonly<{ kind: 'sealed'; plan: SealedZipLayoutPlanV1 }>
  | Readonly<{ kind: 'progressive'; ledger: ZipLayoutLedgerV1 }>

export interface ZipArchiveMember {
  write(data: Uint8Array): Promise<void>
  close(): Promise<void>
  abort(reason: unknown): Promise<void>
}

export interface ZipArchiveWriter {
  readonly cleanupPending: boolean
  readonly cleanupFailure: unknown
  addDirectory(entry: ZipEntryPlanV1): Promise<void>
  beginFile(entry: ZipEntryPlanV1): Promise<ZipArchiveMember>
  close(proof: SealedZipLayoutPlanV1, signal: AbortSignal): Promise<void>
  abort(reason: unknown): Promise<void>
  retryCleanup(): Promise<void>
}
