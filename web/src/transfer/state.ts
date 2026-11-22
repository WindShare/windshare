export type ReceivePauseReason =
  | 'user'
  | 'interrupted'
  | 'storage-full'
  | 'permission-needed'

export type ReceivePartialReason = 'failures' | 'stopped'

export type ReceiveFailureReason =
  | 'source-changed'
  | 'cancelled'
  | 'session-ended'
  | 'error'

export interface ReceiveTiming {
  readonly startedAtMilliseconds?: number
  readonly activeMilliseconds: number
  readonly activeSinceMilliseconds?: number
  readonly finishedAtMilliseconds?: number
}

export interface MissingFileWarning {
  readonly path: readonly string[]
  readonly reason: 'source-changed' | 'source-unavailable' | 'file-failed'
}

export interface ReceiveContentWarning {
  readonly selectedFileCount: bigint
  readonly completedFileCount: bigint
  readonly missingFiles: readonly MissingFileWarning[]
}

export interface SafeRenamedEntry {
  readonly logicalPath: readonly string[]
  readonly physicalName: string
}

export interface SafeRenamedSummary {
  readonly count: number
  readonly sample: readonly SafeRenamedEntry[]
}

export const MAX_SAFE_RENAMED_SAMPLE = 20

export interface SourceRevisionFailure {
  readonly entryId: string
  readonly path: readonly string[]
  readonly sourcePath: readonly string[]
  readonly reason: 'source-changed' | 'source-unavailable'
}

export interface SourceRevisionFailures {
  readonly shareInstance: string
  readonly count: bigint
  readonly files: readonly SourceRevisionFailure[]
}

interface ReceiveStateBase {
  readonly taskId: string
  readonly generation: bigint
  readonly timing?: ReceiveTiming
  readonly contentWarning?: ReceiveContentWarning
}

/**
 * Eight product-facing receive states matching real browser and user boundaries.
 * `preparing` is transient in memory; the other seven states can persist in IndexedDB.
 */
export type ReceiveLifecycleState =
  | Readonly<ReceiveStateBase & { kind: 'preparing' }>
  | Readonly<ReceiveStateBase & { kind: 'downloading'; phase: 'receiving' | 'finalizing' }>
  | Readonly<ReceiveStateBase & {
      kind: 'paused'
      reason: ReceivePauseReason
      completedFiles: bigint
      completedBytes: bigint
      retainedBytes: bigint
      discoveredFiles: bigint
      discoveredBytes: bigint
      discoveryComplete: boolean
    }>
  | Readonly<ReceiveStateBase & { kind: 'ready-to-save'; size: bigint; fileName: string }>
  | Readonly<ReceiveStateBase & { kind: 'download-started'; retained: boolean; fileName: string }>
  | Readonly<ReceiveStateBase & { kind: 'saved'; savedFiles: bigint; savedBytes: bigint }>
  | Readonly<ReceiveStateBase & {
      kind: 'partial'
      reason: ReceivePartialReason
      savedFiles: bigint
      failedFiles: bigint
    }>
  | Readonly<ReceiveStateBase & {
      kind: 'failed'
      reason: ReceiveFailureReason
      detail?: string
    }>

export type ReceiveLifecycleKind = ReceiveLifecycleState['kind']

export const RECEIVE_LIFECYCLE_KINDS: readonly ReceiveLifecycleKind[] = Object.freeze([
  'preparing',
  'downloading',
  'paused',
  'ready-to-save',
  'download-started',
  'saved',
  'partial',
  'failed',
])

export type ReceiveStatePayload = ReceiveLifecycleState extends infer S
  ? S extends ReceiveStateBase ? Omit<S, keyof ReceiveStateBase> : never
  : never

export function initialReceiveState(taskId: string, now = Date.now()): ReceiveLifecycleState {
  return Object.freeze({
    kind: 'preparing',
    taskId,
    generation: 1n,
    timing: Object.freeze({ startedAtMilliseconds: now, activeMilliseconds: 0 }),
  })
}

export function transitionReceiveState(
  current: ReceiveLifecycleState,
  payload: ReceiveStatePayload & Readonly<{ contentWarning?: ReceiveContentWarning | null }>,
  now = Date.now(),
): ReceiveLifecycleState {
  const { contentWarning, ...rest } = payload
  const nextWarning = Object.hasOwn(payload, 'contentWarning')
    ? contentWarning ?? undefined
    : current.contentWarning
  const timing = advanceReceiveTiming(current.timing, payload.kind, now)
  return Object.freeze({
    ...rest,
    taskId: current.taskId,
    generation: current.generation + 1n,
    timing,
    ...(nextWarning === undefined ? {} : { contentWarning: nextWarning }),
  }) as ReceiveLifecycleState
}

export function advanceReceiveTiming(
  previous: ReceiveTiming | undefined,
  nextKind: ReceiveLifecycleKind,
  now = Date.now(),
): ReceiveTiming {
  const active = nextKind === 'downloading'
  const terminal = nextKind === 'saved' || nextKind === 'partial' ||
    nextKind === 'failed' || nextKind === 'download-started' || nextKind === 'ready-to-save'
  let activeMs = previous?.activeMilliseconds ?? 0
  if (previous?.activeSinceMilliseconds !== undefined) {
    activeMs += Math.max(0, now - previous.activeSinceMilliseconds)
  }
  const startedAt = previous?.startedAtMilliseconds ?? now
  return Object.freeze({
    startedAtMilliseconds: startedAt,
    activeMilliseconds: activeMs,
    ...(active ? { activeSinceMilliseconds: now } : {}),
    ...(terminal ? { finishedAtMilliseconds: previous?.finishedAtMilliseconds ?? now } : {}),
  })
}

export function receiveElapsedMilliseconds(
  timing: ReceiveTiming | undefined,
  now = Date.now(),
): number | null {
  if (timing === undefined) return null
  if (timing.activeSinceMilliseconds !== undefined) {
    return timing.activeMilliseconds + Math.max(0, now - timing.activeSinceMilliseconds)
  }
  return timing.activeMilliseconds
}

export function isActiveReceiveState(state: ReceiveLifecycleState): boolean {
  return state.kind === 'preparing' || state.kind === 'downloading'
}

export function isResumableReceiveState(state: ReceiveLifecycleState): boolean {
  return state.kind === 'paused'
}

export function isDeliveredReceiveState(state: ReceiveLifecycleState | null): boolean {
  return state !== null && (
    state.kind === 'saved' ||
    state.kind === 'partial' ||
    state.kind === 'download-started' ||
    state.kind === 'ready-to-save'
  )
}

export function isTerminalReceiveState(state: ReceiveLifecycleState): boolean {
  return state.kind === 'saved' ||
    state.kind === 'partial' ||
    state.kind === 'failed' ||
    (state.kind === 'download-started' && !state.retained)
}

export function safeRenamedSummary(input: SafeRenamedSummary): SafeRenamedSummary {
  return Object.freeze({
    count: Math.max(0, input.count),
    sample: Object.freeze(input.sample.slice(0, MAX_SAFE_RENAMED_SAMPLE).map(entry => Object.freeze({
      logicalPath: Object.freeze([...entry.logicalPath]),
      physicalName: entry.physicalName,
    }))),
  })
}
