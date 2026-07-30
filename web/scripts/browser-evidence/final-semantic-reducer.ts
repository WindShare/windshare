import {
  parseBrowserSampleResult,
  validateMainAcceptance,
  validatePionAcceptance,
  type BrowserSampleResult,
  type PionAcceptanceDisposition,
} from './result.ts'
import {
  parseTestIceTopologyJson,
  parseTestIceTopologyResolutionJson,
  verifyTestIceTopologyLock,
} from './test-ice-topology.ts'
import {
  parseGuardUploadManifest,
  type GuardUploadManifest,
} from './artifact/sealed-suite.ts'

export interface FinalSemanticEvaluationInput {
  readonly result: unknown
  readonly topologyProfileJson: string
  readonly topologyResolutionJson: string
  readonly topologyProfileSha256: string
  readonly topologyResolutionSha256: string
}

export interface FinalSemanticEvaluation {
  readonly result: BrowserSampleResult
  readonly disposition: 'accepted' | PionAcceptanceDisposition
}

/** The dependency-free workflow bundle consumes the producer's exact v2 parser. */
export function parseFinalGuardUploadManifest(encoded: string): GuardUploadManifest {
  return parseGuardUploadManifest(encoded)
}

/**
 * Both the guard-side adapter and the dependency-free workflow reducer call
 * this exact boundary. Bundling this module commits one semantic authority
 * while retaining the typed producer parsers as its source of truth.
 */
export async function evaluateFinalBrowserSample(
  input: FinalSemanticEvaluationInput,
): Promise<FinalSemanticEvaluation> {
  const profile = parseTestIceTopologyJson(input.topologyProfileJson)
  const resolution = parseTestIceTopologyResolutionJson(
    input.topologyResolutionJson,
    profile,
    input.topologyProfileSha256,
  )
  const topologyLock = await verifyTestIceTopologyLock(
    profile,
    resolution,
    input.topologyProfileSha256,
    input.topologyResolutionSha256,
  )
  const result = parseBrowserSampleResult(input.result, topologyLock)
  const disposition = result.suite === 'main'
    ? (validateMainAcceptance(result, topologyLock), 'accepted' as const)
    : validatePionAcceptance(result)
  return Object.freeze({ result, disposition })
}
