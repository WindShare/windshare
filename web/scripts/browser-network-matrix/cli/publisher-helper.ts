import { randomBytes } from 'node:crypto'
import { basename, dirname, isAbsolute, join, resolve } from 'node:path'

import {
  NETWORK_MATRIX_AGGREGATE_FILENAME,
  NETWORK_MATRIX_RUN_FILENAME,
  type NetworkMatrixArtifactPublication,
  type NetworkMatrixArtifactPublicationInput,
  type NetworkMatrixArtifactPublisher,
} from './atomic-publication.ts'
import type {
  ImmutableTextFilePublication,
  ImmutableTextFilePublisher,
} from './immutable-file-publication.ts'
import {
  openPublisherHelperProcessAuthority,
  type PublisherHelperProcessAuthority,
  type PublisherHelperProcessAuthorityOptions,
} from './publisher-helper-process.ts'
import {
  encodePublisherHelperRequest,
  parsePublisherHelperResponse,
  publisherHelperArtifact,
  requireExactPublishedArtifacts,
  type PublisherHelperArtifact,
  type PublisherHelperFailureCode,
} from './publisher-helper-protocol.ts'

const STAGING_NONCE_PATTERN = /^[a-f0-9]{32}$/u
const UTF8_DECODER = new TextDecoder('utf-8', { fatal: true })

export interface NetworkMatrixPublisherHelperAuthority {
  readonly artifactPublisher: NetworkMatrixArtifactPublisher
  readonly aggregatePublisher: ImmutableTextFilePublisher
  close(): Promise<void>
}

export interface NetworkMatrixPublisherHelperOptions extends PublisherHelperProcessAuthorityOptions {
  readonly createNonce?: () => string
}

export class PublisherHelperPublicationError extends Error {
  readonly failureCode: PublisherHelperFailureCode

  constructor(failureCode: PublisherHelperFailureCode) {
    super(`browser network matrix publisher failed: ${failureCode}`)
    this.name = 'PublisherHelperPublicationError'
    this.failureCode = failureCode
  }
}

export async function openNetworkMatrixPublisherHelper(
  options: NetworkMatrixPublisherHelperOptions,
): Promise<NetworkMatrixPublisherHelperAuthority> {
  const processAuthority = await openPublisherHelperProcessAuthority(options)
  return networkMatrixPublisherHelperFromProcess(
    processAuthority,
    options.createNonce ?? (() => randomBytes(16).toString('hex')),
  )
}

export function networkMatrixPublisherHelperFromProcess(
  processAuthority: PublisherHelperProcessAuthority,
  createNonce: () => string,
): NetworkMatrixPublisherHelperAuthority {
  const client = new PublisherHelperClient(processAuthority, createNonce)
  return Object.freeze({
    artifactPublisher: Object.freeze({
      publish: (input: NetworkMatrixArtifactPublicationInput) => client.publishGeneration(input),
    }),
    aggregatePublisher: Object.freeze({
      publish: (path: string, encoded: string) => client.publishFile(path, encoded),
    }),
    close: () => processAuthority.close(),
  })
}

class PublisherHelperClient {
  readonly #process: PublisherHelperProcessAuthority
  readonly #createNonce: () => string

  constructor(processAuthority: PublisherHelperProcessAuthority, createNonce: () => string) {
    this.#process = processAuthority
    this.#createNonce = createNonce
  }

  async publishGeneration(
    input: NetworkMatrixArtifactPublicationInput,
  ): Promise<NetworkMatrixArtifactPublication> {
    const outputRoot = canonicalOutputPath(input.outputRoot, 'publication output root')
    const run = publisherHelperArtifact(
      NETWORK_MATRIX_RUN_FILENAME,
      Buffer.from(input.runJson, 'utf8'),
    )
    const aggregateJson = input.deriveAggregateJson(input.runJson)
    const aggregate = publisherHelperArtifact(
      NETWORK_MATRIX_AGGREGATE_FILENAME,
      Buffer.from(aggregateJson, 'utf8'),
    )
    const artifacts = [run, aggregate] as const
    const actual = await this.#publish(
      'directory',
      dirname(outputRoot),
      basename(outputRoot),
      artifacts,
    )
    return Object.freeze({
      outputRoot,
      runPath: join(outputRoot, NETWORK_MATRIX_RUN_FILENAME),
      aggregatePath: join(outputRoot, NETWORK_MATRIX_AGGREGATE_FILENAME),
      runJson: decodeArtifact(actual[0] as PublisherHelperArtifact),
      aggregateJson: decodeArtifact(actual[1] as PublisherHelperArtifact),
    })
  }

  async publishFile(pathValue: string, encoded: string): Promise<ImmutableTextFilePublication> {
    const path = canonicalOutputPath(pathValue, 'aggregate output')
    const expected = publisherHelperArtifact(basename(path), Buffer.from(encoded, 'utf8'))
    const actual = await this.#publish('file', dirname(path), basename(path), [expected])
    return Object.freeze({ path, encoded: decodeArtifact(actual[0] as PublisherHelperArtifact) })
  }

  async #publish(
    operation: 'directory' | 'file',
    parentPath: string,
    outputName: string,
    artifacts: readonly PublisherHelperArtifact[],
  ): Promise<readonly PublisherHelperArtifact[]> {
    const request = encodePublisherHelperRequest({
      operation,
      parentPath,
      outputName,
      stagingName: `.network-matrix-stage-${this.#nonce()}`,
      artifacts,
    })
    const terminal = await this.#process.execute(request)
    if (terminal.exitCode === 0) {
      if (terminal.stderr.byteLength !== 0) {
        throw new Error('successful publisher helper wrote failure-channel bytes')
      }
      const response = parsePublisherHelperResponse(terminal.stdout)
      if (response.outcome !== 'completed') {
        throw new Error('successful publisher helper returned a failed response')
      }
      requireExactPublishedArtifacts(artifacts, response.artifacts)
      return response.artifacts
    }
    if (terminal.stdout.byteLength !== 0) {
      throw new Error('failed publisher helper wrote success-channel bytes')
    }
    const response = parsePublisherHelperResponse(terminal.stderr)
    if (response.outcome !== 'failed') {
      throw new Error('failed publisher helper returned a completed response')
    }
    const expectedExitCode = response.failureCode === 'destination-exists' ? 3 : 2
    if (terminal.exitCode !== expectedExitCode) {
      throw new Error('publisher helper exit code contradicts its failure response')
    }
    throw new PublisherHelperPublicationError(response.failureCode)
  }

  #nonce(): string {
    const nonce = this.#createNonce()
    if (!STAGING_NONCE_PATTERN.test(nonce)) {
      throw new Error('publisher helper staging nonce must be 128-bit lowercase hex')
    }
    return nonce
  }
}

function canonicalOutputPath(value: string, label: string): string {
  if (!isAbsolute(value) || resolve(value) !== value || dirname(value) === value ||
      basename(value).length === 0 || value.includes('\0')) {
    throw new Error(`browser network matrix ${label} must be explicit, absolute, and canonical`)
  }
  return value
}

function decodeArtifact(artifact: PublisherHelperArtifact): string {
  try {
    return UTF8_DECODER.decode(artifact.bytes)
  } catch {
    throw new Error('publisher helper returned artifact bytes that are not valid UTF-8')
  }
}
