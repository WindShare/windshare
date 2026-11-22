import type { NativeReply, NativeRequest, NativeSyncHandle } from './contracts'
import { truncateNativeObject, writeNativeBytes } from './sync-operations'

// This module is loaded only by a Dedicated Worker: exclusive ownership avoids
// accidental independent flush/close cuts from concurrent receiving connections.
const scope = globalThis as unknown as {
  onmessage: ((event: MessageEvent<NativeRequest>) => void) | null
  postMessage(reply: NativeReply): void
}
let handle: NativeSyncHandle | undefined
let tail: Promise<void> = Promise.resolve()
scope.onmessage = event => {
  tail = tail.then(async () => {
    const request = event.data
    try {
      let size: bigint | undefined
      if (request.kind === 'open') {
        if (handle !== undefined) throw new Error('Native object is already open')
        const file = request.handle as FileSystemFileHandle & {
          createSyncAccessHandle(): Promise<NativeSyncHandle>
        }
        handle = await file.createSyncAccessHandle()
      } else {
        if (handle === undefined) throw new Error('Native object is closed')
        switch (request.kind) {
          case 'write': writeNativeBytes(handle, request.offset, request.bytes); break
          case 'truncate': truncateNativeObject(handle, request.length); break
          case 'size': size = BigInt(handle.getSize()); break
          case 'flush': handle.flush(); break
          case 'close': handle.close(); handle = undefined; break
        }
      }
      scope.postMessage({ id: request.id, ok: true, ...(size === undefined ? {} : { size }) })
    } catch (error) {
      try { handle?.close() } catch { /* Preserve the primary failure. */ }
      handle = undefined
      scope.postMessage({
        id: request.id, ok: false,
        ...errorDetails(error),
      })
    }
  })
}

function errorDetails(error: unknown): { name: string; message: string } {
  return error instanceof Error
    ? { name: error.name, message: error.message }
    : { name: 'Error', message: String(error) }
}
