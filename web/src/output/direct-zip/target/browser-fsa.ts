import { directZipFsaOffset, directZipFsaOffsetBigInt } from '../format'
import { DIRECT_ZIP_BROWSER_TARGET_ENABLED_BY_DEFAULT } from './model'
import type {
  DirectZipExactNameLookup,
  DirectZipFileSnapshotPort,
  DirectZipFileSystemPort,
  DirectZipWritablePort,
} from './ports'

const READ_WRITE_PERMISSION = Object.freeze({ mode: 'readwrite' as const })

interface PermissionCapableDirectoryHandle extends FileSystemDirectoryHandle {
  queryPermission?(descriptor?: { readonly mode?: 'read' | 'readwrite' }): Promise<PermissionState>
  requestPermission?(descriptor?: { readonly mode?: 'read' | 'readwrite' }): Promise<PermissionState>
}

export interface DirectZipBrowserFileSystemOptions {
  /** Support policy must opt in after matching reviewed real-local evidence. */
  readonly enabled?: boolean
}

export function createDirectZipBrowserFileSystemPort(
  options: DirectZipBrowserFileSystemOptions = {},
): DirectZipFileSystemPort<FileSystemDirectoryHandle, FileSystemFileHandle> {
  if ((options.enabled ?? DIRECT_ZIP_BROWSER_TARGET_ENABLED_BY_DEFAULT) !== true) {
    throw new DOMException(
      'Direct resumable ZIP target support requires an injected reviewed support verdict',
      'NotSupportedError',
    )
  }
  const port: DirectZipFileSystemPort<FileSystemDirectoryHandle, FileSystemFileHandle> = {
    queryPermission: async (parent: FileSystemDirectoryHandle) => {
      const capable = parent as PermissionCapableDirectoryHandle
      return capable.queryPermission === undefined
        ? 'unsupported'
        : capable.queryPermission(READ_WRITE_PERMISSION)
    },
    requestPermission: async (parent: FileSystemDirectoryHandle) => {
      const capable = parent as PermissionCapableDirectoryHandle
      return capable.requestPermission === undefined
        ? 'unsupported'
        : capable.requestPermission(READ_WRITE_PERMISSION)
    },
    lookupExactName: lookupExactBrowserName,
    createFile: (parent: FileSystemDirectoryHandle, stableName: string) =>
      parent.getFileHandle(stableName, { create: true }),
    snapshot: async (file: FileSystemFileHandle) => browserFileSnapshot(await file.getFile()),
    createWritable: async (file: FileSystemFileHandle, keepExistingData: true | false) => {
      const writable = await file.createWritable({ keepExistingData })
      return browserWritable(writable)
    },
    removeExactName: (parent: FileSystemDirectoryHandle, stableName: string) =>
      parent.removeEntry(stableName),
  }
  return Object.freeze(port)
}

async function lookupExactBrowserName(
  parent: FileSystemDirectoryHandle,
  stableName: string,
): Promise<DirectZipExactNameLookup<FileSystemFileHandle>> {
  try {
    return Object.freeze({ kind: 'file', handle: await parent.getFileHandle(stableName) })
  } catch (error) {
    if (domErrorNamed(error, 'TypeMismatchError')) return Object.freeze({ kind: 'occupied-non-file' })
    if (!domErrorNamed(error, 'NotFoundError')) throw error
  }
  try {
    await parent.getDirectoryHandle(stableName)
    return Object.freeze({ kind: 'occupied-non-file' })
  } catch (error) {
    if (domErrorNamed(error, 'NotFoundError')) return Object.freeze({ kind: 'absent' })
    if (domErrorNamed(error, 'TypeMismatchError')) return Object.freeze({ kind: 'occupied-non-file' })
    throw error
  }
}

function browserFileSnapshot(file: File): DirectZipFileSnapshotPort {
  const size = directZipFsaOffsetBigInt(file.size, 'direct ZIP target length')
  const lastModified = file.lastModified
  return Object.freeze({
    size,
    lastModified,
    read: async (start: bigint, end: bigint) => {
      const numericStart = directZipFsaOffset(start, 'direct ZIP snapshot read start')
      const numericEnd = directZipFsaOffset(end, 'direct ZIP snapshot read end')
      if (numericEnd < numericStart || end > size) {
        throw new RangeError('direct ZIP snapshot read escaped the observed target')
      }
      return new Uint8Array(await file.slice(numericStart, numericEnd).arrayBuffer())
    },
  })
}

function browserWritable(writable: FileSystemWritableFileStream): DirectZipWritablePort {
  return Object.freeze({
    write: async (position: bigint, bytes: Uint8Array) => {
      const numericPosition = directZipFsaOffset(position, 'direct ZIP write position')
      await writable.write({
        type: 'write',
        position: numericPosition,
        data: Uint8Array.from(bytes).buffer,
      })
    },
    truncate: (size: bigint) => writable.truncate(
      directZipFsaOffset(size, 'direct ZIP truncate length'),
    ),
    close: () => writable.close(),
    abort: (reason?: unknown) => writable.abort(reason),
  })
}

function domErrorNamed(error: unknown, name: string): boolean {
  return error instanceof DOMException && error.name === name
}
