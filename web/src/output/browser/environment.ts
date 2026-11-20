import type {
  BrowserCapabilityRuntime,
} from '../capability/contract'

export interface OriginPrivateStorageManager extends StorageManager {
  getDirectory(): Promise<FileSystemDirectoryHandle>
}

interface BrowserReceiveNavigator extends Navigator {
  readonly storage: OriginPrivateStorageManager
  readonly locks: LockManager
}

export type BrowserReceiveWindow = Window & Readonly<{
  readonly navigator: BrowserReceiveNavigator
  readonly indexedDB: IDBFactory
  readonly URL: typeof URL
  readonly Blob: typeof Blob
  readonly File: typeof File
  readonly WritableStream: typeof WritableStream
  readonly showDirectoryPicker?: BrowserCapabilityRuntime['showDirectoryPicker']
}>

export function isBrowserStoragePolicyDenial(error: unknown): error is DOMException {
  return error instanceof DOMException && error.name === 'SecurityError'
}

export function readBrowserIndexedDb(
  windowPort: Readonly<{ indexedDB?: IDBFactory }>,
): IDBFactory | undefined {
  return readPolicyGatedBrowserApi(() => windowPort.indexedDB)
}

export function readBrowserLocks(
  windowPort: Readonly<{ navigator?: Readonly<{ locks?: LockManager }> }>,
): LockManager | undefined {
  return readPolicyGatedBrowserApi(() => windowPort.navigator?.locks)
}

export function readBrowserStorage(
  windowPort: Readonly<{ navigator?: Readonly<{ storage?: OriginPrivateStorageManager }> }>,
): OriginPrivateStorageManager | undefined {
  return readPolicyGatedBrowserApi(() => windowPort.navigator?.storage)
}

function readPolicyGatedBrowserApi<T>(read: () => T | null | undefined): T | undefined {
  try {
    return read() ?? undefined
  } catch (error) {
    // Sandboxed documents and blocked-cookie contexts can throw SecurityError
    // synchronously from storage getters. Non-policy failures still propagate.
    if (isBrowserStoragePolicyDenial(error)) return undefined
    throw error
  }
}
