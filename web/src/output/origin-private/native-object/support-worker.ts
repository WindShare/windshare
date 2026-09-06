// Probe in the same worker execution context that owns production sync handles.
const scope = globalThis as unknown as {
  FileSystemFileHandle?: { prototype: { createSyncAccessHandle?: unknown } }
  postMessage(supported: boolean): void
}
scope.postMessage(typeof scope.FileSystemFileHandle?.prototype.createSyncAccessHandle === 'function')

export {}
