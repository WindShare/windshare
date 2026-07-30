import { createHash } from 'node:crypto'
import { join } from 'node:path'
import { tmpdir } from 'node:os'
import { describe, expect, it, vi } from 'vitest'

import {
  networkMatrixPublisherHelperFromProcess,
  PublisherHelperPublicationError,
} from '../../scripts/browser-network-matrix/cli/publisher-helper.ts'
import type {
  PublisherHelperProcessAuthority,
  PublisherHelperProcessResult,
} from '../../scripts/browser-network-matrix/cli/publisher-helper-process.ts'
import {
  encodePublisherHelperRequest,
  encodeExistingDirectoryPublisherRequest,
  parseExistingDirectoryPublisherResponse,
  parsePublisherHelperResponse,
  PUBLISHER_HELPER_SCHEMA_VERSION,
  publisherHelperArtifact,
} from '../../scripts/browser-network-matrix/cli/publisher-helper-protocol.ts'

const NONCE = 'a'.repeat(32)
const RUN_JSON = '{"kind":"real-run"}\n'
const AGGREGATE_JSON = '{"kind":"derived-aggregate"}\n'

describe('native browser network matrix publisher helper client', () => {
  it('binds existing-directory publish requests to the receipt and exact snapshots', () => {
    const encoded = encodeExistingDirectoryPublisherRequest({
      operation: 'publish-existing-directory',
      parentPath: join(tmpdir(), 'sealed-parent'),
      outputName: 'sealed',
      stagingName: '.browser-evidence-upload-00000000000000000000000000000000',
      stagingReceipt: Buffer.from('receipt').toString('base64'),
      inventory: {
        directories: ['samples'],
        files: [{ relativePath: 'manifest.json', byteLength: '2', sha256: 'a'.repeat(64) }],
      },
      manifestPath: 'manifest.json',
      expectedManifestSha256: 'a'.repeat(64),
      snapshotPaths: ['manifest.json'],
    })
    const request = JSON.parse(Buffer.from(encoded).toString('utf8')) as Record<string, unknown>
    expect(request).toMatchObject({
      operation: 'publish-existing-directory',
      outputName: 'sealed',
      stagingReceipt: Buffer.from('receipt').toString('base64'),
      snapshotPaths: ['manifest.json'],
    })
    expect(Buffer.from(encoded).toString('utf8').endsWith('\n')).toBe(false)
  })

  it('parses only the operation-specific native response shape', () => {
    const receipt = Buffer.from('receipt').toString('base64')
    const prepared = existingDirectoryResponse({
      schemaVersion: PUBLISHER_HELPER_SCHEMA_VERSION,
      outcome: 'completed',
      failureCode: null,
      artifacts: [],
      stagingReceipt: receipt,
    })
    expect(parseExistingDirectoryPublisherResponse(
      prepared,
      'prepare-existing-directory',
    )).toEqual({
      outcome: 'completed',
      operation: 'prepare-existing-directory',
      stagingReceipt: receipt,
    })
    expect(() => parseExistingDirectoryPublisherResponse(
      prepared,
      'cleanup-existing-directory',
    )).toThrow(/unknown field|canonical/u)

    const failedPublishWithReceipt = existingDirectoryResponse({
      schemaVersion: PUBLISHER_HELPER_SCHEMA_VERSION,
      outcome: 'failed',
      failureCode: 'publication-unsafe',
      artifacts: [],
      stagingReceipt: receipt,
    })
    expect(() => parseExistingDirectoryPublisherResponse(
      failedPublishWithReceipt,
      'publish-existing-directory',
    )).toThrow(/cross-operation authority/u)
  })

  it('emits the one encoding/json-compatible canonical request value', () => {
    const encoded = encodePublisherHelperRequest({
      operation: 'file',
      parentPath: join(tmpdir(), 'authority&parent'),
      outputName: 'aggregate.json',
      stagingName: `.network-matrix-stage-${NONCE}`,
      artifacts: [publisherHelperArtifact('aggregate.json', Buffer.from(AGGREGATE_JSON))],
    })
    const text = Buffer.from(encoded).toString('utf8')
    expect(text).toContain('authority\\u0026parent')
    expect(text.endsWith('\n')).toBe(false)
  })

  it('accepts only the exact reread artifacts returned by one helper transaction', async () => {
    const process = echoingProcess()
    const authority = networkMatrixPublisherHelperFromProcess(process, () => NONCE)
    const outputRoot = join(tmpdir(), 'windshare-helper-generation')

    const generation = await authority.artifactPublisher.publish({
      outputRoot,
      runJson: RUN_JSON,
      deriveAggregateJson: (run) => {
        expect(run).toBe(RUN_JSON)
        return AGGREGATE_JSON
      },
    })
    const aggregatePath = join(tmpdir(), 'windshare-helper-aggregate.json')
    const aggregate = await authority.aggregatePublisher.publish(aggregatePath, AGGREGATE_JSON)
    await authority.close()

    expect(generation).toMatchObject({ outputRoot, runJson: RUN_JSON, aggregateJson: AGGREGATE_JSON })
    expect(aggregate).toEqual({ path: aggregatePath, encoded: AGGREGATE_JSON })
    expect(process.execute).toHaveBeenCalledTimes(2)
    const request = decodeRequest(process.execute.mock.calls[0]?.[0])
    expect(request).toMatchObject({
      operation: 'directory',
      parentPath: tmpdir(),
      outputName: 'windshare-helper-generation',
      stagingName: `.network-matrix-stage-${NONCE}`,
    })
    expect(process.close).toHaveBeenCalledOnce()
  })

  it('maps a canonical collision response without accepting contradictory exit status', async () => {
    const collision = processReturning(3, new Uint8Array(), failureResponse('destination-exists'))
    const authority = networkMatrixPublisherHelperFromProcess(collision, () => NONCE)
    await expect(authority.aggregatePublisher.publish(
      join(tmpdir(), 'already-exists.json'),
      AGGREGATE_JSON,
    )).rejects.toBeInstanceOf(PublisherHelperPublicationError)

    const contradictory = networkMatrixPublisherHelperFromProcess(
      processReturning(2, new Uint8Array(), failureResponse('destination-exists')),
      () => NONCE,
    )
    await expect(contradictory.aggregatePublisher.publish(
      join(tmpdir(), 'contradictory.json'),
      AGGREGATE_JSON,
    )).rejects.toThrow(/exit code contradicts/u)
  })

  it.each(['crashed', 'response deadline exceeded'])(
    'fails before fabricating publication when the helper %s',
    async (message) => {
      const process = rejectingProcess(new Error(message))
      const authority = networkMatrixPublisherHelperFromProcess(process, () => NONCE)
      await expect(authority.aggregatePublisher.publish(
        join(tmpdir(), `${message.replaceAll(' ', '-')}.json`),
        AGGREGATE_JSON,
      )).rejects.toThrow(message)
      expect(process.execute).toHaveBeenCalledOnce()
    },
  )

  it('rejects trailing, duplicate, and byte-inconsistent helper responses', () => {
    const aggregateBytes = Buffer.from(AGGREGATE_JSON)
    const valid = completedResponse([{
      name: 'aggregate.json',
      bytesBase64: aggregateBytes.toString('base64'),
      sha256: createHash('sha256').update(aggregateBytes).digest('hex'),
    }])
    expect(parsePublisherHelperResponse(valid)).toMatchObject({ outcome: 'completed' })
    expect(() => parsePublisherHelperResponse(
      Buffer.concat([valid, Buffer.from('\n')]),
    )).toThrow(/not canonical JSON/u)
    const completed = completedResponse([{
      name: 'aggregate.json',
      bytesBase64: Buffer.from(AGGREGATE_JSON).toString('base64'),
      sha256: '0'.repeat(64),
    }])
    expect(() => parsePublisherHelperResponse(Buffer.concat([completed, Buffer.from('{}')]))).toThrow()
    expect(() => parsePublisherHelperResponse(completed)).toThrow(/digest does not bind/u)
    const duplicate = Buffer.from(
      `{"schemaVersion":"${PUBLISHER_HELPER_SCHEMA_VERSION}",`
      + `"schemaVersion":"${PUBLISHER_HELPER_SCHEMA_VERSION}"}\n`,
    )
    expect(() => parsePublisherHelperResponse(duplicate)).toThrow(/duplicate/u)
  })
})

type RequestArtifact = {
  readonly name: string
  readonly bytesBase64: string
  readonly sha256: string
}

type DecodedRequest = {
  readonly operation: string
  readonly parentPath: string
  readonly outputName: string
  readonly stagingName: string
  readonly artifacts: readonly RequestArtifact[]
}

type MockProcess = PublisherHelperProcessAuthority & {
  readonly execute: ReturnType<typeof vi.fn<(request: Uint8Array) => Promise<PublisherHelperProcessResult>>>
  readonly close: ReturnType<typeof vi.fn<() => Promise<void>>>
}

function echoingProcess(): MockProcess {
  const execute = vi.fn(async (requestBytes: Uint8Array) => {
    const request = decodeRequest(requestBytes)
    return Object.freeze({
      exitCode: 0,
      stdout: completedResponse(request.artifacts),
      stderr: new Uint8Array(),
    })
  })
  return Object.freeze({ execute, close: vi.fn(async () => undefined) })
}

function processReturning(
  exitCode: number,
  stdout: Uint8Array,
  stderr: Uint8Array,
): MockProcess {
  return Object.freeze({
    execute: vi.fn(async () => Object.freeze({ exitCode, stdout, stderr })),
    close: vi.fn(async () => undefined),
  })
}

function rejectingProcess(cause: Error): MockProcess {
  return Object.freeze({
    execute: vi.fn(async () => Promise.reject(cause)),
    close: vi.fn(async () => undefined),
  })
}

function decodeRequest(encoded: Uint8Array | undefined): DecodedRequest {
  if (encoded === undefined) throw new Error('publisher request is absent')
  return JSON.parse(Buffer.from(encoded).toString('utf8')) as DecodedRequest
}

function completedResponse(artifacts: readonly RequestArtifact[]): Uint8Array {
  return Buffer.from(`${JSON.stringify({
    schemaVersion: PUBLISHER_HELPER_SCHEMA_VERSION,
    outcome: 'completed',
    failureCode: null,
    artifacts,
  })}\n`)
}

function failureResponse(failureCode: string): Uint8Array {
  return Buffer.from(`${JSON.stringify({
    schemaVersion: PUBLISHER_HELPER_SCHEMA_VERSION,
    outcome: 'failed',
    failureCode,
    artifacts: [],
  })}\n`)
}

function existingDirectoryResponse(value: Readonly<Record<string, unknown>>): Uint8Array {
  return Buffer.from(`${JSON.stringify(value)}\n`, 'utf8')
}
