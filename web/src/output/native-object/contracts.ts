/** The worker resolves a write only after every byte has reached the native handle. */
export interface NativeObjectIO {
  writeAt(offset: bigint, bytes: Uint8Array): Promise<void>
  truncate(length: bigint): Promise<void>
  size(): Promise<bigint>
  flush(): Promise<void>
  close(): Promise<void>
}

export type NativeObjectFactory = (handle: FileSystemFileHandle) => Promise<NativeObjectIO>

export interface NativeSyncHandle {
  write(bytes: Uint8Array, options: { at: number }): number
  truncate(length: number): void
  getSize(): number
  flush(): void
  close(): void
}

export type NativeRequest = Readonly<{ id: number } & (
  | { kind: 'open'; handle: FileSystemFileHandle }
  | { kind: 'write'; offset: bigint; bytes: Uint8Array }
  | { kind: 'truncate'; length: bigint }
  | { kind: 'size' | 'flush' | 'close' }
)>
export type NativeReply = Readonly<{ id: number } & (
  | { ok: true; size?: bigint }
  | { ok: false; name: string; message: string }
)>
