import type { Readable } from 'node:stream'

import type { OwnedEventChannel } from './owned-process-channel.mjs'
import type { TestIdentity } from './test-identity.mjs'

export const TEST_EVENT_SCHEMA_VERSION: 'windshare.test-event/v1'

export interface TestEvent {
  readonly schemaVersion: typeof TEST_EVENT_SCHEMA_VERSION
  readonly runId: string
  readonly operationId: string
  readonly scenario: string
  readonly component: string
  readonly milestone: string
  readonly outcome: 'started' | 'succeeded' | 'failed'
  readonly payload?: unknown
}

export interface TestEventDecoder {
  readonly events: OwnedEventChannel<TestEvent>
  push(chunk: Uint8Array): void
  fail(cause: unknown): void
  finish(): number
}

export interface TestEventDecoderOptions {
  readonly identity: TestIdentity
  readonly minimumEvents?: number
  readonly maximumEvents?: number
}

export function createTestEventDecoder(options: TestEventDecoderOptions): TestEventDecoder
export function drainTestEventStream(
  stream: Readable,
  options: TestEventDecoderOptions,
): Readonly<{
  readonly events: OwnedEventChannel<TestEvent>
  readonly completion: Promise<number>
}>
export function parseTestEvent(value: unknown, identity: TestIdentity): TestEvent
