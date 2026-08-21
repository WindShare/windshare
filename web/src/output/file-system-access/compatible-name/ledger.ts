import type {
  CompatibleNameEntryKind,
  CompatibleNameMappingSpec,
  CompatibleNameMappingV1,
  CompatibleNameOperationBootstrapV1,
  CompatibleNameOperationHeaderV1,
  CompatibleNameOperationSnapshotV1,
  CompatibleNamePairKind,
  CompatibleNamePendingTerminalOutcomeV1,
  CompatibleNameRepairSummary,
} from './model'

export interface CompatibleNamePairOwnershipInput {
  readonly operationId: string
  readonly pairKind: CompatibleNamePairKind
  readonly handle: FileSystemFileHandle
}

export interface CompatibleNameMappingOwnershipInput {
  readonly operationId: string
  readonly logicalPath: readonly string[]
  readonly entryKind: CompatibleNameEntryKind
  readonly ownedObjectId: string
}

export type CompatibleNameMappingCommitInput = CompatibleNameMappingOwnershipInput

export interface CompatibleNameTargetCreatedInput {
  readonly operationId: string
  readonly logicalPath: readonly string[]
  readonly entryKind: CompatibleNameEntryKind
  readonly repairSummary: CompatibleNameRepairSummary
}

export interface CompatibleNamePendingTerminalInput {
  readonly operationId: string
  readonly outcome: CompatibleNamePendingTerminalOutcomeV1
  readonly repairSummary: CompatibleNameRepairSummary
}

export interface CompatibleNameTerminalRepairInput {
  readonly operationId: string
  readonly repairSummary: CompatibleNameRepairSummary
}

/**
 * This boundary keeps logical transfer identity independent from browser-only physical names.
 * Callers update their session resolver only after the corresponding mutation resolves.
 */
export interface CompatibleNameLedger {
  readHeader(operationId: string): Promise<CompatibleNameOperationHeaderV1 | undefined>
  loadOperation(operationId: string): Promise<CompatibleNameOperationSnapshotV1 | undefined>
  bootstrapOperation(
    bootstrap: CompatibleNameOperationBootstrapV1,
  ): Promise<CompatibleNameOperationSnapshotV1>
  claimMapping(selection: CompatibleNameMappingSpec): Promise<CompatibleNameMappingV1>
  recordPairOwnership(input: CompatibleNamePairOwnershipInput): Promise<CompatibleNameOperationHeaderV1>
  recordCompatibleTargetCreated(
    input: CompatibleNameTargetCreatedInput,
  ): Promise<CompatibleNameOperationHeaderV1>
  recordVerifiedDirectoryOwnership(
    input: CompatibleNameMappingOwnershipInput,
  ): Promise<CompatibleNameMappingV1>
  commitMapping(input: CompatibleNameMappingCommitInput): Promise<CompatibleNameMappingV1>
  scanCommittedMappings(
    operationId: string,
    afterOrdinal?: number,
  ): Promise<readonly CompatibleNameMappingV1[]>
  persistRepairSummary(
    operationId: string,
    repairSummary: CompatibleNameRepairSummary,
  ): Promise<CompatibleNameOperationHeaderV1>
  persistPendingTerminalOutcome(
    input: CompatibleNamePendingTerminalInput,
  ): Promise<CompatibleNameOperationHeaderV1>
  readPendingTerminalOutcome(
    operationId: string,
  ): Promise<CompatibleNamePendingTerminalOutcomeV1 | undefined>
  clearPendingTerminalOutcome(
    input: CompatibleNameTerminalRepairInput,
  ): Promise<CompatibleNameOperationHeaderV1>
  readRepairSummary(operationId: string): Promise<CompatibleNameRepairSummary | undefined>
  removeVerifiedEmptyOperation(
    expectedHeader: CompatibleNameOperationHeaderV1,
  ): Promise<void>
  close(): void
}
