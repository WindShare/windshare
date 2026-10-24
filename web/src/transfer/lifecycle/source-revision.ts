export interface SourceRevisionFailure {
  readonly entryId: string
  readonly path: readonly string[]
  readonly sourcePath: readonly string[]
}

export interface SourceRevisionFailures {
  readonly shareInstance: string
  readonly count: bigint
  readonly files: readonly SourceRevisionFailure[]
}
