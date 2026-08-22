import {
  decimalUint64,
  deepFreezeJson,
  uint32,
} from './json'

export const CONTROLLER_CONTEXT_PHASES = Object.freeze([
  'awaiting-key',
  'joining',
  'browsing',
  'failed',
] as const)

export type ControllerContextPhase =
  (typeof CONTROLLER_CONTEXT_PHASES)[number]

export const LIFECYCLE_CONTEXT_STATES = Object.freeze([
  'intent-frozen',
  'preparing',
  'receiving',
  'resumable-receive',
  'finalizing-tree',
  'committing-atomic',
  'materialization-sealed',
  'packaging',
  'resumable-package',
  'artifact-sealed',
  'waiting-to-save',
  'publishing-managed',
  'handing-off',
  'published',
  'download-started',
  'partial-directory',
  'restart-required',
  'discarded',
  'expired',
  'needs-attention',
  'authorization-required',
  'target-verification-required',
  'destination-space-required',
] as const)

export type LifecycleContextState =
  (typeof LIFECYCLE_CONTEXT_STATES)[number]

export const PROGRESS_DISCOVERY_STATES = Object.freeze([
  'open',
  'complete',
  'failed',
] as const)

export type ProgressDiscoveryState =
  (typeof PROGRESS_DISCOVERY_STATES)[number]

export const OUTPUT_PLAN_KINDS = Object.freeze([
  'direct-tree',
  'direct-atomic',
  'workspace-then-publish',
  'portable-handoff',
  'direct-resumable-zip',
] as const)

export type OutputPlanKind = (typeof OUTPUT_PLAN_KINDS)[number]

export interface ControllerContextSnapshot {
  readonly generation: bigint
  readonly phase: ControllerContextPhase
}

export interface LifecycleContextSnapshot {
  readonly generation: bigint
  readonly state: LifecycleContextState
}

export interface ProgressContextSnapshot {
  readonly generation: bigint
  readonly discovery: ProgressDiscoveryState
  readonly discoveredFiles: bigint
  readonly discoveredBytes: bigint
  readonly writtenBytes: bigint
  readonly completedFiles: bigint
  readonly completedBytes: bigint
  readonly fileErrors: bigint
  readonly selectionErrors: bigint
  readonly failedDirectories: bigint
  readonly contentLanes: number
}

export interface OutputContextSnapshot {
  readonly generation: bigint
  readonly planKind: OutputPlanKind
}

export interface ProtocolContextSnapshot {
  readonly generation: bigint
}

export interface DiagnosticContextSource<Snapshot> {
  read(): Snapshot | undefined
}

export interface DiagnosticContextSources {
  readonly controller?: DiagnosticContextSource<ControllerContextSnapshot>
  readonly lifecycle?: DiagnosticContextSource<LifecycleContextSnapshot>
  readonly progress?: DiagnosticContextSource<ProgressContextSnapshot>
  readonly output?: DiagnosticContextSource<OutputContextSnapshot>
  readonly protocol?: DiagnosticContextSource<ProtocolContextSnapshot>
}

export interface DiagnosticContextV1 {
  readonly controller?: Readonly<{
    generation: string
    phase: 'awaiting_key' | 'joining' | 'browsing' | 'failed'
  }>
  readonly lifecycle?: Readonly<{
    generation: string
    state:
      | 'intent_frozen'
      | 'preparing'
      | 'receiving'
      | 'resumable_receive'
      | 'finalizing_tree'
      | 'committing_atomic'
      | 'materialization_sealed'
      | 'packaging'
      | 'resumable_package'
      | 'artifact_sealed'
      | 'waiting_to_save'
      | 'publishing_managed'
      | 'handing_off'
      | 'published'
      | 'download_started'
      | 'partial_directory'
      | 'restart_required'
      | 'discarded'
      | 'expired'
      | 'needs_attention'
      | 'authorization_required'
      | 'target_verification_required'
      | 'destination_space_required'
  }>
  readonly progress?: Readonly<{
    generation: string
    discovery: ProgressDiscoveryState
    discovered_files: string
    discovered_bytes: string
    written_bytes: string
    completed_files: string
    completed_bytes: string
    file_errors: string
    selection_errors: string
    failed_directories: string
    content_lanes: number
  }>
  readonly output?: Readonly<{
    generation: string
    plan_kind:
      | 'direct_tree'
      | 'direct_atomic'
      | 'workspace_then_publish'
      | 'portable_handoff'
      | 'direct_resumable_zip'
  }>
  readonly protocol?: Readonly<{
    generation: string
  }>
}

export function captureDiagnosticContextV1(
  sources: DiagnosticContextSources = {},
): DiagnosticContextV1 {
  const controller = readController(sources.controller)
  const lifecycle = readLifecycle(sources.lifecycle)
  const progress = readProgress(sources.progress)
  const output = readOutput(sources.output)
  const protocol = readProtocol(sources.protocol)
  return deepFreezeJson({
    ...(controller === undefined ? {} : { controller }),
    ...(lifecycle === undefined ? {} : { lifecycle }),
    ...(progress === undefined ? {} : { progress }),
    ...(output === undefined ? {} : { output }),
    ...(protocol === undefined ? {} : { protocol }),
  })
}

function readController(
  source: DiagnosticContextSources['controller'],
): DiagnosticContextV1['controller'] {
  return readSource(source, (snapshot) => {
    if (!isMember(CONTROLLER_CONTEXT_PHASES, snapshot.phase)) {
      throw new TypeError('controller context phase is invalid')
    }
    return {
      generation: decimalUint64(snapshot.generation, 'controller generation'),
      phase: snapshot.phase === 'awaiting-key' ? 'awaiting_key' : snapshot.phase,
    }
  })
}

function readLifecycle(
  source: DiagnosticContextSources['lifecycle'],
): DiagnosticContextV1['lifecycle'] {
  return readSource(source, (snapshot) => {
    if (!isMember(LIFECYCLE_CONTEXT_STATES, snapshot.state)) {
      throw new TypeError('lifecycle context state is invalid')
    }
    return {
      generation: decimalUint64(snapshot.generation, 'lifecycle generation'),
      state: snapshot.state.replaceAll('-', '_') as NonNullable<
        DiagnosticContextV1['lifecycle']
      >['state'],
    }
  })
}

function readProgress(
  source: DiagnosticContextSources['progress'],
): DiagnosticContextV1['progress'] {
  return readSource(source, (snapshot) => {
    if (!isMember(PROGRESS_DISCOVERY_STATES, snapshot.discovery)) {
      throw new TypeError('progress discovery state is invalid')
    }
    return {
      generation: decimalUint64(snapshot.generation, 'progress generation'),
      discovery: snapshot.discovery,
      discovered_files: decimalUint64(
        snapshot.discoveredFiles,
        'discovered file count',
      ),
      discovered_bytes: decimalUint64(
        snapshot.discoveredBytes,
        'discovered byte count',
      ),
      written_bytes: decimalUint64(snapshot.writtenBytes, 'written byte count'),
      completed_files: decimalUint64(
        snapshot.completedFiles,
        'completed file count',
      ),
      completed_bytes: decimalUint64(
        snapshot.completedBytes,
        'completed byte count',
      ),
      file_errors: decimalUint64(snapshot.fileErrors, 'file error count'),
      selection_errors: decimalUint64(
        snapshot.selectionErrors,
        'selection error count',
      ),
      failed_directories: decimalUint64(
        snapshot.failedDirectories,
        'failed directory count',
      ),
      content_lanes: uint32(snapshot.contentLanes, 'content lane count'),
    }
  })
}

function readOutput(
  source: DiagnosticContextSources['output'],
): DiagnosticContextV1['output'] {
  return readSource(source, (snapshot) => {
    if (!isMember(OUTPUT_PLAN_KINDS, snapshot.planKind)) {
      throw new TypeError('output plan kind is invalid')
    }
    return {
      generation: decimalUint64(snapshot.generation, 'output generation'),
      plan_kind: snapshot.planKind.replaceAll('-', '_') as NonNullable<
        DiagnosticContextV1['output']
      >['plan_kind'],
    }
  })
}

function readProtocol(
  source: DiagnosticContextSources['protocol'],
): DiagnosticContextV1['protocol'] {
  return readSource(source, (snapshot) => ({
    generation: decimalUint64(snapshot.generation, 'protocol generation'),
  }))
}

function readSource<Snapshot, Projected>(
  source: DiagnosticContextSource<Snapshot> | undefined,
  project: (snapshot: Snapshot) => Projected,
): Projected | undefined {
  if (source === undefined) return undefined
  try {
    const snapshot = source.read()
    return snapshot === undefined ? undefined : project(snapshot)
  } catch {
    // One inconsistent producer snapshot cannot suppress other safe context.
    return undefined
  }
}

function isMember<const Value extends string>(
  values: readonly Value[],
  value: unknown,
): value is Value {
  return typeof value === 'string' && values.includes(value as Value)
}
