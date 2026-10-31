import { isCatalogName } from '../../catalog/path-policy'
import type { PersistentOutputStage } from '../persistent-tree/stage-diagnostics'

export type FileSystemComponentKind = 'file' | 'directory'

export type FileSystemComponentInspectionStage = Extract<
  PersistentOutputStage,
  | 'fsa.root.entry.inspect'
  | 'fsa.directory.entry.inspect'
  | 'fsa.file.entry.inspect'
>

export type FileSystemComponentCreationStage = Extract<
  PersistentOutputStage,
  'fsa.directory.entry.create'
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

/**
 * Both forms of evidence share one property: the browser raised the refusal before WindShare
 * mutated the destination. Non-creating lookups prove it by construction. Chromium's write-path
 * policy (which blocks the `.git/hooks` directory) is evaluated before the creation touches the
 * filesystem, so its SecurityError is equally safe to route through compatible-name repair.
 */
export type PathComponentRejectionEvidence =
  | Readonly<{
      readonly kind: 'non-creating-lookup'
      readonly stage: FileSystemComponentInspectionStage
      readonly cause: TypeError
    }>
  | Readonly<{
      readonly kind: 'write-policy-refusal'
      readonly stage: FileSystemComponentCreationStage
      readonly cause: DOMException
    }>

export class PathComponentRejectedError extends Error {
  readonly canonicalComponent: string
  readonly expectedKind: FileSystemComponentKind
  readonly evidenceKind: PathComponentRejectionEvidence['kind']
  readonly stage: PathComponentRejectionEvidence['stage']
  readonly preMutation = true as const

  constructor(input: Readonly<{
    readonly evidence: PathComponentRejectionEvidence
    readonly canonicalComponent: string
    readonly expectedKind: FileSystemComponentKind
  }>) {
    super(rejectionMessage(input.evidence.kind), { cause: input.evidence.cause })
    this.name = 'PathComponentRejectedError'
    this.canonicalComponent = input.canonicalComponent
    this.expectedKind = input.expectedKind
    this.evidenceKind = input.evidence.kind
    this.stage = input.evidence.stage
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
        evidence: { kind: 'non-creating-lookup', stage: input.stage, cause: error },
        canonicalComponent: component,
        expectedKind: input.expectedKind,
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

/**
 * Chromium enforces its write-path policy only when a directory is created; programmatic file
 * creation and writes are unrestricted. A SecurityError from an admitted directory create is
 * therefore a name refusal, not a permission or ownership failure, and compatible names may absorb
 * it exactly as they absorb a refused lookup.
 */
export function classifyDirectoryCreationRefusal(
  cause: unknown,
  canonicalComponent: string,
): PathComponentRejectedError | undefined {
  const component = requireCanonicalComponent(canonicalComponent)
  if (!(cause instanceof DOMException) || cause.name !== 'SecurityError') return undefined
  return new PathComponentRejectedError({
    evidence: { kind: 'write-policy-refusal', stage: 'fsa.directory.entry.create', cause },
    canonicalComponent: component,
    expectedKind: 'directory',
  })
}

/** The lookup stage a descendant repair probes with, independent of how the refusal surfaced. */
export function descendantInspectionStage(
  kind: FileSystemComponentKind,
): Extract<FileSystemComponentInspectionStage, 'fsa.directory.entry.inspect' | 'fsa.file.entry.inspect'> {
  return kind === 'directory' ? 'fsa.directory.entry.inspect' : 'fsa.file.entry.inspect'
}

function rejectionMessage(kind: PathComponentRejectionEvidence['kind']): string {
  return kind === 'non-creating-lookup'
    ? 'The browser rejected a non-creating lookup for a canonical path component'
    : 'The browser refused to create a canonical path component under its write-path policy'
}

function requireCanonicalComponent(component: string): string {
  if (typeof component !== 'string' || !isCatalogName(component)) {
    throw new TypeError('FSA inspection requires one canonical catalog path component')
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
