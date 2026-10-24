import type { DirectZipRequiredFeatureFactsV1 } from '../session'
import type { BrowserReceiveWindow } from '../../browser/environment'

export function observeBrowserDirectZipFeatureFacts(
  windowPort: BrowserReceiveWindow,
): DirectZipRequiredFeatureFactsV1 {
  const constructors = windowPort as BrowserReceiveWindow & Readonly<{
    FileSystemHandle?: { readonly prototype?: Record<string, unknown> }
    FileSystemFileHandle?: { readonly prototype?: Record<string, unknown> }
    FileSystemDirectoryHandle?: { readonly prototype?: Record<string, unknown> }
  }>
  const base = constructors.FileSystemHandle?.prototype
  const file = constructors.FileSystemFileHandle?.prototype
  const directory = constructors.FileSystemDirectoryHandle?.prototype
  return Object.freeze({
    createWritable: fact(file?.createWritable),
    handleIsSameEntry: fact(base?.isSameEntry ?? directory?.isSameEntry ?? file?.isSameEntry),
    handleQueryPermission: fact(base?.queryPermission ?? directory?.queryPermission),
    handleRequestPermission: fact(base?.requestPermission ?? directory?.requestPermission),
    indexedDB: typeof windowPort.indexedDB === 'object' ? 'object' : 'missing',
    isSecureContext: windowPort.isSecureContext === true,
    locks: typeof windowPort.navigator.locks === 'object' ? 'object' : 'missing',
    showDirectoryPicker: fact(windowPort.showDirectoryPicker),
  })
}

function fact(value: unknown): 'function' | 'missing' {
  return typeof value === 'function' ? 'function' : 'missing'
}
