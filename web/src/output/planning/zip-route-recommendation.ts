import type {
  OfferedArtifactChoice,
  WorkspaceCostObservationV1,
  ZipRouteGroup,
  ZipRouteRecommendationPolicyV1,
} from './contracts'
import { canonicalDigest, canonicalFrame, canonicalRecord, canonicalU64 } from '../workspace/canonical'

const RECOMMENDATION_POLICY_DOMAIN = 'windshare/zip-route-recommendation-policy/v1'

export async function zipRouteRecommendationPolicyDigestV1(
  workspacePeakBytesThreshold: bigint,
): Promise<string> {
  return canonicalDigest(canonicalRecord(RECOMMENDATION_POLICY_DOMAIN, 1, [
    canonicalFrame(canonicalU64(workspacePeakBytesThreshold)),
  ]))
}

export interface ZipRouteRecommendationInput {
  readonly direct: OfferedArtifactChoice | null
  readonly workspace: OfferedArtifactChoice | null
  readonly portable: OfferedArtifactChoice | null
  readonly discoveryComplete: boolean
  readonly workspaceCost: WorkspaceCostObservationV1 | null
  readonly policy: ZipRouteRecommendationPolicyV1
}

/** Pure ranking cannot create the discovery or authority facts it consumes. */
export function recommendZipRoutes(input: ZipRouteRecommendationInput): ZipRouteGroup | null {
  const main = [input.direct, input.workspace].filter(
    (choice): choice is OfferedArtifactChoice => choice !== null,
  )
  if (main.length === 0 && input.portable === null) return null
  if (main.length === 0) {
    return group(input.portable!, null, 'only-one-route-available')
  }
  if (main.length === 1) return group(main[0]!, input.portable, 'only-one-route-available')

  const direct = input.direct!
  const workspace = input.workspace!
  if (input.policy.kind === 'unavailable') {
    return group(direct, workspace, 'recommendation-policy-unavailable')
  }
  if (!input.discoveryComplete) return group(direct, workspace, 'discovery-incomplete')
  if (input.workspaceCost === null) {
    return recommended(direct, workspace, 'direct-unknown-or-over-budget')
  }
  return input.workspaceCost.peakOwnedBytes <= input.policy.workspacePeakBytesThreshold
    ? recommended(workspace, direct, 'workspace-within-reviewed-budget')
    : recommended(direct, workspace, 'direct-unknown-or-over-budget')
}

export function nativeZipFallback(): ZipRouteGroup['fallback'] {
  return Object.freeze({
    kind: 'native-recommended',
    reason: 'no-supported-browser-zip-route',
  })
}

function recommended(
  primary: OfferedArtifactChoice,
  secondary: OfferedArtifactChoice,
  reason: Extract<ZipRouteGroup['recommendation'], { kind: 'recommended' }>['reason'],
): ZipRouteGroup {
  return Object.freeze({
    kind: 'zip-route-group',
    primary: withImportance(primary, 'primary'),
    secondary: withImportance(secondary, 'secondary'),
    recommendation: Object.freeze({ kind: 'recommended', choiceId: primary.choice.choiceId, reason }),
    fallback: Object.freeze({ kind: 'none' }),
  })
}

function group(
  primary: OfferedArtifactChoice,
  secondary: OfferedArtifactChoice | null,
  reason: Extract<ZipRouteGroup['recommendation'], { kind: 'no-recommendation' }>['reason'],
): ZipRouteGroup {
  return Object.freeze({
    kind: 'zip-route-group',
    primary: withImportance(primary, 'primary'),
    secondary: secondary === null ? null : withImportance(secondary, 'secondary'),
    recommendation: Object.freeze({ kind: 'no-recommendation', reason }),
    fallback: Object.freeze({ kind: 'none' }),
  })
}

function withImportance(
  choice: OfferedArtifactChoice,
  importance: OfferedArtifactChoice['importance'],
): OfferedArtifactChoice {
  return choice.importance === importance ? choice : Object.freeze({ ...choice, importance })
}
