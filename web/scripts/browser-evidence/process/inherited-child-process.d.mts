import type { ChildProcess, SpawnOptions } from 'node:child_process'

import type {
  OwnedByteChannel,
  OwnedByteSnapshot,
  OwnedEventChannel,
  OwnedEventSnapshot,
} from './owned-process-channel.mjs'
import type { TestEvent } from './test-event-channel.mjs'
import type { TestIdentity } from './test-identity.mjs'

export type InheritedChildTerminal =
  | { readonly terminal: 'exited'; readonly exitCode: number }
  | { readonly terminal: 'signaled'; readonly signal: string }
  | {
      readonly terminal: 'spawn-failed'
      readonly errorCode: string
      readonly errorMessage: string
    }

export interface InheritedChildEventPolicy {
  readonly minimumEvents: number
  readonly maximumEvents: number
}

export interface InheritedChildLaunchRequest {
  readonly identity: TestIdentity
  readonly command: {
    readonly executable: string
    readonly arguments: readonly string[]
    readonly cwd: string
  }
  readonly environment: Readonly<Record<string, string>>
  readonly capture: Readonly<{
    readonly stdoutBytes: number
    readonly stderrBytes: number
  }>
  readonly events?: InheritedChildEventPolicy
}

export interface InheritedChildExecution {
  readonly terminal: InheritedChildTerminal
  readonly output: Readonly<{
    readonly stdout: OwnedByteSnapshot
    readonly stderr: OwnedByteSnapshot
  }>
  readonly events: OwnedEventSnapshot<TestEvent>
}

export class InheritedChildProcessError extends Error {
  readonly terminal: InheritedChildTerminal | undefined
  readonly output: InheritedChildExecution['output']
  readonly events: InheritedChildExecution['events']
  constructor(
    message: string,
    terminal: InheritedChildTerminal | undefined,
    output: InheritedChildExecution['output'],
    events: InheritedChildExecution['events'],
    cause: unknown,
  )
}

export interface InheritedChildSession {
  readonly kind: 'inherited-descendant'
  readonly platform: 'linux' | 'win32'
  readonly terminal: Promise<InheritedChildTerminal>
  readonly stdout: OwnedByteChannel
  readonly stderr: OwnedByteChannel
  readonly events: OwnedEventChannel<TestEvent>
  readonly completion: Promise<InheritedChildExecution>
  requestStop(): void
}

export interface InheritedChildProcessBackend {
  readonly kind: 'inherited-descendant'
  launch(request: InheritedChildLaunchRequest): InheritedChildSession
}

export type ChildProcessSpawner = (
  executable: string,
  arguments_: readonly string[],
  options: SpawnOptions,
) => ChildProcess

export function createInheritedChildProcessBackend(options?: {
  readonly platform?: NodeJS.Platform
  readonly spawnProcess?: ChildProcessSpawner
}): InheritedChildProcessBackend

export function inheritedChildEnvironment(
  environment: Readonly<Record<string, string>>,
  identity: TestIdentity,
  eventsEnabled: boolean,
): Readonly<Record<string, string>>
