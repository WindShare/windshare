import { isPortableCatalogName } from '../../catalog/path-policy'
import type { PersistentOutputStage } from '../persistent-tree/stage-diagnostics'

export type FileSystemComponentKind = 'file' | 'directory'

export type FileSystemComponentInspectionStage = Extract<
  PersistentOutputStage,
  | 'fsa.root.entry.inspect'
  | 'fsa.directory.entry.inspect'
  | 'fsa.file.entry.inspect'
>

export type FileSystemComponentInspectionMode = 'classify-rejection' | 'diagnostic'

export interface FileSystemComponentInspectionInput {
  /** The caller must establish parent authority before crossing this namespace boundary. */
  readonly verifiedParent: FileSystemDirectoryHandle
  readonly component: string
  readonly expectedKind: FileSystemComponentKind
  readonly stage: FileSystemComponentInspectionStage
  readonly mode: FileSystemComponentInspectionMode
}

export class PathComponentRejectedError extends Error {
  readonly canonicalComponent: string
  readonly expectedKind: FileSystemComponentKind
  readonly stage: FileSystemComponentInspectionStage
  readonly preMutation = true as const

  constructor(input: Readonly<{
    readonly cause: TypeError
    readonly canonicalComponent: string
    readonly expectedKind: FileSystemComponentKind
    readonly stage: FileSystemComponentInspectionStage
  }>) {
    super('The browser rejected a non-creating lookup for a canonical path component', {
      cause: input.cause,
    })
    this.name = 'PathComponentRejectedError'
    this.canonicalComponent = input.canonicalComponent
    this.expectedKind = input.expectedKind
    this.stage = input.stage
  }
}

/**
 * Inspects one verified FSA namespace without creating an entry. The expected-kind-first order is
 * part of the evidence: only that first native call can prove a browser component refusal happened
 * before WindShare mutated the destination.
 */
export async function inspectFileSystemComponent(
  input: FileSystemComponentInspectionInput,
): Promise<'absent' | 'occupied'> {
  const component = requireCanonicalComponent(input.component)
  const expectedLookup = nativeLookup(input.verifiedParent, input.expectedKind)

  try {
    await expectedLookup(component)
    return 'occupied'
  } catch (error) {
    if (error instanceof TypeError) {
      if (input.mode === 'diagnostic') throw error
      throw new PathComponentRejectedError({
        cause: error,
        canonicalComponent: component,
        expectedKind: input.expectedKind,
        stage: input.stage,
      })
    }
    if (errorNamed(error, 'TypeMismatchError')) return 'occupied'
    if (!errorNamed(error, 'NotFoundError')) throw error
  }

  const oppositeLookup = nativeLookup(input.verifiedParent, oppositeKind(input.expectedKind))
  try {
    await oppositeLookup(component)
    return 'occupied'
  } catch (error) {
    if (errorNamed(error, 'NotFoundError')) return 'absent'
    if (errorNamed(error, 'TypeMismatchError')) return 'occupied'
    throw error
  }
}

function requireCanonicalComponent(component: string): string {
  if (typeof component !== 'string' || !isPortableCatalogName(component)) {
    throw new TypeError('FSA inspection requires one canonical portable path component')
  }
  return component
}

function nativeLookup(
  parent: FileSystemDirectoryHandle,
  kind: FileSystemComponentKind,
): (component: string) => Promise<FileSystemHandle> {
  // Resolve and bind the method outside the classifier catch so an invalid parent cannot masquerade
  // as a browser refusal from the awaited native lookup.
  return kind === 'file'
    ? parent.getFileHandle.bind(parent)
    : parent.getDirectoryHandle.bind(parent)
}

function oppositeKind(kind: FileSystemComponentKind): FileSystemComponentKind {
  return kind === 'file' ? 'directory' : 'file'
}

function errorNamed(error: unknown, name: string): boolean {
  return typeof error === 'object' && error !== null &&
    'name' in error && (error as { readonly name?: unknown }).name === name
}
