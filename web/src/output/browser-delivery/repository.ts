import type { FinalFileCheckpointProof } from '../persistence/journal'
import type { BrowserDeliveryRecordV1, BrowserSavePolicyV1 } from './model'

export interface BrowserDeliveryFileScan {
  readonly operationId: string
  readonly afterFileId?: string
  readonly limit?: number
}
export interface BrowserDeliveryFilePage {
  readonly records: readonly BrowserDeliveryRecordV1[]
  readonly nextFileId?: string
}

/** Physical writers still require the operation lease; metadata transitions additionally use exact CAS. */
export interface BrowserDeliveryRepository {
  installPolicy(policy: BrowserSavePolicyV1): Promise<BrowserSavePolicyV1>
  readPolicy(operationId: string): Promise<BrowserSavePolicyV1 | undefined>
  createFile(record: BrowserDeliveryRecordV1): Promise<BrowserDeliveryRecordV1>
  readFile(operationId: string, fileId: string): Promise<BrowserDeliveryRecordV1 | undefined>
  replaceFile(previous: BrowserDeliveryRecordV1, next: BrowserDeliveryRecordV1): Promise<void>
  authorizeRestart(previous: BrowserDeliveryRecordV1, checkpoint: import('../persistence/checkpoint').FileCheckpointV2, authorizationId: string): Promise<BrowserDeliveryRecordV1>
  /** A direct file has no intermediate staging to clean; finalize its two semantic states in one durable cut. */
  finalizeDirect(previous: BrowserDeliveryRecordV1, proof: FinalFileCheckpointProof): Promise<BrowserDeliveryRecordV1>
  scanFiles(scan: BrowserDeliveryFileScan): Promise<BrowserDeliveryFilePage>
  close(): void
}

export class BrowserDeliveryConcurrencyError extends DOMException {
  constructor(message = 'Browser delivery policy, source, or generation changed') {
    super(message, 'InvalidStateError')
  }
}
