import {
  persistentOutputObservedFact,
  type PersistentOutputFailureFactContext,
  type PersistentOutputFailureFacts,
  type PersistentOutputFSAFacts,
  type PersistentOutputObservedFact,
  type PersistentOutputWriterFacts,
} from '../persistent-tree/stage-diagnostics'

export type FSAFailureTarget =
  | Readonly<{
      readonly kind: 'named-entry'
      readonly resolve: () => Promise<Readonly<{
        readonly parent: FileSystemDirectoryHandle
        readonly name: string
      }>>
    }>
  | Readonly<{
      readonly kind: 'handle'
      readonly resolve: () => Promise<FileSystemHandle>
    }>

export interface CaptureFSAFailureFactsInput {
  readonly target: FSAFailureTarget
  readonly permissionFallback: FileSystemHandle
  readonly expectedKind: 'file' | 'directory'
  readonly readPersistedHandle: () => Promise<unknown | undefined>
  readonly writer?: (context: PersistentOutputFailureFactContext) =>
    PersistentOutputWriterFacts | undefined
}

type FSAEntryProbe =
  | Readonly<{ readonly kind: 'absent' }>
  | Readonly<{ readonly kind: 'other' }>
  | Readonly<{ readonly kind: 'file'; readonly handle: FileSystemFileHandle }>
  | Readonly<{ readonly kind: 'directory'; readonly handle: FileSystemDirectoryHandle }>

interface PermissionCapableFileSystemHandle extends FileSystemHandle {
  queryPermission?(descriptor?: { readonly mode?: 'read' | 'readwrite' }): Promise<PermissionState>
}

export async function captureFSAFailureFacts(
  input: CaptureFSAFailureFactsInput,
  context: PersistentOutputFailureFactContext,
): Promise<Omit<PersistentOutputFailureFacts, 'observation'>> {
  const entryProbe = await persistentOutputObservedFact(
    () => probeTarget(input.target),
    context,
  )
  const entry = mapObservedFact(entryProbe, value => value.kind)
  const permissionTarget = observedHandle(entryProbe) ?? input.permissionFallback
  const [read, readwrite, persistedHandle, committedBytes] = await Promise.all([
    queryPermissionFact(permissionTarget, 'read', context),
    queryPermissionFact(permissionTarget, 'readwrite', context),
    persistedHandleFact(input, entryProbe, context),
    input.expectedKind === 'file'
      ? committedBytesFact(entryProbe, context)
      : Promise.resolve(undefined),
  ])
  const writer = input.writer?.(context)
  const fsa: PersistentOutputFSAFacts = Object.freeze({
    entry,
    ...(committedBytes === undefined ? {} : { committedBytes }),
    permissions: Object.freeze({
      target: observedHandle(entryProbe) === undefined ? 'parent' : 'entry',
      read,
      readwrite,
    }),
    persistedHandle,
    ...(writer === undefined ? {} : { writer }),
  })
  return Object.freeze({ fsa })
}

export function fileSystemHandle(value: unknown): FileSystemHandle {
  if (typeof value !== 'object' || value === null ||
      !('kind' in value) || !('isSameEntry' in value) ||
      ((value as { readonly kind?: unknown }).kind !== 'file' &&
       (value as { readonly kind?: unknown }).kind !== 'directory') ||
      typeof (value as { readonly isSameEntry?: unknown }).isSameEntry !== 'function') {
    throw new TypeError('Persisted FSA handle is invalid')
  }
  return value as FileSystemHandle
}

export function errorNamed(error: unknown, name: string): boolean {
  return typeof error === 'object' && error !== null &&
    'name' in error && (error as { readonly name?: unknown }).name === name
}

async function probeTarget(target: FSAFailureTarget): Promise<FSAEntryProbe> {
  if (target.kind === 'handle') {
    const handle = fileSystemHandle(await target.resolve())
    return handle.kind === 'file'
      ? Object.freeze({ kind: 'file' as const, handle: handle as FileSystemFileHandle })
      : Object.freeze({
          kind: 'directory' as const,
          handle: handle as FileSystemDirectoryHandle,
        })
  }
  const { parent, name } = await target.resolve()
  try {
    return Object.freeze({
      kind: 'file' as const,
      handle: await parent.getFileHandle(name),
    })
  } catch (error) {
    if (errorNamed(error, 'TypeMismatchError')) {
      return directoryProbe(parent, name)
    }
    if (!errorNamed(error, 'NotFoundError')) throw error
  }
  return directoryProbe(parent, name)
}

async function directoryProbe(
  parent: FileSystemDirectoryHandle,
  name: string,
): Promise<FSAEntryProbe> {
  try {
    return Object.freeze({
      kind: 'directory' as const,
      handle: await parent.getDirectoryHandle(name),
    })
  } catch (error) {
    if (errorNamed(error, 'TypeMismatchError')) return Object.freeze({ kind: 'other' as const })
    if (!errorNamed(error, 'NotFoundError')) throw error
    return Object.freeze({ kind: 'absent' as const })
  }
}

function observedHandle(
  entry: PersistentOutputObservedFact<FSAEntryProbe>,
): FileSystemHandle | undefined {
  if (entry.status !== 'observed') return undefined
  return entry.value.kind === 'file' || entry.value.kind === 'directory'
    ? entry.value.handle
    : undefined
}

async function persistedHandleFact(
  input: CaptureFSAFailureFactsInput,
  entry: PersistentOutputObservedFact<FSAEntryProbe>,
  context: PersistentOutputFailureFactContext,
): Promise<PersistentOutputFSAFacts['persistedHandle']> {
  return persistentOutputObservedFact(async () => {
    const raw = await input.readPersistedHandle()
    if (raw === undefined) return 'absent' as const
    let persisted: FileSystemHandle
    try {
      persisted = fileSystemHandle(raw)
    } catch {
      return 'invalid' as const
    }
    if (persisted.kind !== input.expectedKind) return 'invalid' as const
    if (entry.status === 'unavailable') throw entry.exception.raw
    const current = observedHandle(entry)
    if (current === undefined || current.kind !== input.expectedKind) {
      return 'mismatches-entry' as const
    }
    return await persisted.isSameEntry(current)
      ? 'matches-entry' as const
      : 'mismatches-entry' as const
  }, context)
}

async function committedBytesFact(
  entry: PersistentOutputObservedFact<FSAEntryProbe>,
  context: PersistentOutputFailureFactContext,
): Promise<NonNullable<PersistentOutputFSAFacts['committedBytes']>> {
  if (entry.status === 'unavailable') {
    return Object.freeze({ status: 'unavailable', exception: entry.exception })
  }
  if (entry.value.kind === 'absent') {
    return Object.freeze({ status: 'observed', value: 'absent' })
  }
  if (entry.value.kind !== 'file') {
    return Object.freeze({ status: 'observed', value: 'not-file' })
  }
  const handle = entry.value.handle
  return persistentOutputObservedFact(
    async () => BigInt((await handle.getFile()).size),
    context,
  )
}

function queryPermissionFact(
  target: FileSystemHandle,
  mode: 'read' | 'readwrite',
  context: PersistentOutputFailureFactContext,
): Promise<PersistentOutputObservedFact<PermissionState | 'unsupported'>> {
  return persistentOutputObservedFact(async () => {
    const capable = target as PermissionCapableFileSystemHandle
    if (capable.queryPermission === undefined) return 'unsupported' as const
    return capable.queryPermission({ mode })
  }, context)
}

function mapObservedFact<Input, Output>(
  fact: PersistentOutputObservedFact<Input>,
  map: (input: Input) => Output,
): PersistentOutputObservedFact<Output> {
  return fact.status === 'observed'
    ? Object.freeze({ status: 'observed', value: map(fact.value) })
    : Object.freeze({ status: 'unavailable', exception: fact.exception })
}
