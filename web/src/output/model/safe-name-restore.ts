export type RestorePlatform = 'windows' | 'posix'

export type RestoreWorkingDirectory = 'inside-result' | 'parent-folder'

export type SafeNameUnsupportedReason = 'invalid-for-os' | 'case-collision'

export interface SafeNameRestoreStep {
  readonly kind: 'file' | 'directory'
  readonly parentPhysicalPath: readonly string[]
  readonly parentLogicalPath: readonly string[]
  readonly fromName: string
  readonly toName: string
  readonly logicalPath: readonly string[]
}

export interface SafeNameUnsupportedEntry {
  readonly kind: 'file' | 'directory'
  readonly logicalPath: readonly string[]
  readonly physicalPath: readonly string[]
  readonly fromName: string
  readonly toName: string
  readonly reason: SafeNameUnsupportedReason
}

export interface SafeNameRestorePlan {
  readonly platform: RestorePlatform
  readonly workingDirectory: RestoreWorkingDirectory
  readonly targetFolderLabel: string
  readonly restorable: readonly SafeNameRestoreStep[]
  readonly unsupported: readonly SafeNameUnsupportedEntry[]
  readonly totalCount: number
}

export interface SafeNameRestorePrepared {
  readonly plan: SafeNameRestorePlan
  readonly inlineCommand: string | null
}

export interface SafeNameRestoreArtifact {
  readonly suggestedName: string
  readonly fallbackName: string | null
  readonly mediaType: string
  readonly content: string
  readonly runCommand: (fileName: string) => string
}

export interface SafeNameRestoreExportResult {
  readonly fileName: string
  readonly runCommand: string
  readonly directoryLabel: string
}

/** Raised when the saved destination folder can no longer be found for restoration export. */
export class FSARestoreTargetMissingError extends Error {
  readonly directoryLabel: string

  constructor(directoryLabel: string, options?: ErrorOptions) {
    super(`The saved folder "${directoryLabel}" could not be found.`, options)
    this.name = 'FSARestoreTargetMissingError'
    this.directoryLabel = directoryLabel
  }
}
