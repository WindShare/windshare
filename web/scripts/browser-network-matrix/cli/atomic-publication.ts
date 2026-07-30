export const NETWORK_MATRIX_RUN_FILENAME = 'run.json' as const
export const NETWORK_MATRIX_AGGREGATE_FILENAME = 'aggregate.json' as const

export interface NetworkMatrixArtifactPublicationInput {
  readonly outputRoot: string
  readonly runJson: string
  readonly deriveAggregateJson: (publishedRunJson: string) => string
}

export interface NetworkMatrixArtifactPublication {
  readonly outputRoot: string
  readonly runPath: string
  readonly aggregatePath: string
  readonly runJson: string
  readonly aggregateJson: string
}

/** Publication is delegated to the explicit native helper authority. */
export interface NetworkMatrixArtifactPublisher {
  publish(input: NetworkMatrixArtifactPublicationInput): Promise<NetworkMatrixArtifactPublication>
}
