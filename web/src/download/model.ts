import type {
  ByteLength,
  CanonicalPath,
  ChunkIndex,
  TransferPlan,
} from '../contracts'

/** C1 injects its pinned path-policy validator; C3 never guesses with locale APIs. */
export type CanonicalPathValidator = (path: string) => CanonicalPath

export interface BlockFileRange {
  readonly path: CanonicalPath
  readonly offset: ByteLength
  readonly length: ByteLength
}

/** The narrow geometry projection C3 consumes without owning C1's layout. */
export interface BlockLayout {
  chunkRanges(index: ChunkIndex): readonly BlockFileRange[]
}

export interface DownloadSinkContext {
  readonly plan: TransferPlan
  readonly layout: BlockLayout
  readonly validatePath: CanonicalPathValidator
}
