export const FILE_CHECKPOINT_PHASE_RESERVED = 1 as const
export const FILE_CHECKPOINT_PHASE_ACTIVE = 2 as const
export const FILE_CHECKPOINT_PHASE_PAUSED = 3 as const
export const FILE_CHECKPOINT_PHASE_PUBLISHING = 4 as const
export const FILE_CHECKPOINT_PHASE_PUBLISHED = 5 as const
export const FILE_CHECKPOINT_PHASE_QUARANTINED = 6 as const
export const FILE_CHECKPOINT_PHASE_RETIRED = 7 as const

export const FILE_CHECKPOINT_COMMIT_CANDIDATE = 1 as const
export const FILE_CHECKPOINT_COMMIT_VERIFIED = 2 as const
export const FILE_CHECKPOINT_COMMIT_PUBLISHED = 3 as const
export const FILE_CHECKPOINT_COMMIT_QUARANTINED = 4 as const

export const FILE_CHECKPOINT_QUARANTINE_ANCHOR_MISSING = 1 as const
export const FILE_CHECKPOINT_QUARANTINE_ANCHOR_UNSAFE = 2 as const
export const FILE_CHECKPOINT_QUARANTINE_STAGE_MISSING = 3 as const
export const FILE_CHECKPOINT_QUARANTINE_STAGE_MISMATCH = 4 as const
export const FILE_CHECKPOINT_QUARANTINE_STAGE_UNSAFE = 5 as const
export const FILE_CHECKPOINT_QUARANTINE_FINAL_MISMATCH = 6 as const
export const FILE_CHECKPOINT_QUARANTINE_FINAL_UNSAFE = 7 as const
export const FILE_CHECKPOINT_QUARANTINE_PARTIAL_OBJECT_CREATION = 8 as const
export const FILE_CHECKPOINT_QUARANTINE_PUBLICATION_HISTORY = 9 as const
export const FILE_CHECKPOINT_QUARANTINE_METADATA_MISMATCH = 10 as const
export const FILE_CHECKPOINT_QUARANTINE_UPDATE_TEMPORARY = 11 as const
export const FILE_CHECKPOINT_QUARANTINE_OUTPUT_OBJECT_DUPLICATE = 12 as const

export const FILE_CHECKPOINT_ORIGIN_RESERVED = 1 as const
export const FILE_CHECKPOINT_ORIGIN_WITNESSED = 2 as const
export const FILE_CHECKPOINT_ORIGIN_PUBLISHING = 3 as const
export const FILE_CHECKPOINT_ORIGIN_PUBLISH_BLOCKED = 4 as const
export const FILE_CHECKPOINT_ORIGIN_PUBLISHED = 5 as const
export const FILE_CHECKPOINT_ORIGIN_RETIRING = 6 as const

export const FILE_CHECKPOINT_RETIREMENT_PUBLISHED = 1 as const
export const FILE_CHECKPOINT_RETIREMENT_ISOLATED_FAILURE = 2 as const
export const FILE_CHECKPOINT_RETIREMENT_PRE_OBJECT_COLLISION = 3 as const
export const FILE_CHECKPOINT_RETIREMENT_INVALIDATED_REVISION = 4 as const

export const FILE_CHECKPOINT_MAX_QUARANTINE_REASON = FILE_CHECKPOINT_QUARANTINE_OUTPUT_OBJECT_DUPLICATE
export const FILE_CHECKPOINT_MAX_QUARANTINE_ORIGIN = FILE_CHECKPOINT_ORIGIN_RETIRING
export const FILE_CHECKPOINT_MAX_RETIREMENT_REASON = FILE_CHECKPOINT_RETIREMENT_INVALIDATED_REVISION

export type FileCheckpointPhase = typeof FILE_CHECKPOINT_PHASE_RESERVED | typeof FILE_CHECKPOINT_PHASE_ACTIVE |
  typeof FILE_CHECKPOINT_PHASE_PAUSED | typeof FILE_CHECKPOINT_PHASE_PUBLISHING |
  typeof FILE_CHECKPOINT_PHASE_PUBLISHED | typeof FILE_CHECKPOINT_PHASE_QUARANTINED | typeof FILE_CHECKPOINT_PHASE_RETIRED
export type FileCheckpointCommitState = typeof FILE_CHECKPOINT_COMMIT_CANDIDATE |
  typeof FILE_CHECKPOINT_COMMIT_VERIFIED | typeof FILE_CHECKPOINT_COMMIT_PUBLISHED | typeof FILE_CHECKPOINT_COMMIT_QUARANTINED
export type FileCheckpointQuarantineReason = typeof FILE_CHECKPOINT_QUARANTINE_ANCHOR_MISSING |
  typeof FILE_CHECKPOINT_QUARANTINE_ANCHOR_UNSAFE | typeof FILE_CHECKPOINT_QUARANTINE_STAGE_MISSING |
  typeof FILE_CHECKPOINT_QUARANTINE_STAGE_MISMATCH | typeof FILE_CHECKPOINT_QUARANTINE_STAGE_UNSAFE |
  typeof FILE_CHECKPOINT_QUARANTINE_FINAL_MISMATCH | typeof FILE_CHECKPOINT_QUARANTINE_FINAL_UNSAFE |
  typeof FILE_CHECKPOINT_QUARANTINE_PARTIAL_OBJECT_CREATION | typeof FILE_CHECKPOINT_QUARANTINE_PUBLICATION_HISTORY |
  typeof FILE_CHECKPOINT_QUARANTINE_METADATA_MISMATCH | typeof FILE_CHECKPOINT_QUARANTINE_UPDATE_TEMPORARY |
  typeof FILE_CHECKPOINT_QUARANTINE_OUTPUT_OBJECT_DUPLICATE
export type FileCheckpointQuarantineOrigin = typeof FILE_CHECKPOINT_ORIGIN_RESERVED |
  typeof FILE_CHECKPOINT_ORIGIN_WITNESSED | typeof FILE_CHECKPOINT_ORIGIN_PUBLISHING |
  typeof FILE_CHECKPOINT_ORIGIN_PUBLISH_BLOCKED | typeof FILE_CHECKPOINT_ORIGIN_PUBLISHED |
  typeof FILE_CHECKPOINT_ORIGIN_RETIRING
export type FileCheckpointRetirementReason = typeof FILE_CHECKPOINT_RETIREMENT_PUBLISHED |
  typeof FILE_CHECKPOINT_RETIREMENT_ISOLATED_FAILURE | typeof FILE_CHECKPOINT_RETIREMENT_PRE_OBJECT_COLLISION |
  typeof FILE_CHECKPOINT_RETIREMENT_INVALIDATED_REVISION

export interface FileCheckpointLifecycle {
  readonly phase: FileCheckpointPhase
  readonly commitState: FileCheckpointCommitState
}

export class FileCheckpointError extends Error {
  readonly code:
    | 'invalid'
    | 'checksum'
    | 'non-canonical'
    | 'binding'
    | 'generation'
    | 'ownership'
    | 'recovery'
    | 'crash-boundary'

  constructor(code: FileCheckpointError['code'], message: string) {
    super(message)
    this.name = 'FileCheckpointError'
    this.code = code
  }
}

export function normalizeCheckpointPhase(value: FileCheckpointPhase | number | string): FileCheckpointPhase {
  if (typeof value === 'number' && value >= FILE_CHECKPOINT_PHASE_RESERVED && value <= FILE_CHECKPOINT_PHASE_RETIRED) {
    return value as FileCheckpointPhase
  }
  const aliases: Record<string, FileCheckpointPhase> = {
    reserved: 1, active: 2, paused: 3, publishing: 4, published: 5, quarantined: 6, retired: 7,
  }
  if (typeof value === 'string' && aliases[value] !== undefined) return aliases[value]!
  throw new FileCheckpointError('invalid', 'checkpoint phase is invalid')
}

export function normalizeCheckpointCommitState(
  value: FileCheckpointCommitState | number | string,
): FileCheckpointCommitState {
  if (typeof value === 'number' && value >= FILE_CHECKPOINT_COMMIT_CANDIDATE &&
      value <= FILE_CHECKPOINT_COMMIT_QUARANTINED) return value as FileCheckpointCommitState
  const aliases: Record<string, FileCheckpointCommitState> = {
    candidate: 1, verified: 2, published: 3, quarantined: 4,
  }
  if (typeof value === 'string' && aliases[value] !== undefined) return aliases[value]!
  throw new FileCheckpointError('invalid', 'checkpoint commit state is invalid')
}

export function normalizeCheckpointLifecycleClaim<T extends number>(
  value: number,
  maximum: number,
  label: string,
): T | 0 {
  if (Number.isInteger(value) && value >= 0 && value <= maximum) return value as T | 0
  throw new FileCheckpointError('invalid', `checkpoint ${label} is invalid`)
}

export function validateCheckpointLifecycleClaims(
  phase: FileCheckpointPhase,
  quarantineReason: number,
  quarantineOrigin: number,
  retirementReason: number,
): void {
  if (phase === FILE_CHECKPOINT_PHASE_QUARANTINED) {
    const retirementValid = retirementReason >= 1 && retirementReason <= FILE_CHECKPOINT_MAX_RETIREMENT_REASON
    if (quarantineReason < 1 || quarantineReason > FILE_CHECKPOINT_MAX_QUARANTINE_REASON ||
        quarantineOrigin < 1 || quarantineOrigin > FILE_CHECKPOINT_MAX_QUARANTINE_ORIGIN ||
        !validQuarantineHistory(quarantineOrigin, quarantineReason) ||
        (quarantineOrigin === FILE_CHECKPOINT_ORIGIN_RETIRING) !== retirementValid) {
      throw new FileCheckpointError('invalid', 'checkpoint quarantined lifecycle claims are invalid')
    }
    return
  }
  if (quarantineReason !== 0 || quarantineOrigin !== 0) {
    throw new FileCheckpointError('invalid', 'checkpoint quarantine claims require the quarantined phase')
  }
  if (phase === FILE_CHECKPOINT_PHASE_RETIRED) {
    if (retirementReason < 1 || retirementReason > FILE_CHECKPOINT_MAX_RETIREMENT_REASON) {
      throw new FileCheckpointError('invalid', 'checkpoint retired lifecycle claim is invalid')
    }
    return
  }
  if (retirementReason !== 0) {
    throw new FileCheckpointError('invalid', 'checkpoint retirement claim requires retired lifecycle history')
  }
}

export function validCheckpointLifecycleTransition(
  previous: FileCheckpointLifecycle,
  next: FileCheckpointLifecycle,
): boolean {
  if (next.commitState < previous.commitState) return false
  if (next.phase === FILE_CHECKPOINT_PHASE_PUBLISHED) {
    if (next.commitState !== FILE_CHECKPOINT_COMMIT_PUBLISHED) return false
  } else if (next.phase === FILE_CHECKPOINT_PHASE_QUARANTINED) {
    if (next.commitState !== FILE_CHECKPOINT_COMMIT_QUARANTINED) return false
  } else if (next.commitState !== FILE_CHECKPOINT_COMMIT_VERIFIED) return false

  switch (previous.phase) {
    case FILE_CHECKPOINT_PHASE_RESERVED:
      return next.phase === FILE_CHECKPOINT_PHASE_ACTIVE || next.phase === FILE_CHECKPOINT_PHASE_RETIRED ||
        next.phase === FILE_CHECKPOINT_PHASE_QUARANTINED
    case FILE_CHECKPOINT_PHASE_ACTIVE:
      return next.phase === FILE_CHECKPOINT_PHASE_PAUSED || next.phase === FILE_CHECKPOINT_PHASE_PUBLISHING ||
        next.phase === FILE_CHECKPOINT_PHASE_RETIRED || next.phase === FILE_CHECKPOINT_PHASE_QUARANTINED
    case FILE_CHECKPOINT_PHASE_PAUSED:
      return next.phase === FILE_CHECKPOINT_PHASE_ACTIVE || next.phase === FILE_CHECKPOINT_PHASE_PUBLISHING ||
        next.phase === FILE_CHECKPOINT_PHASE_RETIRED || next.phase === FILE_CHECKPOINT_PHASE_QUARANTINED
    case FILE_CHECKPOINT_PHASE_PUBLISHING:
      return next.phase === FILE_CHECKPOINT_PHASE_PAUSED || next.phase === FILE_CHECKPOINT_PHASE_PUBLISHED ||
        next.phase === FILE_CHECKPOINT_PHASE_RETIRED || next.phase === FILE_CHECKPOINT_PHASE_QUARANTINED
    default:
      return false
  }
}

function validQuarantineHistory(origin: number, reason: number): boolean {
  switch (reason) {
    case FILE_CHECKPOINT_QUARANTINE_ANCHOR_MISSING:
      return origin >= FILE_CHECKPOINT_ORIGIN_WITNESSED && origin <= FILE_CHECKPOINT_ORIGIN_PUBLISHED
    case FILE_CHECKPOINT_QUARANTINE_ANCHOR_UNSAFE:
    case FILE_CHECKPOINT_QUARANTINE_STAGE_UNSAFE:
    case FILE_CHECKPOINT_QUARANTINE_UPDATE_TEMPORARY:
    case FILE_CHECKPOINT_QUARANTINE_OUTPUT_OBJECT_DUPLICATE:
    case FILE_CHECKPOINT_QUARANTINE_STAGE_MISMATCH:
      return origin >= FILE_CHECKPOINT_ORIGIN_RESERVED && origin <= FILE_CHECKPOINT_ORIGIN_RETIRING
    case FILE_CHECKPOINT_QUARANTINE_STAGE_MISSING:
      return origin >= FILE_CHECKPOINT_ORIGIN_WITNESSED && origin <= FILE_CHECKPOINT_ORIGIN_PUBLISH_BLOCKED
    case FILE_CHECKPOINT_QUARANTINE_FINAL_MISMATCH:
      return origin === FILE_CHECKPOINT_ORIGIN_PUBLISHED
    case FILE_CHECKPOINT_QUARANTINE_FINAL_UNSAFE:
      return origin >= FILE_CHECKPOINT_ORIGIN_RESERVED && origin <= FILE_CHECKPOINT_ORIGIN_PUBLISHED
    case FILE_CHECKPOINT_QUARANTINE_PARTIAL_OBJECT_CREATION:
      return origin === FILE_CHECKPOINT_ORIGIN_RESERVED || origin === FILE_CHECKPOINT_ORIGIN_RETIRING
    case FILE_CHECKPOINT_QUARANTINE_PUBLICATION_HISTORY:
      return origin >= FILE_CHECKPOINT_ORIGIN_RESERVED && origin <= FILE_CHECKPOINT_ORIGIN_PUBLISH_BLOCKED
    case FILE_CHECKPOINT_QUARANTINE_METADATA_MISMATCH:
      return origin === FILE_CHECKPOINT_ORIGIN_PUBLISHING || origin === FILE_CHECKPOINT_ORIGIN_PUBLISHED
    default:
      return false
  }
}
