import type { TransferIntent } from '../../../transfer/intent'
import {
  assertPausedTaskCurrentShare,
  createRootCapabilityRef,
  pausedTaskDescriptorKey,
  pausedTaskDescriptorNamespace,
  pausedTaskDescriptorV1,
  samePausedTaskDescriptor,
  validatePausedTaskDescriptorV1,
  type PausedTaskDescriptorV1,
} from '../../resume/descriptor'
import { durableCheckpointNamespaceKey } from '../../persistence/namespace'
import {
  observePausedTask,
  ResumeStateInventory,
  ResumeStateRef,
  type PausedTaskDescriptorRepository,
  type ReconstructedPausedTask,
  type ResumeStateAuthority,
  type ResumeStateDiscardResult,
  type ResumeStateOperationRequest,
  type ResumeStateReferenceOwner,
} from '../../resume/authority'
import {
  DEFAULT_OUTPUT_CHECKPOINT_DATABASE_NAME,
  INDEXEDDB_CHECKPOINT_METADATA_STORE,
  INDEXEDDB_PAUSED_TASK_DESCRIPTOR_STORE,
  INDEXEDDB_ROOT_CAPABILITY_STORE,
  openIndexedDbCheckpointDatabase,
  requestResult,
  transactionCompletion,
} from '../indexeddb-database'
import { verifyIndexedDbRootIdentity } from '../indexeddb-root-binding'
import {
  browserPausedTaskDependencies,
  resumePreparedPausedTask,
  type BrowserPausedTaskDependencies,
  type BrowserPausedTaskStateOptions,
} from './capability'
import { discardPinnedResumeState } from './discard'
import {
  prepareResumeStateInventory,
  type BrowserResumeStatePin,
} from './inventory'
import {
  assertNamespaceMetadata,
  completedFiles,
  PausedTaskCapabilityError,
  PausedTaskDescriptorConflictError,
  storedPausedTaskDescriptor,
  storedRootCapability,
  validateStoredCapability,
  validateStoredDescriptor,
} from './records'

const MAXIMUM_PAUSED_TASK_DESCRIPTORS = 1_024

export class IndexedDbPausedTaskState
implements PausedTaskDescriptorRepository, ResumeStateAuthority {
  readonly #databaseName: string
  readonly #database: IDBDatabase
  readonly #dependencies: BrowserPausedTaskDependencies
  readonly #referenceOwners = new WeakMap<ResumeStateRef, ResumeStateReferenceOwner>()
  readonly #inventoryOwners = new Set<ResumeStateReferenceOwner>()
  #closed = false

  private constructor(
    databaseName: string,
    database: IDBDatabase,
    dependencies: BrowserPausedTaskDependencies,
  ) {
    this.#databaseName = databaseName
    this.#database = database
    this.#dependencies = dependencies
    database.addEventListener('versionchange', () => {
      this.#closed = true
      database.close()
    })
  }

  static async open(
    options: BrowserPausedTaskStateOptions = {},
  ): Promise<IndexedDbPausedTaskState> {
    const databaseName = options.databaseName ?? DEFAULT_OUTPUT_CHECKPOINT_DATABASE_NAME
    if (databaseName.length === 0) {
      throw new TypeError('paused-task database name must not be empty')
    }
    const database = await openIndexedDbCheckpointDatabase(databaseName)
    return new IndexedDbPausedTaskState(
      databaseName,
      database,
      browserPausedTaskDependencies(options),
    )
  }

  async persist(
    intent: TransferIntent,
    root: FileSystemDirectoryHandle,
  ): Promise<PausedTaskDescriptorV1> {
    this.#assertOpen()
    if (root.kind !== 'directory') {
      throw new TypeError('paused-task capability is not a directory handle')
    }
    const descriptor = await pausedTaskDescriptorV1({
      intent,
      rootCapabilityRef: createRootCapabilityRef(),
    })
    const namespace = pausedTaskDescriptorNamespace(descriptor)
    await verifyIndexedDbRootIdentity({
      databaseName: this.#databaseName,
      backend: namespace.backend,
      rootIdentity: namespace.rootIdentity,
      root,
    })

    const namespaceKey = durableCheckpointNamespaceKey(namespace)
    const transaction = this.#database.transaction([
      INDEXEDDB_CHECKPOINT_METADATA_STORE,
      INDEXEDDB_PAUSED_TASK_DESCRIPTOR_STORE,
      INDEXEDDB_ROOT_CAPABILITY_STORE,
    ], 'readwrite')
    const metadata = await requestResult<unknown>(
      transaction.objectStore(INDEXEDDB_CHECKPOINT_METADATA_STORE).get(namespaceKey),
    )
    assertNamespaceMetadata(metadata, namespaceKey, descriptor)
    const descriptors = transaction.objectStore(INDEXEDDB_PAUSED_TASK_DESCRIPTOR_STORE)
    const existing = await requestResult<unknown>(descriptors.get(namespaceKey))
    if (existing !== undefined) {
      await transactionCompletion(transaction)
      throw new PausedTaskDescriptorConflictError()
    }
    const capabilities = transaction.objectStore(INDEXEDDB_ROOT_CAPABILITY_STORE)
    const capabilityCollision = await requestResult<unknown>(
      capabilities.get(descriptor.rootCapabilityRef),
    )
    if (capabilityCollision !== undefined) {
      await transactionCompletion(transaction)
      throw new PausedTaskCapabilityError(
        'binding-mismatch',
        'A generated root capability reference already exists',
      )
    }
    descriptors.add(storedPausedTaskDescriptor(descriptor))
    capabilities.add(storedRootCapability(descriptor, root))
    await transactionCompletion(transaction)
    observePausedTask(
      this.#dependencies.onTrace,
      'paused-task-descriptor-persisted',
      descriptor,
      { decision: 'namespace-verified' },
    )
    return descriptor
  }

  async list(): Promise<readonly PausedTaskDescriptorV1[]> {
    this.#assertOpen()
    const transaction = this.#database.transaction(
      INDEXEDDB_PAUSED_TASK_DESCRIPTOR_STORE,
      'readonly',
    )
    const stored = await requestResult<unknown[]>(
      transaction.objectStore(INDEXEDDB_PAUSED_TASK_DESCRIPTOR_STORE)
        .getAll(undefined, MAXIMUM_PAUSED_TASK_DESCRIPTORS + 1),
    )
    await transactionCompletion(transaction)
    if (stored.length > MAXIMUM_PAUSED_TASK_DESCRIPTORS) {
      throw new DOMException(
        'Paused task inventory exceeds its browser bound',
        'QuotaExceededError',
      )
    }
    const descriptors: PausedTaskDescriptorV1[] = []
    for (const value of stored) descriptors.push(await validateStoredDescriptor(value))
    for (const descriptor of descriptors) {
      observePausedTask(
        this.#dependencies.onTrace,
        'paused-task-descriptors-listed',
        descriptor,
        { decision: 'reconstruction-metadata-only' },
      )
    }
    return Object.freeze(descriptors)
  }

  async listResumeState(): Promise<ResumeStateInventory> {
    this.#assertOpen()
    const owner: ResumeStateReferenceOwner = { open: true }
    const pins = await prepareResumeStateInventory(
      this.#databaseName,
      this.#database,
      await this.list(),
      this.#dependencies.onTrace,
    )
    const tasks = pins.map((pin) => this.#reference(owner, pin))
    this.#inventoryOwners.add(owner)
    return new ResumeStateInventory(owner, tasks)
  }

  resume(
    reference: ResumeStateRef,
    request: ResumeStateOperationRequest,
  ): Promise<ReconstructedPausedTask> {
    this.#assertOpen()
    assertPausedTaskCurrentShare(reference.descriptor, request.currentShare)
    const pin = this.#consume(reference)
    if (pin.discardMarker !== undefined) {
      throw new PausedTaskCapabilityError(
        'binding-mismatch',
        'The paused task has an unfinished discard and cannot be resumed',
      )
    }
    return resumePreparedPausedTask(
      this.#databaseName,
      pin.descriptor,
      pin.capability,
      request,
      this.#dependencies,
    )
  }

  discard(
    reference: ResumeStateRef,
    request: ResumeStateOperationRequest,
  ): Promise<ResumeStateDiscardResult> {
    this.#assertOpen()
    assertPausedTaskCurrentShare(reference.descriptor, request.currentShare)
    return discardPinnedResumeState(
      this.#databaseName,
      this.#consume(reference),
      request,
      this.#dependencies,
    )
  }

  async removeCompleted(requested: PausedTaskDescriptorV1): Promise<void> {
    this.#assertOpen()
    const descriptor = await validatePausedTaskDescriptorV1(requested)
    const key = pausedTaskDescriptorKey(descriptor)
    const inspection = this.#database.transaction([
      INDEXEDDB_PAUSED_TASK_DESCRIPTOR_STORE,
      INDEXEDDB_ROOT_CAPABILITY_STORE,
    ], 'readonly')
    const [storedDescriptor, storedCapability] = await Promise.all([
      requestResult<unknown>(
        inspection.objectStore(INDEXEDDB_PAUSED_TASK_DESCRIPTOR_STORE).get(key),
      ),
      requestResult<unknown>(
        inspection.objectStore(INDEXEDDB_ROOT_CAPABILITY_STORE)
          .get(descriptor.rootCapabilityRef),
      ),
    ])
    await transactionCompletion(inspection)
    if (storedDescriptor === undefined) return
    const current = await validateStoredDescriptor(storedDescriptor)
    if (!samePausedTaskDescriptor(current, descriptor)) {
      throw new PausedTaskCapabilityError(
        'binding-mismatch',
        'The completed paused-task descriptor no longer matches storage',
      )
    }
    validateStoredCapability(storedCapability, descriptor)

    // Both immutable authority rows retire together only after their untrusted
    // structured values have been validated against the canonical descriptor.
    const removal = this.#database.transaction([
      INDEXEDDB_PAUSED_TASK_DESCRIPTOR_STORE,
      INDEXEDDB_ROOT_CAPABILITY_STORE,
    ], 'readwrite')
    removal.objectStore(INDEXEDDB_PAUSED_TASK_DESCRIPTOR_STORE).delete(key)
    removal.objectStore(INDEXEDDB_ROOT_CAPABILITY_STORE)
      .delete(descriptor.rootCapabilityRef)
    await transactionCompletion(removal)
    observePausedTask(
      this.#dependencies.onTrace,
      'paused-task-descriptor-removed',
      descriptor,
      { decision: 'stable-completion' },
    )
  }

  close(): void {
    if (this.#closed) return
    this.#closed = true
    for (const owner of this.#inventoryOwners) owner.open = false
    this.#inventoryOwners.clear()
    this.#database.close()
  }

  #reference(
    owner: ResumeStateReferenceOwner,
    pin: BrowserResumeStatePin,
  ): ResumeStateRef {
    const reference = new ResumeStateRef(
      owner,
      pin.descriptor,
      completedFiles(pin.committed).length,
      pin,
    )
    this.#referenceOwners.set(reference, owner)
    return reference
  }

  #consume(reference: ResumeStateRef): BrowserResumeStatePin {
    const owner = this.#referenceOwners.get(reference)
    if (owner === undefined) {
      throw new DOMException(
        'Resume-state reference belongs to another authority',
        'InvalidStateError',
      )
    }
    this.#referenceOwners.delete(reference)
    return reference.consume(owner) as BrowserResumeStatePin
  }

  #assertOpen(): void {
    if (this.#closed) {
      throw new DOMException(
        'Paused-task repository is closed or version-obsolete',
        'InvalidStateError',
      )
    }
  }
}
