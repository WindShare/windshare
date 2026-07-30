export interface FinalSemanticEvaluationInput {
  readonly result: unknown
  readonly topologyProfileJson: string
  readonly topologyResolutionJson: string
  readonly topologyProfileSha256: string
  readonly topologyResolutionSha256: string
}

export interface FinalSemanticEvaluation {
  readonly result: Readonly<Record<string, unknown>>
  readonly disposition: 'accepted' | 'applicable' | 'not-applicable'
}

export interface FinalGuardUploadManifest {
  readonly schemaVersion: 2
  readonly runId: string
  readonly runPolicy: Readonly<Record<string, unknown>>
  readonly suite: 'main' | 'pion'
  readonly checkoutSha: string
  readonly topology: Readonly<Record<string, unknown>>
  readonly samples: readonly Readonly<Record<string, unknown>>[]
}

export function parseFinalGuardUploadManifest(encoded: string): FinalGuardUploadManifest

export function evaluateFinalBrowserSample(
  input: FinalSemanticEvaluationInput,
): Promise<FinalSemanticEvaluation>
