declare const directoryIdBrand: unique symbol
declare const fileIdBrand: unique symbol
declare const scanAttemptIdBrand: unique symbol

export type DirectoryId = string & { readonly [directoryIdBrand]: 'DirectoryId' }
export type FileId = string & { readonly [fileIdBrand]: 'FileId' }
export type ScanAttemptId = string & { readonly [scanAttemptIdBrand]: 'ScanAttemptId' }
export type CatalogNodeId = DirectoryId | FileId

export interface CatalogNamePolicy {
  validate(name: string): string
  collisionKey(name: string): string
}

export interface CatalogModifiedTime {
  readonly milliseconds: bigint
  readonly precisionMilliseconds: bigint
}

interface CatalogChildBase {
  readonly name: string
  readonly modifiedTime?: CatalogModifiedTime
}

export interface CatalogDirectoryChild extends CatalogChildBase {
  readonly kind: 'directory'
  readonly id: DirectoryId
}

export interface CatalogFileChild extends CatalogChildBase {
  readonly kind: 'file'
  readonly id: FileId
  readonly expectedSize: bigint
}

export type CatalogChild = CatalogDirectoryChild | CatalogFileChild

interface CatalogNodeBase {
  readonly name: string
  readonly parentId: DirectoryId | undefined
  readonly modifiedTime?: CatalogModifiedTime
}

export interface CatalogDirectoryNode extends CatalogNodeBase {
  readonly kind: 'directory'
  readonly id: DirectoryId
}

export interface CatalogFileNode extends CatalogNodeBase {
  readonly kind: 'file'
  readonly id: FileId
  readonly expectedSize: bigint
}

export type CatalogNode = CatalogDirectoryNode | CatalogFileNode

export interface CatalogDirectoryGeneration {
  readonly directoryId: DirectoryId
  /** Opaque semantic identity; its wire representation belongs to the protocol codec. */
  readonly generation: string
  readonly children: readonly CatalogChild[]
}

export type CatalogDirectoryFailureKind =
  | 'stale'
  | 'permanent'
  | 'retryable'
  | 'resource-limit'

export interface CatalogDirectoryFailure {
  readonly attemptId: ScanAttemptId
  readonly kind: CatalogDirectoryFailureKind
  readonly message: string
  readonly retryAfterMilliseconds?: number
}

export type CatalogDirectoryState =
  | { readonly status: 'undiscovered' }
  | { readonly status: 'loading' }
  | { readonly status: 'ready'; readonly generation: CatalogDirectoryGeneration }
  | { readonly status: 'failed'; readonly failure: CatalogDirectoryFailure }

export interface CatalogVisibleRow {
  readonly node: CatalogNode
  readonly depth: number
  readonly expanded: boolean
  readonly directoryState?: CatalogDirectoryState['status']
}

export interface CatalogVisibleWindow {
  readonly rows: readonly CatalogVisibleRow[]
  readonly truncated: boolean
}

/**
 * IDs are opaque domain tokens here. Protocol decoders remain responsible for
 * enforcing the eventual byte representation before constructing these values.
 */
export function directoryId(value: string): DirectoryId {
  return opaqueId(value, 'directory') as DirectoryId
}

export function fileId(value: string): FileId {
  return opaqueId(value, 'file') as FileId
}

export function scanAttemptId(value: string): ScanAttemptId {
  return opaqueId(value, 'scan attempt') as ScanAttemptId
}

function opaqueId(value: string, kind: string): string {
  if (value.length === 0) {
    throw new TypeError(`${kind} ID must not be empty`)
  }
  return value
}

/** Structural checks only; the versioned cross-platform path policy is injected. */
export const structuralCatalogNamePolicy: CatalogNamePolicy = Object.freeze({
  validate(name: string): string {
    if (
      name.length === 0 ||
      name === '.' ||
      name === '..' ||
      name.includes('/') ||
      name.includes('\\') ||
      name.includes('\0')
    ) {
      throw new TypeError('catalog child name is not a single safe path segment')
    }
    return name
  },
  collisionKey: (name: string): string => name,
})
