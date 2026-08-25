declare const FSA_PARENT_MUTATION_IDENTITY: unique symbol
declare const FSA_TERMINAL_EXCLUSIVE_AUTHORITY: unique symbol

/**
 * Only a verified authority producer may create this token. The scheduler deliberately
 * knows nothing about paths or display names, so untrusted namespace text cannot alias lanes.
 */
export type FSAParentMutationIdentity = symbol & {
  readonly [FSA_PARENT_MUTATION_IDENTITY]: 'verified-fsa-parent'
}

export type FSANamespaceMutationKind =
  | 'reserve-name'
  | 'create-directory'
  | 'create-file'
  | 'remove-entry'
  | 'rename-entry'
  | 'repair-compatible-name'

export type FSATerminalMutationKind =
  | 'settle-operation'
  | 'pause-operation'
  | 'stop-operation'
  | 'discard-operation'
  | 'repair-operation'

export type FSAMutationSchedulerState = 'accepting' | 'draining' | 'closed'

export class FSAOperationMutationClosedError extends DOMException {
  constructor() {
    super('The File System Access operation mutation scheduler is not accepting work', 'InvalidStateError')
  }
}

export class FSATerminalMutationAlreadyBegunError extends DOMException {
  constructor() {
    super('The File System Access terminal mutation cut has already begun', 'InvalidStateError')
  }
}

export class FSATerminalMutationUnavailableError extends DOMException {
  constructor() {
    super('The File System Access terminal mutation authority is no longer available', 'InvalidStateError')
  }
}

export interface FSAWriterLifecycleLease {
  readonly parent: FSAParentMutationIdentity
  release(): void
}

/**
 * Terminal-only tree operations accept this capability instead of a normal parent token.
 * That makes recursive or operation-wide mutation impossible through ordinary lane APIs.
 */
export interface FSATerminalExclusiveAuthority {
  readonly kind: FSATerminalMutationKind
  readonly [FSA_TERMINAL_EXCLUSIVE_AUTHORITY]: true
}

export interface FSATerminalDrain {
  readonly kind: FSATerminalMutationKind
  readonly drained: Promise<void>
  runExclusive<T>(
    operation: (authority: FSATerminalExclusiveAuthority) => Promise<T>,
  ): Promise<T>
}

export interface FSAMutationSchedulerDiagnosticsSnapshot {
  readonly state: FSAMutationSchedulerState
  readonly maximumActiveWriters: number
  readonly activeWriters: number
  readonly queuedWriters: number
  readonly peakActiveWriters: number
  readonly acquiredWriterLeases: number
  readonly releasedWriterLeases: number
  readonly activeNamespaceMutations: number
  readonly queuedNamespaceMutations: number
  readonly peakActiveNamespaceMutations: number
  readonly startedNamespaceMutations: number
  readonly completedNamespaceMutations: number
  readonly failedNamespaceMutations: number
  readonly terminalExclusiveRuns: number
  readonly failedTerminalExclusiveRuns: number
}

export interface FSAOperationMutationScheduler {
  runRootNamespace<T>(
    kind: FSANamespaceMutationKind,
    operation: () => Promise<T>,
  ): Promise<T>
  runNamespace<T>(
    parents: readonly FSAParentMutationIdentity[],
    kind: FSANamespaceMutationKind,
    operation: () => Promise<T>,
  ): Promise<T>
  acquireWriter(parent: FSAParentMutationIdentity): Promise<FSAWriterLifecycleLease>
  beginTerminal(kind: FSATerminalMutationKind): FSATerminalDrain
  close(): Promise<void>
  diagnostics(): FSAMutationSchedulerDiagnosticsSnapshot
}
