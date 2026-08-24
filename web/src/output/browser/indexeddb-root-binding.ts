import {
  fsaTreeGuarantees,
  validateDestinationReservation,
  validateReceiveIntent,
  type FSANamedContainerEntryReservation,
  type ArtifactChoiceID,
  type ReceiveIntent,
} from '../../transfer/intent'
import { sameGuaranteeFacts } from '../planning'
import type {
  CompatibleNameOperationBootstrapV1,
  CompatibleNameOperationHeaderV1,
  CompatibleNamePairKind,
} from '../file-system-access/compatible-name/model'
import { TargetOwnershipUnknownError } from '../persistent-tree/errors'
import {
  runPersistentOutputStage,
  type PersistentOutputStage,
  type PersistentOutputStageScope,
} from '../persistent-tree/stage-diagnostics'
import { captureFSAFailureFacts } from './filesystem-failure-facts'
import {
  RECEIVE_RECORD_RESERVATION,
  RECEIVE_RECORD_OPERATION,
  createPersistedReceiveRecord,
  createReceiveOperationV2,
  decodeStoredReceiveOperation,
  operationRecordId,
  receiveOperationHandleRecord,
  storedReceiveOperationRecord,
  type PersistedReceiveRecord,
  type ReceiveOperationHandleRecord,
} from '../workspace/records'
import type {
  ReceiveOperationRepository,
  ReceiveOperationTransition,
} from '../workspace/repository'
import { equalCanonicalBytes } from '../workspace/canonical'

export const FSA_OPERATION_HANDLE_PARENT = 1 as const
export const FSA_OPERATION_HANDLE_DIRECTORY = 2 as const
export const FSA_OPERATION_HANDLE_COMPATIBLE_NAME_SCRIPT = 3 as const
export const FSA_OPERATION_HANDLE_COMPATIBLE_NAME_SIDECAR = 4 as const

const FSA_PARENT_HANDLE_DOMAIN = 'windshare/fsa-parent-handle/v1'
const FSA_DIRECTORY_HANDLE_DOMAIN = 'windshare/fsa-directory-handle/v1'

export interface PersistedFSAOperationBinding {
  readonly intent: ReceiveIntent
  readonly reservation: FSANamedContainerEntryReservation
  readonly parent: FileSystemDirectoryHandle
  readonly parentHandleId: string
}

export interface PreparedFSAOperationBindingTransition {
  readonly intent: ReceiveIntent
  readonly reservation: FSANamedContainerEntryReservation
  readonly parent: FileSystemDirectoryHandle
  readonly parentHandleId: string
  readonly transition: Pick<ReceiveOperationTransition, 'records' | 'handles'>
}

export type FSAOperationBindingRepository = Pick<
  ReceiveOperationRepository,
  'commitTransition' | 'readRecord' | 'readHandle'
>

export type FSACompatibleNamePairHandleRepository = Pick<
  ReceiveOperationRepository,
  'readHandle'
>

/**
 * Rejected-root activation uses this separate port so the generic receive transition
 * never learns an FSA-only repair flag or permits the repair header to commit later.
 */
export interface FSACompatibleNameBootstrapRepository {
  commitFSACompatibleNameBootstrap(input: Readonly<{
    transition: ReceiveOperationTransition
    bootstrap: CompatibleNameOperationBootstrapV1
  }>): Promise<void>
}

/**
 * Preparation is deliberately read-only so lifecycle and lease ownership can join
 * these records in the caller's single durable transition.
 */
export async function prepareFSAOperationBindingTransition(input: Readonly<{
  repository: FSAOperationBindingRepository
  intent: ReceiveIntent
  parent: FileSystemDirectoryHandle
  preClickRanking: readonly ArtifactChoiceID[]
}>): Promise<PreparedFSAOperationBindingTransition> {
  const validated = await validatedFSAIntent(input.intent)
  const operation = await createReceiveOperationV2({
    receiveIntent: validated.intent,
    preClickRanking: input.preClickRanking,
  })
  const operationRecord = storedReceiveOperationRecord(operation)
  const reservationRecord = await createPersistedReceiveRecord({
    operationId: validated.intent.operationId,
    kind: RECEIVE_RECORD_RESERVATION,
    canonicalBytes: validated.reservation.canonicalBytes,
  })
  const parentHandleId = fsaParentHandleId(
    validated.intent.operationId,
    validated.reservation.authorityRef,
  )
  const parentRecord = receiveOperationHandleRecord({
    id: parentHandleId,
    operationId: validated.intent.operationId,
    kind: FSA_OPERATION_HANDLE_PARENT,
    authorityRef: validated.reservation.authorityRef,
    handle: input.parent,
  })

  await assertCompatibleExistingParent(input.repository, parentRecord, input.parent)
  await assertCompatibleExistingRecord(input.repository, operationRecord)
  await assertCompatibleExistingRecord(input.repository, reservationRecord)
  return Object.freeze({
    intent: validated.intent,
    reservation: validated.reservation,
    parent: input.parent,
    parentHandleId,
    transition: Object.freeze({
      records: Object.freeze([operationRecord, reservationRecord]),
      handles: Object.freeze([parentRecord]),
    }),
  })
}

export async function verifyFSAOperationBinding(input: Readonly<{
  repository: FSAOperationBindingRepository
  intent: ReceiveIntent
  expectedParent?: FileSystemDirectoryHandle
  stageScope?: PersistentOutputStageScope
}>): Promise<PersistedFSAOperationBinding> {
  const validated = await validatedFSAIntent(input.intent)
  const reservationRecord = await createPersistedReceiveRecord({
    operationId: validated.intent.operationId,
    kind: RECEIVE_RECORD_RESERVATION,
    canonicalBytes: validated.reservation.canonicalBytes,
  })
  const parentHandleId = fsaParentHandleId(
    validated.intent.operationId,
    validated.reservation.authorityRef,
  )
  if (input.expectedParent !== undefined) {
    const expectedParent = input.expectedParent
    input.stageScope?.addFailureFacts('binding', context => captureFSAFailureFacts({
      target: { kind: 'handle', resolve: async () => expectedParent },
      permissionFallback: expectedParent,
      expectedKind: 'directory',
      readPersistedHandle: async () =>
        (await input.repository.readHandle<FileSystemDirectoryHandle>(parentHandleId))?.handle,
    }, context))
  }
  let storedOperation: PersistedReceiveRecord | undefined
  let storedReservation: PersistedReceiveRecord | undefined
  let storedParent: ReceiveOperationHandleRecord<FileSystemDirectoryHandle> | undefined
  try {
    [storedOperation, storedReservation, storedParent] = await Promise.all([
      runPersistentOutputStage(
        input.stageScope,
        'indexeddb.binding.operation.read',
        () => input.repository.readRecord(operationRecordId(
          validated.intent.operationId,
          RECEIVE_RECORD_OPERATION,
        )),
      ),
      runPersistentOutputStage(
        input.stageScope,
        'indexeddb.binding.reservation.read',
        () => input.repository.readRecord(reservationRecord.id),
      ),
      runPersistentOutputStage(
        input.stageScope,
        'indexeddb.binding.parent-handle.read',
        () => input.repository.readHandle<FileSystemDirectoryHandle>(parentHandleId),
      ),
    ])
  } catch (cause) {
    throw ownershipUnknown('reservation', validated.intent.operationId, cause)
  }
  const storedParentHandle = storedParent === undefined
    ? undefined
    : requireDirectoryHandle(
        storedParent.handle,
        validated.intent.operationId,
        'parent-authority',
      )
  let decodedOperation
  try {
    decodedOperation = storedOperation === undefined
      ? undefined
      : await decodeStoredReceiveOperation(storedOperation)
  } catch {
    decodedOperation = undefined
  }
  if (decodedOperation === undefined ||
      decodedOperation.receiveIntentDigest !== validated.intent.digest ||
      !equalCanonicalBytes(decodedOperation.receiveIntent.canonicalBytes, validated.intent.canonicalBytes) ||
      !samePersistedRecord(reservationRecord, storedReservation) ||
      storedParent === undefined || storedParent.kind !== FSA_OPERATION_HANDLE_PARENT ||
      storedParent.operationId !== validated.intent.operationId ||
      storedParent.authorityRef !== validated.reservation.authorityRef ||
      storedParent.ownedObjectId !== undefined || storedParentHandle === undefined) {
    throw new TargetOwnershipUnknownError('reservation', validated.intent.operationId)
  }
  if (input.expectedParent !== undefined &&
      !await sameEntry(
        input.expectedParent,
        storedParentHandle,
        validated.intent.operationId,
        'parent-authority',
        input.stageScope,
        'fsa.binding.parent-handle.verify',
      )) {
    throw new TargetOwnershipUnknownError('parent-authority', validated.intent.operationId)
  }
  return Object.freeze({
    intent: validated.intent,
    reservation: validated.reservation,
    parent: storedParentHandle,
    parentHandleId,
  })
}

export async function persistFSAOwnedDirectory(input: Readonly<{
  repository: FSAOperationBindingRepository
  reservation: FSANamedContainerEntryReservation
  handleId: string
  ownedObjectId: string
  handle: FileSystemDirectoryHandle
  diagnosticTarget?: 'root' | 'directory'
  stageScope?: PersistentOutputStageScope
}>): Promise<void> {
  requireOwnedDirectoryHandleId(input.handleId, input.reservation.operationId)
  const record = receiveOperationHandleRecord({
    id: input.handleId,
    operationId: input.reservation.operationId,
    kind: FSA_OPERATION_HANDLE_DIRECTORY,
    authorityRef: input.reservation.authorityRef,
    ownedObjectId: input.ownedObjectId,
    handle: input.handle,
  })
  let existing: ReceiveOperationHandleRecord<FileSystemDirectoryHandle> | undefined
  try {
    existing = await runPersistentOutputStage(
      input.stageScope,
      ownedDirectoryStage(input.diagnosticTarget, 'read'),
      () => input.repository.readHandle<FileSystemDirectoryHandle>(input.handleId),
    )
  } catch (cause) {
    throw ownershipUnknown('namespace-create', input.reservation.operationId, cause)
  }
  if (existing !== undefined &&
      !await sameOwnedDirectoryRecord(
        existing,
        record,
        input.reservation.operationId,
        input.stageScope,
        input.diagnosticTarget,
      )) {
    throw new TargetOwnershipUnknownError('namespace-create', input.reservation.operationId)
  }
  try {
    await runPersistentOutputStage(
      input.stageScope,
      ownedDirectoryStage(input.diagnosticTarget, 'persist'),
      () => input.repository.commitTransition({
        operationId: input.reservation.operationId,
        handles: [record],
      }),
    )
  } catch (cause) {
    throw new TargetOwnershipUnknownError(
      'namespace-create',
      input.reservation.operationId,
      { cause },
    )
  }
  let committed: ReceiveOperationHandleRecord<FileSystemDirectoryHandle> | undefined
  try {
    committed = await runPersistentOutputStage(
      input.stageScope,
      ownedDirectoryStage(input.diagnosticTarget, 'committed-read'),
      () => input.repository.readHandle<FileSystemDirectoryHandle>(input.handleId),
    )
  } catch (cause) {
    throw ownershipUnknown('namespace-create', input.reservation.operationId, cause)
  }
  if (committed === undefined ||
      !await sameOwnedDirectoryRecord(
        committed,
        record,
        input.reservation.operationId,
        input.stageScope,
        input.diagnosticTarget,
      )) {
    throw new TargetOwnershipUnknownError('namespace-create', input.reservation.operationId)
  }
}

export async function readFSAOwnedDirectory(input: Readonly<{
  repository: FSAOperationBindingRepository
  reservation: FSANamedContainerEntryReservation
  handleId: string
  ownedObjectId?: string
  diagnosticTarget?: 'root' | 'directory'
  stageScope?: PersistentOutputStageScope
}>): Promise<FileSystemDirectoryHandle | undefined> {
  requireOwnedDirectoryHandleId(input.handleId, input.reservation.operationId)
  let record: ReceiveOperationHandleRecord<FileSystemDirectoryHandle> | undefined
  try {
    record = await runPersistentOutputStage(
      input.stageScope,
      ownedDirectoryStage(input.diagnosticTarget, 'read'),
      () => input.repository.readHandle<FileSystemDirectoryHandle>(input.handleId),
    )
  } catch (cause) {
    throw ownershipUnknown('parent-authority', input.reservation.operationId, cause)
  }
  if (record === undefined) return undefined
  const handle = requireDirectoryHandle(
    record.handle,
    input.reservation.operationId,
    'parent-authority',
  )
  if (record.kind !== FSA_OPERATION_HANDLE_DIRECTORY ||
      record.operationId !== input.reservation.operationId ||
      record.authorityRef !== input.reservation.authorityRef ||
      (input.ownedObjectId !== undefined && record.ownedObjectId !== input.ownedObjectId)) {
    throw new TargetOwnershipUnknownError('parent-authority', input.reservation.operationId)
  }
  return handle
}

/** Reopen verifies the exact persisted pair handle instead of trusting its claimed physical name. */
export async function readFSACompatibleNamePairHandle(input: Readonly<{
  repository: FSACompatibleNamePairHandleRepository
  header: CompatibleNameOperationHeaderV1
  pairKind: CompatibleNamePairKind
}>): Promise<FileSystemFileHandle> {
  const identity = input.header.pair[input.pairKind]
  let record: ReceiveOperationHandleRecord<FileSystemFileHandle> | undefined
  try {
    record = await input.repository.readHandle<FileSystemFileHandle>(identity.handleId)
  } catch (cause) {
    throw new TargetOwnershipUnknownError('parent-authority', input.header.operationId, { cause })
  }
  const expectedKind = input.pairKind === 'script'
    ? FSA_OPERATION_HANDLE_COMPATIBLE_NAME_SCRIPT
    : FSA_OPERATION_HANDLE_COMPATIBLE_NAME_SIDECAR
  if (record === undefined || record.kind !== expectedKind ||
      record.operationId !== input.header.operationId ||
      record.authorityRef !== input.header.authorityRef ||
      record.ownedObjectId !== identity.ownedObjectId || !isFileHandle(record.handle)) {
    throw new TargetOwnershipUnknownError('parent-authority', input.header.operationId)
  }
  return record.handle
}

export function fsaParentHandleId(operationId: string, authorityRef: string): string {
  return `${FSA_PARENT_HANDLE_DOMAIN}/${operationId}/${authorityRef}`
}

export function fsaOwnedDirectoryHandleId(
  operationId: string,
  opaqueLocatorDigest: string,
): string {
  if (typeof opaqueLocatorDigest !== 'string' || opaqueLocatorDigest.length === 0 ||
      opaqueLocatorDigest.includes('/')) {
    throw new TypeError('FSA owned-directory locator digest is invalid')
  }
  return `${FSA_DIRECTORY_HANDLE_DOMAIN}/${operationId}/${opaqueLocatorDigest}`
}

async function validatedFSAIntent(intentInput: ReceiveIntent): Promise<{
  readonly intent: ReceiveIntent
  readonly reservation: FSANamedContainerEntryReservation
}> {
  const intent = await validateReceiveIntent(intentInput)
  if (intent.plan.kind !== 'direct-tree' || intent.artifact.kind !== 'directory-tree' ||
      intent.plan.reservation.kind !== 'named-container-entry' ||
      intent.plan.reservation.authorityKind !== 'fsa-container') {
    throw new TypeError('FSA binding requires a named DirectTree reservation')
  }
  const reservation = await validateDestinationReservation(
    intent.plan.reservation,
    intent.artifact,
  ) as FSANamedContainerEntryReservation
  const expected = fsaTreeGuarantees()
  if (!sameGuaranteeFacts(reservation.guarantees, expected) ||
      reservation.guarantees.profile !== 'fsa-tree') {
    throw new TypeError('FSA reservation does not use CoordinatedNoReplace and PrefixVisible')
  }
  return Object.freeze({ intent, reservation })
}

async function assertCompatibleExistingParent(
  repository: FSAOperationBindingRepository,
  record: ReceiveOperationHandleRecord<FileSystemDirectoryHandle>,
  parent: FileSystemDirectoryHandle,
): Promise<void> {
  const existing = await repository.readHandle<FileSystemDirectoryHandle>(record.id)
  if (existing === undefined) return
  const existingHandle = requireDirectoryHandle(
    existing.handle,
    record.operationId,
    'parent-authority',
  )
  if (existing.kind !== record.kind || existing.operationId !== record.operationId ||
      existing.authorityRef !== record.authorityRef || existing.ownedObjectId !== undefined ||
      !await sameEntry(
        existingHandle,
        parent,
        record.operationId,
        'parent-authority',
      )) {
    throw new TargetOwnershipUnknownError('parent-authority', record.operationId)
  }
}

async function assertCompatibleExistingRecord(
  repository: FSAOperationBindingRepository,
  expected: PersistedReceiveRecord,
): Promise<void> {
  const existing = await repository.readRecord(expected.id)
  if (existing !== undefined && !samePersistedRecord(expected, existing)) {
    throw new TargetOwnershipUnknownError('reservation', expected.operationId)
  }
}

function samePersistedRecord(
  expected: PersistedReceiveRecord,
  actual: PersistedReceiveRecord | undefined,
): boolean {
  return actual !== undefined && actual.id === expected.id && actual.kind === expected.kind &&
    actual.operationId === expected.operationId && actual.digest === expected.digest &&
    equalCanonicalBytes(actual.canonicalBytes, expected.canonicalBytes)
}

async function sameOwnedDirectoryRecord(
  existing: ReceiveOperationHandleRecord<FileSystemDirectoryHandle>,
  expected: ReceiveOperationHandleRecord<FileSystemDirectoryHandle>,
  operationId: string,
  stageScope?: PersistentOutputStageScope,
  diagnosticTarget: 'root' | 'directory' = 'directory',
): Promise<boolean> {
  const existingHandle = requireDirectoryHandle(existing.handle, operationId, 'namespace-create')
  const expectedHandle = requireDirectoryHandle(expected.handle, operationId, 'namespace-create')
  return existing.id === expected.id && existing.operationId === expected.operationId &&
    existing.kind === expected.kind && existing.authorityRef === expected.authorityRef &&
    existing.ownedObjectId === expected.ownedObjectId &&
    await sameEntry(
      existingHandle,
      expectedHandle,
      operationId,
      'namespace-create',
      stageScope,
      diagnosticTarget === 'root'
        ? 'fsa.root.handle.verify'
        : 'fsa.directory.handle.verify',
    )
}

function requireDirectoryHandle(
  value: unknown,
  operationId: string,
  stage: 'parent-authority' | 'namespace-create',
): FileSystemDirectoryHandle {
  if (typeof value !== 'object' || value === null ||
      !('kind' in value) || (value as { readonly kind?: unknown }).kind !== 'directory' ||
      !('isSameEntry' in value) ||
      typeof (value as { readonly isSameEntry?: unknown }).isSameEntry !== 'function') {
    throw new TargetOwnershipUnknownError(stage, operationId)
  }
  return value as FileSystemDirectoryHandle
}

function isFileHandle(value: unknown): value is FileSystemFileHandle {
  return typeof value === 'object' && value !== null && 'kind' in value &&
    (value as { readonly kind?: unknown }).kind === 'file' && 'isSameEntry' in value &&
    typeof (value as { readonly isSameEntry?: unknown }).isSameEntry === 'function'
}

async function sameEntry(
  left: FileSystemHandle,
  right: FileSystemHandle,
  operationId: string,
  stage: 'parent-authority' | 'namespace-create',
  stageScope?: PersistentOutputStageScope,
  diagnosticStage?: PersistentOutputStage,
): Promise<boolean> {
  try {
    return diagnosticStage === undefined
      ? await left.isSameEntry(right)
      : await runPersistentOutputStage(
          stageScope,
          diagnosticStage,
          () => left.isSameEntry(right),
        )
  } catch (cause) {
    throw new TargetOwnershipUnknownError(stage, operationId, { cause })
  }
}

function ownedDirectoryStage(
  target: 'root' | 'directory' = 'directory',
  operation: 'read' | 'persist' | 'committed-read',
): PersistentOutputStage {
  if (target === 'root') {
    switch (operation) {
      case 'read': return 'indexeddb.root-handle.read'
      case 'persist': return 'indexeddb.root-handle.persist'
      case 'committed-read': return 'indexeddb.root-handle.committed-read'
    }
  }
  switch (operation) {
    case 'read': return 'indexeddb.directory-handle.read'
    case 'persist': return 'indexeddb.directory-handle.persist'
    case 'committed-read': return 'indexeddb.directory-handle.committed-read'
  }
}

function ownershipUnknown(
  stage: 'parent-authority' | 'reservation' | 'namespace-create',
  operationId: string,
  cause: unknown,
): TargetOwnershipUnknownError {
  return cause instanceof TargetOwnershipUnknownError
    ? cause
    : new TargetOwnershipUnknownError(stage, operationId, { cause })
}

function requireOwnedDirectoryHandleId(value: string, operationId: string): void {
  if (!value.startsWith(`${FSA_DIRECTORY_HANDLE_DOMAIN}/${operationId}/`)) {
    throw new TypeError('FSA owned-directory handle escaped its operation')
  }
}

export type { ReceiveOperationRepository, ReceiveOperationTransition }
