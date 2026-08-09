import type {
  BrowserCapabilityRuntime,
} from '../../output/capability/contract'

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
