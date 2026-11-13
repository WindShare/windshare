export type ProjectedByteCount =
  | Readonly<{ kind: 'exact'; bytes: bigint }>
  | Readonly<{ kind: 'estimated-lower-bound'; bytes: bigint }>

export interface ArtifactSizeProjection {
  readonly raw: ProjectedByteCount
  readonly artifact: ProjectedByteCount
}
