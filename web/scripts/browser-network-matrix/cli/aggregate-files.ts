import type { BigIntStats } from 'node:fs'
import { lstat, open } from 'node:fs/promises'
import { resolve } from 'node:path'
import {
  aggregateNetworkMatrix,
  canonicalNetworkMatrixAggregateJson,
  type NetworkMatrixAggregate,
} from '../aggregate.ts'
import type { LoadedNetworkMatrixRegistry } from '../manifest.ts'
import { parseNetworkRunResultJson, type NetworkRunResult } from '../result.ts'
import { NETWORK_MATRIX_EXECUTION_MODES } from '../vocabulary.ts'
import { sameFileIdentity } from './filesystem-authority.ts'
import {
  type ImmutableTextFilePublication,
  type ImmutableTextFilePublisher,
} from './immutable-file-publication.ts'

export const MAXIMUM_NETWORK_MATRIX_RUN_BYTES = 8 * 1024 * 1024

const UTF8_DECODER = new TextDecoder('utf-8', { fatal: true })

export interface AggregateNetworkMatrixFilesOptions {
  readonly registry: LoadedNetworkMatrixRegistry
  readonly inputPaths: readonly string[]
  readonly outputPath: string
  readonly publisher: ImmutableTextFilePublisher
}

export interface AggregateNetworkMatrixFilesResult {
  readonly runs: readonly NetworkRunResult[]
  readonly aggregate: NetworkMatrixAggregate
  readonly publication: ImmutableTextFilePublication
}

export async function aggregateNetworkMatrixFiles(
  options: AggregateNetworkMatrixFilesOptions,
): Promise<AggregateNetworkMatrixFilesResult> {
  if (
    options.inputPaths.length < 1 ||
    options.inputPaths.length > NETWORK_MATRIX_EXECUTION_MODES.length
  ) throw new Error('local network matrix aggregate requires one or two explicit run files')
  const paths = options.inputPaths.map((path) => resolve(path))
  if (new Set(paths).size !== paths.length) {
    throw new Error('local network matrix aggregate run file paths must be distinct')
  }
  const runs = Object.freeze(await Promise.all(paths.map(async (path) =>
    parseNetworkRunResultJson(await readStableRunJson(path), options.registry))))
  const aggregate = aggregateNetworkMatrix(options.registry, runs)
  const encoded = canonicalNetworkMatrixAggregateJson(aggregate, options.registry, runs)
  const publication = await options.publisher.publish(options.outputPath, encoded)
  if (publication.encoded !== encoded) {
    throw new Error('network matrix aggregate publisher changed the canonical aggregate bytes')
  }
  return Object.freeze({ runs, aggregate, publication })
}

async function readStableRunJson(path: string): Promise<string> {
  const namedBefore = await lstat(path, { bigint: true })
  requireBoundedRegularFile(namedBefore, path)
  const handle = await open(path, 'r')
  try {
    const openedBefore = await handle.stat({ bigint: true })
    requireBoundedRegularFile(openedBefore, path)
    if (
      !sameFileIdentity(namedBefore, openedBefore) ||
      !sameFileRevision(namedBefore, openedBefore)
    ) throw new Error(`network matrix run file ${path} changed while opened`)
    const bytes = await handle.readFile()
    const [openedAfter, namedAfter] = await Promise.all([
      handle.stat({ bigint: true }),
      lstat(path, { bigint: true }),
    ])
    if (
      !sameFileIdentity(openedBefore, openedAfter) ||
      !sameFileIdentity(openedAfter, namedAfter) ||
      !sameFileRevision(openedBefore, openedAfter) ||
      !sameFileRevision(openedAfter, namedAfter) ||
      BigInt(bytes.length) !== openedAfter.size
    ) throw new Error(`network matrix run file ${path} changed while read`)
    try {
      return UTF8_DECODER.decode(bytes)
    } catch {
      throw new Error(`network matrix run file ${path} is not valid UTF-8`)
    }
  } finally {
    await handle.close().catch(() => undefined)
  }
}

function requireBoundedRegularFile(metadata: BigIntStats, path: string): void {
  if (
    !metadata.isFile() ||
    metadata.size < 1n ||
    metadata.size > BigInt(MAXIMUM_NETWORK_MATRIX_RUN_BYTES)
  ) throw new Error(`network matrix run file ${path} is not a bounded regular file`)
}

function sameFileRevision(left: BigIntStats, right: BigIntStats): boolean {
  return left.size === right.size &&
    left.mtimeNs === right.mtimeNs &&
    left.ctimeNs === right.ctimeNs
}
