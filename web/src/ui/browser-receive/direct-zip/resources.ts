import { encodeBase64Url, equalBytes } from '../../../crypto/bytes'
import { browserReceiveOperationLockName } from '../../../output/browser/session-lease'
import {
  createDirectZipBrowserFileSystemPort,
  createDirectZipTarget,
  type DirectZipHandleBindingPort,
  type DirectZipReservationCandidatePort,
  type DirectZipReservationCandidate,
  type DirectZipParentLockPort,
} from '../../../output/direct-zip/target'
import type { DirectZipBootstrapCandidateV1, DirectZipPolicyDigestsV1 } from '../../../output/direct-zip/journal'
import {
  receiveOperationHandleRecord,
  type ReceiveOperationHandleRecord,
} from '../../../output/workspace/records'
import type { ReceiveOperationRepository } from '../../../output/workspace/repository'
import type { BoundReceiveIntent } from '../../../output/planning'
import { validateReceiveIntent, type AvailableDirectZipPolicyDigests } from '../../../transfer/intent'
import { equalDirectZipOwnershipMarkersV1 } from '../../../output/direct-zip/format'
import type { BrowserReceiveWindow } from '../contracts'
import { digestText } from '../shared'
import { bytes, type BrowserDirectZipBinding } from './target'

const DIRECT_ZIP_HANDLE_KIND = 4
const DIRECT_ZIP_HANDLE_DOMAIN = 'windshare/direct-zip-browser-authority/v1'
export const browserDirectZipFileSystem = () => createDirectZipBrowserFileSystemPort({ enabled: true })

export async function requestBrowserDirectZipAuthorization(parent: FileSystemDirectoryHandle): Promise<void> {
  // Start the native request in the click stack, before storage I/O can expire
  // user activation. Already-granted permission resolves without a prompt.
  if (await browserDirectZipFileSystem().requestPermission(parent) !== 'granted') {
    throw new DOMException('Access to the saved ZIP is required', 'NotAllowedError')
  }
}

export interface BrowserDirectZipEnvelope {
  readonly version: 1
  readonly frozen: BoundReceiveIntent
  readonly candidate: DirectZipReservationCandidate<FileSystemDirectoryHandle>
  readonly binding?: BrowserDirectZipBinding
}

export function browserDirectZipHandleId(operationId: string) {
  return `${DIRECT_ZIP_HANDLE_DOMAIN}/${operationId}`
}

export function envelopeRecord(envelope: BrowserDirectZipEnvelope):
  ReceiveOperationHandleRecord<BrowserDirectZipEnvelope> {
  return receiveOperationHandleRecord({
    id: browserDirectZipHandleId(envelope.frozen.intent.operationId),
    operationId: envelope.frozen.intent.operationId,
    kind: DIRECT_ZIP_HANDLE_KIND,
    authorityRef: encodeBase64Url(envelope.candidate.targetRef),
    handle: envelope,
  })
}

export async function readEnvelope(repository: ReceiveOperationRepository, operationId: string) {
  const record = await repository.readHandle<BrowserDirectZipEnvelope>(browserDirectZipHandleId(operationId))
  const envelope = record?.handle
  if (envelope?.version !== 1 || envelope.frozen.intent.operationId !== operationId ||
      encodeBase64Url(envelope.candidate.operationId) !== operationId ||
      envelope.frozen.intent.plan.kind !== 'direct-resumable-zip' ||
      encodeBase64Url(envelope.candidate.bindingDigest) !== envelope.frozen.intent.plan.binding.digest ||
      envelope.candidate.stableName !== envelope.frozen.intent.plan.binding.stableName) {
    throw new DOMException('The retained ZIP has no matching browser authority', 'DataError')
  }
  await validateReceiveIntent(envelope.frozen.intent)
  if (envelope.binding !== undefined) {
    const { binding, candidate } = envelope
    if (!equalBytes(binding.operationId, candidate.operationId) ||
        !equalBytes(binding.bindingDigest, candidate.bindingDigest) ||
        !equalBytes(binding.targetRef, candidate.targetRef) ||
        !equalDirectZipOwnershipMarkersV1(binding.marker, candidate.marker) ||
        binding.resultRootComponent !== candidate.resultRootComponent ||
        binding.stableName !== candidate.stableName ||
        !equalBytes(binding.parentBinding.bindingDigest, candidate.parentBinding.bindingDigest) ||
        !await binding.parentBinding.persistedHandle.isSameEntry(candidate.parentBinding.persistedHandle)) {
      throw new DOMException('The retained ZIP file binding belongs to another authority', 'DataError')
    }
  }
  return envelope
}

export async function acquireOperationLock(windowPort: BrowserReceiveWindow, operationId: string) {
  let entered!: () => void
  let rejected!: (error: unknown) => void
  let released!: () => void
  const acquired = new Promise<void>((resolve, reject) => { entered = resolve; rejected = reject })
  const held = new Promise<void>(resolve => { released = resolve })
  const completion = windowPort.navigator.locks.request(
    browserReceiveOperationLockName(operationId), { mode: 'exclusive', ifAvailable: true },
    async lock => {
      if (lock === null) {
        rejected(new DOMException('This ZIP is active in another browser tab', 'InvalidStateError'))
        return
      }
      entered()
      await held
    },
  )
  completion.catch(rejected)
  await acquired
  return async () => { released(); await completion }
}

export function browserTarget(input: {
  readonly leaseId: string
  readonly reservations: DirectZipReservationCandidatePort<FileSystemDirectoryHandle>
  readonly parentLocks: DirectZipParentLockPort<FileSystemDirectoryHandle>
  readonly claimFile: (file: FileSystemFileHandle) => Promise<void>
}) {
  const bindings: DirectZipHandleBindingPort<FileSystemDirectoryHandle, FileSystemFileHandle> = {
    compareParent: async (binding, current) =>
      await binding.persistedHandle.isSameEntry(current) ? 'same' : 'different',
    compareFile: async (binding, current) =>
      await binding.persistedHandle.isSameEntry(current) ? 'same' : 'different',
    compareCurrentFiles: async (left, right) => await left.isSameEntry(right) ? 'same' : 'different',
    bindFile: async ({ targetRef, stableName, file }) => {
      // Transfer bootstrap's short namespace protection to the concrete file lease
      // before the target reservation releases its parent lock.
      await input.claimFile(file)
      return {
        handleRef: encodeBase64Url(targetRef),
        bindingDigest: bytes(await digestText(`windshare/direct-zip-file-locator/v1\n${encodeBase64Url(targetRef)}\n${stableName}`)),
        persistedHandle: file,
      }
    },
  }
  return createDirectZipTarget({
    fileSystem: browserDirectZipFileSystem(),
    handleBindings: bindings,
    reservations: input.reservations,
    // The storage owner already holds the operation lease; namespace protection
    // belongs to each reservation attempt and ends before archive transfer starts.
    operationLeases: { acquire: async () => ({
      leaseId: input.leaseId, generation: 1n, release: async () => undefined,
    }) },
    parentLocks: input.parentLocks,
    random: { bytes: randomBytes },
    maximumReservationCandidates: 1,
  })
}

export function randomBytes(length: number) { return crypto.getRandomValues(new Uint8Array(length)) }
export function randomId() { return encodeBase64Url(randomBytes(16)) }

export function journalPolicies(policies: AvailableDirectZipPolicyDigests): DirectZipPolicyDigestsV1 {
  return {
    encodingPolicyDigest: policies.zipEncoding, layoutPolicyDigest: policies.layout,
    checkpointPolicyDigest: policies.checkpoint, journalBudgetDigest: policies.journalBudget,
    epochPolicyDigest: policies.epoch,
  }
}

export function requireBootstrapEnvelope(candidate: DirectZipBootstrapCandidateV1, envelope: BrowserDirectZipEnvelope) {
  if (candidate.targetBindingDigest !== encodeBase64Url(envelope.candidate.bindingDigest) ||
      candidate.stablePhysicalName !== envelope.candidate.stableName ||
      candidate.ownershipNonce !== encodeBase64Url(envelope.candidate.ownershipNonce)) {
    throw new DOMException('ZIP bootstrap authority changed', 'DataError')
  }
}
