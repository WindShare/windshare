import {
  failureFactRelationForRef,
  isKnownFailureFactRef,
  type FailureFactRef,
  type IncidentScopeKind,
} from './scope'

export const PRESENTATION_BOUNDARIES = Object.freeze([
  'join',
  'browse',
  'preview',
  'projection_authority',
  'receive',
  'lifecycle_action',
  'retained_inventory',
  'retained_action',
] as const)

export type PresentationBoundary = (typeof PRESENTATION_BOUNDARIES)[number]

export const PRESENTATION_OUTCOMES = Object.freeze([
  'failed',
  'partial_directory_failures',
  'resumable_receive',
  'resumable_package',
  'restart_required',
  'needs_attention',
] as const)

export type PresentationOutcome = (typeof PRESENTATION_OUTCOMES)[number]

export const PRESENTATION_EXCLUSION_REASONS = Object.freeze([
  'success',
  'cancelled',
  'stale_replacement',
  'user_paused',
  'user_stopped',
  'user_discarded',
  'normal_expiry',
  'picker_refused',
  'authority_invalidated',
  'not_user_visible',
] as const)

export type PresentationExclusionReason =
  (typeof PRESENTATION_EXCLUSION_REASONS)[number]

export type PresentationDecision =
  | Readonly<{
      kind: 'incident'
      boundary: PresentationBoundary
      outcome: PresentationOutcome
      trigger: FailureFactRef
    }>
  | Readonly<{
      kind: 'excluded'
      boundary: PresentationBoundary
      reason: PresentationExclusionReason
    }>

export type IncidentPresentationDecision = Extract<
  PresentationDecision,
  { readonly kind: 'incident' }
>
export type ExcludedPresentationDecision = Extract<
  PresentationDecision,
  { readonly kind: 'excluded' }
>

const receiveOutcomes = new Set<PresentationOutcome>(PRESENTATION_OUTCOMES)
const actionOutcomes = new Set<PresentationOutcome>([
  'failed',
  'restart_required',
  'needs_attention',
])

export function incidentPresentationDecision(
  boundary: PresentationBoundary,
  outcome: PresentationOutcome,
  trigger: FailureFactRef,
): IncidentPresentationDecision {
  if (!isAllowedPresentationOutcome(boundary, outcome)) {
    throw new RangeError(`Presentation outcome ${outcome} is invalid for ${boundary}`)
  }
  if (
    !isKnownFailureFactRef(trigger) ||
    failureFactRelationForRef(trigger) !== 'contributor'
  ) {
    throw new TypeError('Incident presentation decisions require a contributor reference')
  }
  return Object.freeze({ kind: 'incident', boundary, outcome, trigger })
}

export function excludedPresentationDecision(
  boundary: PresentationBoundary,
  reason: PresentationExclusionReason,
): ExcludedPresentationDecision {
  if (!isPresentationBoundary(boundary)) {
    throw new RangeError('Unknown presentation boundary')
  }
  if (!isPresentationExclusionReason(reason)) {
    throw new RangeError('Unknown presentation exclusion reason')
  }
  return Object.freeze({ kind: 'excluded', boundary, reason })
}

export function isPresentationDecision(value: unknown): value is PresentationDecision {
  if (!isRecord(value) || !isPresentationBoundary(value.boundary)) {
    return false
  }
  if (value.kind === 'incident') {
    return (
      hasExactKeys(value, ['kind', 'boundary', 'outcome', 'trigger']) &&
      isPresentationOutcome(value.outcome) &&
      isAllowedPresentationOutcome(value.boundary, value.outcome) &&
      isKnownFailureFactRef(value.trigger) &&
      failureFactRelationForRef(value.trigger) === 'contributor'
    )
  }
  return (
    value.kind === 'excluded' &&
    hasExactKeys(value, ['kind', 'boundary', 'reason']) &&
    isPresentationExclusionReason(value.reason)
  )
}

export function presentationBoundaryForScope(
  scopeKind: IncidentScopeKind,
): PresentationBoundary {
  switch (scopeKind) {
    case 'preview_open':
    case 'preview_seek':
    case 'preview_media':
      return 'preview'
    case 'projection':
    case 'authority_activation':
      return 'projection_authority'
    case 'join':
    case 'browse':
    case 'receive':
    case 'lifecycle_action':
    case 'retained_inventory':
    case 'retained_action':
      return scopeKind
  }
  throw new RangeError('Unknown incident scope kind')
}

export function isAllowedPresentationOutcome(
  boundary: PresentationBoundary,
  outcome: PresentationOutcome,
): boolean {
  switch (boundary) {
    case 'receive':
      return receiveOutcomes.has(outcome)
    case 'lifecycle_action':
    case 'retained_action':
      return actionOutcomes.has(outcome)
    case 'join':
    case 'browse':
    case 'preview':
    case 'projection_authority':
    case 'retained_inventory':
      return outcome === 'failed'
  }
}

function isPresentationBoundary(value: unknown): value is PresentationBoundary {
  return (
    typeof value === 'string' &&
    (PRESENTATION_BOUNDARIES as readonly string[]).includes(value)
  )
}

function isPresentationOutcome(value: unknown): value is PresentationOutcome {
  return (
    typeof value === 'string' &&
    (PRESENTATION_OUTCOMES as readonly string[]).includes(value)
  )
}

function isPresentationExclusionReason(
  value: unknown,
): value is PresentationExclusionReason {
  return (
    typeof value === 'string' &&
    (PRESENTATION_EXCLUSION_REASONS as readonly string[]).includes(value)
  )
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function hasExactKeys(
  value: Record<string, unknown>,
  expected: readonly string[],
): boolean {
  const keys = Object.keys(value)
  return keys.length === expected.length && expected.every((key) => keys.includes(key))
}
