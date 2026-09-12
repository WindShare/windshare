import type {
  FSAFileMutationIdentity,
  FSAParentMutationIdentity,
  FSAVerifiedFileMutationTarget,
  FSAWriterLifecycleLease,
} from '../../src/output/browser/mutation-coordination/model'
import { createFSAOperationMutationScheduler } from '../../src/output/browser/mutation-coordination/scheduler'

const NATIVE_SCHEDULER_WRITER_LIMIT = 2

export interface NativeMutationSchedulingProof {
  readonly siblingInspection: string
  readonly siblingCreatedBeforeWriterClose: boolean
  readonly siblingWriterCompletedBeforeWriterClose: boolean
  readonly removalStartedBeforeWriterClose: boolean
  readonly laterWriterAdmittedDuringRemoval: boolean
  readonly eventOrder: readonly string[]
  readonly siblingFileBytes: readonly number[]
  readonly independentParentFileBytes: readonly number[]
  readonly peakActiveWriters: number
}

export async function probeNativeMutationScheduling(
  root: FileSystemDirectoryHandle,
): Promise<NativeMutationSchedulingProof> {
  const parent = await root.getDirectoryHandle('active-parent', { create: true })
  const independentParent = await root.getDirectoryHandle('independent-parent', { create: true })
  const identity = parentIdentity('active-parent')
  const independentIdentity = parentIdentity('independent-parent')
  const scheduler = createFSAOperationMutationScheduler({
    rootParent: parentIdentity('root'),
    maximumActiveWriters: NATIVE_SCHEDULER_WRITER_LIMIT,
  })
  const activeHandle = await parent.getFileHandle('active.bin', { create: true })
  const activeTarget = fileTarget(identity, 'active.bin')
  const activeLease = await scheduler.acquireWriter(activeTarget)
  const activeWriter = await activeHandle.createWritable()
  const eventOrder: string[] = []
  const removalStarted = deferred<void>()
  const releaseRemoval = deferred<void>()
  let removalEntered = false
  let laterWriterAdmitted = false
  let laterLease: FSAWriterLifecycleLease | undefined
  let laterWriterPromise: Promise<FSAWriterLifecycleLease> | undefined
  let removal: Promise<void> | undefined

  try {
    await activeWriter.write(Uint8Array.of(1, 2, 3))
    removal = scheduler.runFileMutation(activeTarget, 'remove-file', async () => {
      removalEntered = true
      eventOrder.push('same-file-removal')
      await parent.removeEntry('active.bin')
      removalStarted.resolve()
      await releaseRemoval.promise
    })
    laterWriterPromise = scheduler.acquireWriter(activeTarget).then((lease) => {
      laterLease = lease
      laterWriterAdmitted = true
      eventOrder.push('later-same-file-writer')
      return lease
    })

    // A waiting destructive operation must retain only the file it can destroy;
    // native sibling creation and publication remain possible while that file is open.
    const siblingInspection = await scheduler.runNamespace(
      [identity], 'inspect-entry', () => rejectionName(parent.getFileHandle('sibling.bin')),
    )
    const sibling = await scheduler.runNamespace([identity], 'create-file', async () => {
      eventOrder.push('sibling-created')
      return parent.getFileHandle('sibling.bin', { create: true })
    })
    const siblingCreatedBeforeWriterClose = scheduler.diagnostics().activeWriters === 1
    const siblingLease = await scheduler.acquireWriter(fileTarget(identity, 'sibling.bin'))
    const siblingWriter = await sibling.createWritable()
    try {
      await siblingWriter.write(Uint8Array.of(4, 5))
      await siblingWriter.close()
    } finally {
      await siblingWriter.abort().catch(() => undefined)
      siblingLease.release()
    }
    const siblingWriterCompletedBeforeWriterClose = scheduler.diagnostics().activeWriters === 1
    const independentResult = await scheduler.runNamespace(
      [independentIdentity],
      'create-file',
      async () => {
        eventOrder.push('independent-parent-created')
        const created = await independentParent.getFileHandle('independent.bin', { create: true })
        const writer = await created.createWritable()
        try {
          await writer.write(Uint8Array.of(8, 9))
          await writer.close()
        } finally {
          await writer.abort().catch(() => undefined)
        }
        return created
      },
    )
    const removalStartedBeforeWriterClose = removalEntered
    await activeWriter.close()
    activeLease.release()
    await Promise.race([removalStarted.promise, removal])
    const laterWriterAdmittedDuringRemoval = laterWriterAdmitted
    releaseRemoval.resolve()
    await removal
    laterLease = await laterWriterPromise
    laterLease.release()
    laterLease = undefined

    const siblingFile = await sibling.getFile()
    const independentParentFile = await independentResult.getFile()
    const diagnostics = scheduler.diagnostics()
    await scheduler.close()
    return Object.freeze({
      siblingInspection,
      siblingCreatedBeforeWriterClose,
      siblingWriterCompletedBeforeWriterClose,
      removalStartedBeforeWriterClose,
      laterWriterAdmittedDuringRemoval,
      eventOrder: Object.freeze(eventOrder),
      siblingFileBytes: Object.freeze([...new Uint8Array(await siblingFile.arrayBuffer())]),
      independentParentFileBytes: Object.freeze([
        ...new Uint8Array(await independentParentFile.arrayBuffer()),
      ]),
      peakActiveWriters: diagnostics.peakActiveWriters,
    })
  } finally {
    releaseRemoval.resolve()
    await activeWriter.abort().catch(() => undefined)
    activeLease.release()
    await removal?.catch(() => undefined)
    await laterWriterPromise?.catch(() => undefined)
    laterLease?.release()
    await scheduler.close().catch(() => undefined)
  }
}

export function fileTarget(
  parent: FSAParentMutationIdentity,
  description: string,
): FSAVerifiedFileMutationTarget {
  return Object.freeze({ parent, file: Symbol(description) as FSAFileMutationIdentity })
}

export function parentIdentity(description: string): FSAParentMutationIdentity {
  return Symbol(description) as FSAParentMutationIdentity
}

async function rejectionName(promise: Promise<unknown>): Promise<string> {
  return promise.then(
    () => 'resolved',
    error => error instanceof Error ? error.name : 'Error',
  )
}

function deferred<T>() {
  let resolve!: (value: T | PromiseLike<T>) => void
  const promise = new Promise<T>(complete => { resolve = complete })
  return Object.freeze({ promise, resolve })
}
