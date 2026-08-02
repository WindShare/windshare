import type { Writable } from 'node:stream'

export interface OwnedProcessCaptureLimits {
  readonly stdoutBytes: number
  readonly stderrBytes: number
  /** Zero disables the private event endpoint. */
  readonly eventCount: number
}

export interface OwnedByteSnapshot {
  readonly observedBytes: number
  readonly capturedBytes: number
  readonly truncated: boolean
  readonly completed: boolean
  /** Returns a detached copy; mutations never affect retained process evidence. */
  bytes(): Uint8Array
}

export interface OwnedByteChannel extends AsyncIterable<Uint8Array> {
  snapshot(): OwnedByteSnapshot
}

export interface OwnedEventSnapshot<Event> {
  readonly events: readonly Event[]
  readonly observedEvents: number
  readonly capturedEvents: number
  readonly truncated: boolean
  readonly completed: boolean
}

export interface OwnedEventChannel<Event> extends AsyncIterable<Event> {
  snapshot(): OwnedEventSnapshot<Event>
}

export const OWNED_PROCESS_CAPTURE_LIMITS: Readonly<{
  defaultStdoutBytes: number
  defaultStderrBytes: number
  maximumStreamBytes: number
  maximumEvents: number
}>

export function normalizeOwnedProcessCapture(
  value?: Partial<OwnedProcessCaptureLimits>,
): OwnedProcessCaptureLimits

export function createOwnedByteChannel(maximumBytes: number, label: string): Readonly<{
  view: OwnedByteChannel
  append(chunk: Uint8Array): void
  fail(cause: unknown): void
  finish(): void
  failure(): Error | undefined
}>

export function createOwnedEventChannel<Event>(maximumEvents: number, label: string): Readonly<{
  view: OwnedEventChannel<Event>
  append(event: Event): void
  fail(cause: unknown): void
  finish(): void
  failure(): Error | undefined
}>

export function waitForExactWritableCompletion(stream: Writable, label: string): Promise<void>
