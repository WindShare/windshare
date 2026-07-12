import {
  FileSystemDownloadSink,
  createFileSystemDownloadSink,
  type FsaMetadataWriter,
} from './fsa'
import type { DownloadSinkContext } from './model'
import {
  SingleFileDownloadSink,
  createSingleFileDownloadSink,
} from './single-file'
import { DownloadError } from './errors'
import { ZipDownloadSink, createZipDownloadSink } from './zip'

export interface FileSystemDownloadTarget {
  readonly kind: 'file-system'
  readonly root: FileSystemDirectoryHandle
  readonly metadata?: FsaMetadataWriter
}

export interface SingleFileDownloadTarget {
  readonly kind: 'single-file-stream'
  readonly output: WritableStream<Uint8Array>
}

export interface ZipDownloadTarget {
  readonly kind: 'zip-stream'
  readonly output: WritableStream<Uint8Array>
}

export type DownloadTarget =
  | FileSystemDownloadTarget
  | SingleFileDownloadTarget
  | ZipDownloadTarget

/** Ordering is derived solely from the selected target capability. */
export function createDownloadSink(
  context: DownloadSinkContext,
  target: FileSystemDownloadTarget,
): FileSystemDownloadSink
export function createDownloadSink(
  context: DownloadSinkContext,
  target: SingleFileDownloadTarget,
): SingleFileDownloadSink
export function createDownloadSink(
  context: DownloadSinkContext,
  target: ZipDownloadTarget,
): ZipDownloadSink
export function createDownloadSink(
  context: DownloadSinkContext,
  target: DownloadTarget,
): FileSystemDownloadSink | SingleFileDownloadSink | ZipDownloadSink
export function createDownloadSink(
  context: DownloadSinkContext,
  target: DownloadTarget,
): FileSystemDownloadSink | SingleFileDownloadSink | ZipDownloadSink {
  switch (target.kind) {
    case 'file-system':
      return createFileSystemDownloadSink(context, target.root, target.metadata)
    case 'single-file-stream':
      return createSingleFileDownloadSink(context, target.output)
    case 'zip-stream':
      return createZipDownloadSink(context, target.output)
    default:
      throw new DownloadError(
        'unsupported-target',
        'The requested download target is not supported',
      )
  }
}
