export interface ZipArchiveEntry {
  readonly path: readonly string[]
  readonly modifiedTimeMilliseconds?: bigint
}

export interface ZipArchiveFileEntry extends ZipArchiveEntry {
  readonly exactSize: bigint
}

export interface ZipArchiveMember {
  write(data: Uint8Array): Promise<void>
  close(): Promise<void>
  abort(reason: unknown): Promise<void>
}

export interface ZipArchiveWriter {
  readonly cleanupPending: boolean
  readonly cleanupFailure: unknown
  addDirectory(entry: ZipArchiveEntry): Promise<void>
  beginFile(entry: ZipArchiveFileEntry): Promise<ZipArchiveMember>
  close(signal: AbortSignal): Promise<void>
  abort(reason: unknown): Promise<void>
  retryCleanup(): Promise<void>
}
