export const FaultDomain = {
  Source: 'source',
  Catalog: 'catalog',
  Session: 'session',
  Output: 'output',
  Checkpoint: 'checkpoint',
} as const

export type FaultDomain = typeof FaultDomain[keyof typeof FaultDomain]

// This declaration order is the cross-runtime severity order used by joinFaults.
export const FaultScope = {
  FileLocal: 'file-local',
  DirectoryLocal: 'directory-local',
  OutputPause: 'output-pause',
  SessionTerminal: 'session-terminal',
} as const

export type FaultScope = typeof FaultScope[keyof typeof FaultScope]

export const SourceFaultCode = {
  Unavailable: 'unavailable',
  RevisionChanged: 'revision-changed',
  RevisionInvalidated: 'revision-invalidated',
  Permanent: 'permanent',
} as const

export type SourceFaultCode = typeof SourceFaultCode[keyof typeof SourceFaultCode]

export const CatalogFaultCode = {
  Unavailable: 'unavailable',
  DirectoryStale: 'directory-stale',
  InvalidGeneration: 'invalid-generation',
} as const

export type CatalogFaultCode = typeof CatalogFaultCode[keyof typeof CatalogFaultCode]

export const SessionFaultCode = {
  Transport: 'transport',
  Protocol: 'protocol',
  ResourceBudget: 'resource-budget',
  DependencyContract: 'dependency-contract',
} as const

export type SessionFaultCode = typeof SessionFaultCode[keyof typeof SessionFaultCode]

export const OutputFaultCode = {
  StateIO: 'state-io',
  Ownership: 'ownership',
  NamespaceUnsafe: 'namespace-unsafe',
  UnsupportedFilesystem: 'unsupported-filesystem',
  DirectoryBinding: 'directory-binding',
  DirectoryMetadata: 'directory-metadata',
  FileAlreadyActive: 'file-already-active',
  ResourceBudget: 'resource-budget',
  MutationAmbiguous: 'mutation-ambiguous',
  Contract: 'contract',
} as const

export type OutputFaultCode = typeof OutputFaultCode[keyof typeof OutputFaultCode]

export const CheckpointFaultCode = {
  Busy: 'busy',
  CorruptRecord: 'corrupt-record',
  UnsafeInstall: 'unsafe-install',
  OwnershipMismatch: 'ownership-mismatch',
  StateIO: 'state-io',
} as const

export type CheckpointFaultCode = typeof CheckpointFaultCode[keyof typeof CheckpointFaultCode]

type FaultValue<Domain extends FaultDomain, Code extends string> = Readonly<{
  domain: Domain
  scope: FaultScope
  code: Code
}>

export type SourceFault = FaultValue<typeof FaultDomain.Source, SourceFaultCode>
export type CatalogFault = FaultValue<typeof FaultDomain.Catalog, CatalogFaultCode>
export type SessionFault = FaultValue<typeof FaultDomain.Session, SessionFaultCode>
export type OutputFault = FaultValue<typeof FaultDomain.Output, OutputFaultCode>
export type CheckpointFault = FaultValue<typeof FaultDomain.Checkpoint, CheckpointFaultCode>
export type Fault = SourceFault | CatalogFault | SessionFault | OutputFault | CheckpointFault

const faultDomainOrder = Object.freeze([
  FaultDomain.Source,
  FaultDomain.Catalog,
  FaultDomain.Session,
  FaultDomain.Output,
  FaultDomain.Checkpoint,
] as const)

const faultScopeOrder = Object.freeze([
  FaultScope.FileLocal,
  FaultScope.DirectoryLocal,
  FaultScope.OutputPause,
  FaultScope.SessionTerminal,
] as const)

const sourceCodeOrder = Object.freeze([
  SourceFaultCode.Unavailable,
  SourceFaultCode.RevisionChanged,
  SourceFaultCode.RevisionInvalidated,
  SourceFaultCode.Permanent,
] as const)

const catalogCodeOrder = Object.freeze([
  CatalogFaultCode.Unavailable,
  CatalogFaultCode.DirectoryStale,
  CatalogFaultCode.InvalidGeneration,
] as const)

const sessionCodeOrder = Object.freeze([
  SessionFaultCode.Transport,
  SessionFaultCode.Protocol,
  SessionFaultCode.ResourceBudget,
  SessionFaultCode.DependencyContract,
] as const)

const outputCodeOrder = Object.freeze([
  OutputFaultCode.StateIO,
  OutputFaultCode.Ownership,
  OutputFaultCode.NamespaceUnsafe,
  OutputFaultCode.UnsupportedFilesystem,
  OutputFaultCode.DirectoryBinding,
  OutputFaultCode.DirectoryMetadata,
  OutputFaultCode.FileAlreadyActive,
  OutputFaultCode.ResourceBudget,
  OutputFaultCode.MutationAmbiguous,
  OutputFaultCode.Contract,
] as const)

const checkpointCodeOrder = Object.freeze([
  CheckpointFaultCode.Busy,
  CheckpointFaultCode.CorruptRecord,
  CheckpointFaultCode.UnsafeInstall,
  CheckpointFaultCode.OwnershipMismatch,
  CheckpointFaultCode.StateIO,
] as const)

const INVALID_FAULT_MESSAGE = 'Transfer fault is invalid'
const BOUNDARY_FAULT_MESSAGE = 'Transfer collaborator returned a normalized fault'

export function sourceFault(scope: FaultScope, code: SourceFaultCode): SourceFault {
  return createFault(FaultDomain.Source, scope, code) as SourceFault
}

export function catalogFault(scope: FaultScope, code: CatalogFaultCode): CatalogFault {
  return createFault(FaultDomain.Catalog, scope, code) as CatalogFault
}

export function sessionFault(scope: FaultScope, code: SessionFaultCode): SessionFault {
  return createFault(FaultDomain.Session, scope, code) as SessionFault
}

export function outputFault(scope: FaultScope, code: OutputFaultCode): OutputFault {
  return createFault(FaultDomain.Output, scope, code) as OutputFault
}

export function checkpointFault(scope: FaultScope, code: CheckpointFaultCode): CheckpointFault {
  return createFault(FaultDomain.Checkpoint, scope, code) as CheckpointFault
}

function createFault(domain: FaultDomain, scope: FaultScope, code: string): Fault {
  const candidate = { domain, scope, code }
  if (!isFault(candidate)) throw new TypeError(INVALID_FAULT_MESSAGE)
  return immutableFault(candidate)
}

function immutableFault(value: Fault): Fault {
  return Object.freeze({ ...value }) as Fault
}

export function isFault(value: unknown): value is Fault {
  if (typeof value !== 'object' || value === null) return false
  const candidate = value as Readonly<Record<string, unknown>>
  if (!isMember(faultScopeOrder, candidate.scope)) return false
  switch (candidate.domain) {
    case FaultDomain.Source:
      return isMember(sourceCodeOrder, candidate.code)
    case FaultDomain.Catalog:
      return isMember(catalogCodeOrder, candidate.code)
    case FaultDomain.Session:
      return isMember(sessionCodeOrder, candidate.code)
    case FaultDomain.Output:
      return isMember(outputCodeOrder, candidate.code)
    case FaultDomain.Checkpoint:
      return isMember(checkpointCodeOrder, candidate.code)
    default:
      return false
  }
}

function isMember<const Value extends string>(values: readonly Value[], value: unknown): value is Value {
  return typeof value === 'string' && values.some(candidate => candidate === value)
}

// Domain and code are deterministic tie-breakers, not additional authority.
export function compareFaults(left: Fault, right: Fault): number {
  assertFault(left)
  assertFault(right)
  const scopeComparison = compareNumber(
    faultScopeOrder.indexOf(left.scope),
    faultScopeOrder.indexOf(right.scope),
  )
  if (scopeComparison !== 0) return scopeComparison
  const domainComparison = compareNumber(
    faultDomainOrder.indexOf(left.domain),
    faultDomainOrder.indexOf(right.domain),
  )
  if (domainComparison !== 0) return domainComparison
  return compareNumber(codeOrder(left), codeOrder(right))
}

export function joinFaults(...faults: readonly Fault[]): Fault | undefined {
  let joined: Fault | undefined
  for (const candidate of faults) {
    assertFault(candidate)
    if (joined === undefined || compareFaults(candidate, joined) > 0) {
      joined = candidate
    }
  }
  return joined === undefined ? undefined : immutableFault(joined)
}

/** Raises policy severity without changing the normalized domain decision. */
export function promoteFaultScope(fault: Fault, minimumScope: FaultScope): Fault {
  assertFault(fault)
  if (!isMember(faultScopeOrder, minimumScope)) throw new TypeError(INVALID_FAULT_MESSAGE)
  if (faultScopeOrder.indexOf(fault.scope) >= faultScopeOrder.indexOf(minimumScope)) {
    return immutableFault(fault)
  }
  switch (fault.domain) {
    case FaultDomain.Source:
      return sourceFault(minimumScope, fault.code)
    case FaultDomain.Catalog:
      return catalogFault(minimumScope, fault.code)
    case FaultDomain.Session:
      return sessionFault(minimumScope, fault.code)
    case FaultDomain.Output:
      return outputFault(minimumScope, fault.code)
    case FaultDomain.Checkpoint:
      return checkpointFault(minimumScope, fault.code)
  }
}

function codeOrder(fault: Fault): number {
  switch (fault.domain) {
    case FaultDomain.Source:
      return sourceCodeOrder.indexOf(fault.code)
    case FaultDomain.Catalog:
      return catalogCodeOrder.indexOf(fault.code)
    case FaultDomain.Session:
      return sessionCodeOrder.indexOf(fault.code)
    case FaultDomain.Output:
      return outputCodeOrder.indexOf(fault.code)
    case FaultDomain.Checkpoint:
      return checkpointCodeOrder.indexOf(fault.code)
  }
}

function compareNumber(left: number, right: number): number {
  if (left < right) return -1
  if (left > right) return 1
  return 0
}

function assertFault(value: unknown): asserts value is Fault {
  if (!isFault(value)) throw new TypeError(INVALID_FAULT_MESSAGE)
}

/**
 * Carries only closed product authority across a failure boundary. Native errors
 * stay at their classifier so later policy cannot recover authority from causes.
 */
export class BoundaryFaultError extends Error {
  readonly fault: Fault

  constructor(fault: Fault, message = BOUNDARY_FAULT_MESSAGE) {
    assertFault(fault)
    super(message)
    this.name = 'BoundaryFaultError'
    this.fault = immutableFault(fault)
  }
}

export type BoundaryNormalization =
  | Readonly<{ kind: 'success' }>
  | Readonly<{ kind: 'canceled' }>
  | Readonly<{ kind: 'fault', fault: Fault }>

const successfulBoundary = Object.freeze({ kind: 'success' } as const)
const canceledBoundary = Object.freeze({ kind: 'canceled' } as const)

export function normalizeBoundaryError(
  error: unknown,
  signal?: AbortSignal,
): BoundaryNormalization {
  if (signal?.aborted === true) return canceledBoundary
  if (error instanceof BoundaryFaultError && isFault(error.fault)) {
    return Object.freeze({ kind: 'fault', fault: immutableFault(error.fault) })
  }
  if (error === undefined) return successfulBoundary
  return Object.freeze({ kind: 'fault', fault: dependencyContractFault() })
}

export function dependencyContractFault(): SessionFault {
  return sessionFault(FaultScope.OutputPause, SessionFaultCode.DependencyContract)
}

export const FaultRetirement = {
  PermanentSource: 'permanent-source',
  InvalidatedRevision: 'invalidated-revision',
} as const

export type FaultRetirement = typeof FaultRetirement[keyof typeof FaultRetirement]

function retirementForFault(fault: Fault): FaultRetirement | undefined {
  if (!isFault(fault) || fault.domain !== FaultDomain.Source || fault.scope !== FaultScope.FileLocal) {
    return undefined
  }
  switch (fault.code) {
    case SourceFaultCode.Permanent:
      return FaultRetirement.PermanentSource
    case SourceFaultCode.RevisionInvalidated:
      return FaultRetirement.InvalidatedRevision
    default:
      return undefined
  }
}

const FILE_RETIREMENT_ISSUER = Object.freeze({})
const FILE_RETIREMENT_CONSUME = Symbol('windshare/file-retirement-consume')

/**
 * An owned object may be removed only while holding this one-use capability.
 * The constructor token and code allowlist prevent raw errors or severity from
 * being promoted into deletion authority by a downstream backend.
 */
export class FileRetirementAuthorization {
  readonly fault: Fault
  readonly retirement: FaultRetirement
  #available = true

  constructor(
    issuer: typeof FILE_RETIREMENT_ISSUER,
    fault: Fault,
    retirement: FaultRetirement,
  ) {
    if (issuer !== FILE_RETIREMENT_ISSUER || retirementForFault(fault) !== retirement) {
      throw new TypeError('File retirement authorization is invalid')
    }
    this.fault = immutableFault(fault)
    this.retirement = retirement
    Object.freeze(this)
  }

  [FILE_RETIREMENT_CONSUME](): FaultRetirement {
    if (!this.#available) throw new TypeError('File retirement authorization was already consumed')
    this.#available = false
    return this.retirement
  }
}

export function authorizeFileRetirement(fault: Fault): FileRetirementAuthorization | undefined {
  const retirement = retirementForFault(fault)
  return retirement === undefined
    ? undefined
    : new FileRetirementAuthorization(FILE_RETIREMENT_ISSUER, fault, retirement)
}

export function consumeFileRetirementAuthorization(value: unknown): FaultRetirement {
  if (!(value instanceof FileRetirementAuthorization)) {
    throw new TypeError('File retirement requires an allowlisted authorization')
  }
  return value[FILE_RETIREMENT_CONSUME]()
}

/** Ambiguous ownership/publication evidence must remain visible to the user. */
export function faultRequiresAttention(fault: Fault): boolean {
  if (!isFault(fault) || faultScopeOrder.indexOf(fault.scope) < faultScopeOrder.indexOf(FaultScope.OutputPause)) {
    return false
  }
  if (fault.domain === FaultDomain.Checkpoint) {
    return fault.code === CheckpointFaultCode.CorruptRecord ||
      fault.code === CheckpointFaultCode.UnsafeInstall ||
      fault.code === CheckpointFaultCode.OwnershipMismatch
  }
  return fault.domain === FaultDomain.Output && (
    fault.code === OutputFaultCode.Ownership ||
    fault.code === OutputFaultCode.NamespaceUnsafe ||
    fault.code === OutputFaultCode.MutationAmbiguous
  )
}
