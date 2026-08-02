import { createHash, generateKeyPairSync, sign, type KeyObject } from 'node:crypto'
import { mkdtemp, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join, resolve } from 'node:path'

import { describe, expect, it, vi } from 'vitest'

import {
  EXTERNAL_FIXTURE_CREDENTIAL_BROKER_PROTOCOL,
  ProcessExternalFixtureControlCredentialAuthority,
  type CredentialBrokerPipeExchange,
} from '../../scripts/browser-network-matrix/linux-topology/control-credential-broker.ts'
import {
  EXTERNAL_FIXTURE_CONTROL_PROTOCOL,
  EXTERNAL_FIXTURE_SIGNATURE_ALGORITHM,
} from '../../scripts/browser-network-matrix/linux-topology/external-fixture-attestation.ts'
import {
  PARENT_WORKLOAD_IDENTITY_PROTOCOL,
  type ParentWorkloadIdentityAuthority,
} from '../../scripts/browser-network-matrix/linux-topology/parent-workload-identity.ts'
import { CredentialBrokerProcessOwner } from '../../scripts/browser-network-matrix/linux-topology/credential-broker/process-owner.ts'
import { requireCompleteLinuxTopologyTrace } from '../../scripts/browser-network-matrix/linux-topology/trace/index.ts'
import {
  EXTERNAL_FIXTURE_CONFIG_SCHEMA,
  type NetworkMatrixExternalFixtureConfig,
} from '../../scripts/browser-network-matrix/linux-topology/concrete-runtime-config.ts'
import {
  NETWORK_MATRIX_CONTROL_AUTHORITY_SCHEMA,
  NETWORK_MATRIX_SAMPLE_AUTHORITY_SCHEMA,
  type NetworkMatrixSampleAuthority,
} from '../../scripts/browser-network-matrix/sample-authority.ts'

const RUN_ID = 'run-alpha'
const PROFILE_ID = 'scheduled-public-stun' as const
const PROBE_NONCE = 'probe_nonce_1234567890123456'
const LEASE_ID = 'lease_id_1234567890123456'
const AUTHORITY_ID = 'fixture-alpha'
const CREDENTIAL = Buffer.from('credential_12345678901234567890123456789012', 'ascii')
const ATTESTATION_SHA256 = 'a'.repeat(64)
const LEASE_ISSUED_AT = new Date(Date.now() - 1_000).toISOString()
const LEASE_EXPIRES_AT = new Date(Date.now() + 60_000).toISOString()
const SAMPLE_AUTHORITY: NetworkMatrixSampleAuthority = Object.freeze({
  schemaVersion: NETWORK_MATRIX_SAMPLE_AUTHORITY_SCHEMA,
  runId: RUN_ID,
  profileId: PROFILE_ID,
  browser: 'chromium',
  sampleOrdinal: 1,
  processInstanceId: 'credential-broker-process',
  operationId: 'credential-broker-operation',
})

interface FakePipeInput {
  readonly metadata: Record<string, unknown>
  readonly stdin: Uint8Array
  readonly signal: AbortSignal
}

type FakePipeHandler = (input: FakePipeInput) => Promise<Uint8Array>

describe('process external fixture credential broker ownership', () => {
  it('retries a failed release with the exact request and closes identity only after revocation', async () => {
    const fixture = await createFixture()
    const identity = new FakeIdentity()
    const releaseFrames: Buffer[] = []
    let releaseAttempts = 0
    const pipe = new FakePipe(async ({ metadata, stdin }) => {
      if (metadata.operation === 'acquire') return fixture.leaseResponse(metadata)
      if (metadata.operation === 'release') {
        releaseFrames.push(Buffer.from(stdin))
        releaseAttempts += 1
        if (releaseAttempts === 1) throw new Error('lost release response')
        return fixture.retirementResponse(metadata)
      }
      throw new Error('unexpected broker operation')
    })
    const authority = fixture.authority(identity, pipe)

    const lease = await authority.acquire(scope())
    await expect(lease.release()).rejects.toThrow('lost release response')
    expect(identity.closeCalls).toBe(0)
    await expect(lease.release()).resolves.toMatchObject({
      receipt: {
        operation: 'release',
        controlAuthority: { controlLeaseId: LEASE_ID },
        terminal: 'revoked',
      },
    })
    expect(releaseFrames).toHaveLength(2)
    expect(releaseFrames[0]).toEqual(releaseFrames[1])
    releaseFrames.forEach((frame) => frame.fill(0))
    lease.credential.fill(0)

    await expect(authority.closeAndWait()).resolves.toEqual({ terminal: 'closed' })
    expect(identity.closeCalls).toBe(1)
    expect(identity.forceCalls).toBe(0)
    expect(authority.traces).not.toHaveProperty('append')
    const traces = authority.traces.snapshot()
    requireCompleteLinuxTopologyTrace(traces)
    expect(traces.events.filter(({ milestone }) => milestone === 'credential-broker-started'))
      .toHaveLength(3)
    expect(traces.events.filter(({ milestone }) => milestone === 'credential-broker-terminal'))
      .toHaveLength(3)
    expect(traces.events.at(-1)).toMatchObject({
      milestone: 'credential-broker-terminal',
      outcome: 'succeeded',
      context: {
        cleanupOutcome: 'not-required',
        lastMilestone: 'broker-response-accepted',
      },
    })
  })

  it('returns one complete pull-only channel for each helper exchange', async () => {
    const fixture = await createFixture()
    const identity = new FakeIdentity()
    const owner = new CredentialBrokerProcessOwner({
      helperPath: resolve('testdata', 'credential-broker-client.exe'),
      workingDirectory: resolve('.'),
      platform: 'win32',
      processOwner: Object.freeze({
        path: resolve('testdata', 'testprocessowner.exe'),
        byteLength: 1,
        sha256: 'c'.repeat(64),
      }),
      config: fixture.config,
      workloadIdentity: identity,
      pipeExchange: {
        exchange: async () => Uint8Array.from([1, 2, 3]),
      },
    })

    expect(owner.exchange).toHaveLength(3)
    const throwingLegacyCallback = vi.fn(() => {
      throw new Error('legacy callback must not execute')
    })
    const stallingLegacyCallback = vi.fn(() => new Promise<void>(() => undefined))
    const legacyExchange = owner.exchange as unknown as (...input: readonly unknown[]) => unknown
    for (const callback of [throwingLegacyCallback, stallingLegacyCallback]) {
      expect(() => legacyExchange(
        Object.freeze({ operation: 'legacy-exchange' }),
        Object.freeze({ sampleAuthority: SAMPLE_AUTHORITY, probeNonce: PROBE_NONCE }),
        new AbortController().signal,
        callback,
      )).toThrow('does not accept callback authority')
      expect(callback).not.toHaveBeenCalled()
    }

    const aborted = new AbortController()
    aborted.abort()
    const notDispatched = owner.exchange(
      Object.freeze({ operation: 'aborted-exchange' }),
      Object.freeze({ sampleAuthority: SAMPLE_AUTHORITY, probeNonce: PROBE_NONCE }),
      aborted.signal,
    )
    await expect(notDispatched.result).rejects.toThrow('terminated')
    await expect(notDispatched.dispatchOutcome).resolves.toBe('not-dispatched')

    const execution = owner.exchange(
      Object.freeze({ operation: 'test-exchange' }),
      Object.freeze({ sampleAuthority: SAMPLE_AUTHORITY, probeNonce: PROBE_NONCE }),
      new AbortController().signal,
    )
    expect(execution.traces).not.toHaveProperty('append')
    await expect(execution.dispatchOutcome).resolves.toBe('dispatched')

    const response = await execution.result
    expect([...response]).toEqual([1, 2, 3])
    response.fill(0)
    const operationTrace = execution.traces.snapshot()
    requireCompleteLinuxTopologyTrace(operationTrace)
    expect(operationTrace.events.at(-1)).toMatchObject({
      milestone: 'credential-broker-terminal',
      outcome: 'succeeded',
      context: {
        cleanupOutcome: 'not-required',
        lastMilestone: 'broker-response-accepted',
      },
    })

    expect(owner.traces.snapshot().completed).toBe(false)
    await owner.closeIdentity(false)
    requireCompleteLinuxTopologyTrace(owner.traces.snapshot())
  })

  it('fences close against an in-flight acquire and replays the exact request before revoking', async () => {
    const fixture = await createFixture()
    const identity = new FakeIdentity()
    let releaseFirstDispatch: (() => void) | undefined
    const firstDispatched = new Promise<void>((resolveDispatch) => {
      releaseFirstDispatch = resolveDispatch
    })
    let releaseFirstResponse: (() => void) | undefined
    const firstResponse = new Promise<void>((resolveResponse) => {
      releaseFirstResponse = resolveResponse
    })
    const acquireFrames: Buffer[] = []
    const responseFrames: Buffer[] = []
    let acquireAttempts = 0
    const pipe = new FakePipe(async ({ metadata, stdin }) => {
      if (metadata.operation === 'acquire') {
        acquireAttempts += 1
        acquireFrames.push(Buffer.from(stdin))
        if (acquireAttempts === 1) {
          releaseFirstDispatch?.()
          await firstResponse
        }
        const response = fixture.leaseResponse(metadata)
        responseFrames.push(response)
        return response
      }
      if (metadata.operation === 'revoke-and-wait') {
        return fixture.retirementResponse(metadata)
      }
      throw new Error('unexpected broker operation')
    })
    const authority = fixture.authority(identity, pipe)

    const acquisition = authority.acquire(scope())
    await firstDispatched
    const closing = authority.closeAndWait()
    releaseFirstResponse?.()

    await expect(acquisition).rejects.toThrow('terminated')
    await expect(closing).resolves.toEqual({ terminal: 'closed' })
    expect(acquireFrames).toHaveLength(2)
    expect(acquireFrames[0]).toEqual(acquireFrames[1])
    expect(responseFrames.every((frame) => frame.every((byte) => byte === 0))).toBe(true)
    acquireFrames.forEach((frame) => frame.fill(0))
    expect(identity.closeCalls).toBe(1)
  })

  it('retains workload identity after failed force cleanup and retries one canonical revoke', async () => {
    const fixture = await createFixture()
    const identity = new FakeIdentity()
    const revokeFrames: Buffer[] = []
    let revokeAttempts = 0
    const pipe = new FakePipe(async ({ metadata, stdin }) => {
      if (metadata.operation === 'acquire') return fixture.leaseResponse(metadata)
      if (metadata.operation === 'revoke-and-wait') {
        revokeFrames.push(Buffer.from(stdin))
        revokeAttempts += 1
        if (revokeAttempts === 1) throw new Error('ambiguous revoke')
        return fixture.retirementResponse(metadata)
      }
      throw new Error('unexpected broker operation')
    })
    const authority = fixture.authority(identity, pipe)
    const lease = await authority.acquire(scope())

    await expect(authority.forceTerminateAndWait()).rejects.toThrow('did not settle ownership')
    expect(identity.closeCalls).toBe(0)
    expect(identity.forceCalls).toBe(0)
    await expect(authority.forceTerminateAndWait()).resolves.toEqual({ terminal: 'closed' })
    expect(revokeFrames).toHaveLength(2)
    expect(revokeFrames[0]).toEqual(revokeFrames[1])
    revokeFrames.forEach((frame) => frame.fill(0))
    lease.credential.fill(0)
    expect(identity.forceCalls).toBe(1)
  })

  it('lets force settlement preempt a hung graceful identity close', async () => {
    const fixture = await createFixture()
    const identity = new HangingCloseIdentity()
    const pipe = new FakePipe(async () => {
      throw new Error('a closed empty broker must not dispatch a helper')
    })
    const authority = fixture.authority(identity, pipe)

    const graceful = authority.closeAndWait()
    await identity.closeStarted
    const forcedReceipt = await authority.forceTerminateAndWait()
    const gracefulReceipt = await graceful

    expect(identity.closeCalls).toBe(1)
    expect(identity.forceCalls).toBe(1)
    expect(gracefulReceipt).toBe(forcedReceipt)
  })

  it('aborts a pending release and dispatches its fixed revoke without queueing behind it', async () => {
    const fixture = await createFixture()
    const identity = new FakeIdentity()
    let markReleaseStarted: (() => void) | undefined
    const releaseStarted = new Promise<void>((resolve) => { markReleaseStarted = resolve })
    let revokeDispatched = false
    const pipe = new FakePipe(async ({ metadata, signal }) => {
      if (metadata.operation === 'acquire') return fixture.leaseResponse(metadata)
      if (metadata.operation === 'release') {
        markReleaseStarted?.()
        return new Promise<Uint8Array>((_resolve, reject) => {
          signal.addEventListener('abort', () => reject(new Error('release aborted')), { once: true })
        })
      }
      if (metadata.operation === 'revoke-and-wait') {
        revokeDispatched = true
        return fixture.retirementResponse(metadata)
      }
      throw new Error('unexpected broker operation')
    })
    const authority = fixture.authority(identity, pipe)
    const lease = await authority.acquire(scope())

    const release = lease.release()
    await releaseStarted
    const receipt = await lease.revokeAndWait()

    expect(revokeDispatched).toBe(true)
    await expect(release).rejects.toThrow('release aborted')
    expect(receipt).toMatchObject({
      receipt: {
        operation: 'revoke-and-wait',
        controlAuthority: { controlLeaseId: LEASE_ID },
        terminal: 'revoked',
      },
    })
    lease.credential.fill(0)
  })

  it('quarantines a signed lease-ID collision by its full scope instead of aliasing prior ownership', async () => {
    const fixture = await createFixture()
    const identity = new FakeIdentity()
    const retirements: Record<string, unknown>[] = []
    const pipe = new FakePipe(async ({ metadata }) => {
      if (metadata.operation === 'acquire') return fixture.leaseResponse(metadata)
      if (metadata.operation === 'release' || metadata.operation === 'revoke-and-wait') {
        retirements.push(metadata)
        return fixture.retirementResponse(metadata)
      }
      throw new Error('unexpected broker operation')
    })
    const authority = fixture.authority(identity, pipe)
    const first = await authority.acquire(scope())
    const crossedNonce = 'crossed_nonce_1234567890123456'

    await expect(authority.acquire({ ...scope(), probeNonce: crossedNonce }))
      .rejects.toThrow('one-shot request scope')
    expect(retirements).toHaveLength(1)
    expect(retirements[0]).toMatchObject({
      operation: 'revoke-and-wait',
      controlAuthority: { controlLeaseId: LEASE_ID },
      probeNonce: crossedNonce,
    })

    await first.release()
    first.credential.fill(0)
    expect(retirements).toHaveLength(2)
    expect(retirements[1]).toMatchObject({
      operation: 'release',
      controlAuthority: { controlLeaseId: LEASE_ID },
      probeNonce: PROBE_NONCE,
    })
    expect(retirements[0]?.requestId).not.toBe(retirements[1]?.requestId)
    await authority.closeAndWait()
  })
})

class FakeIdentity implements ParentWorkloadIdentityAuthority {
  readonly binding = Object.freeze({
    protocolVersion: PARENT_WORKLOAD_IDENTITY_PROTOCOL,
    kind: 'github-actions-oidc' as const,
    audience: 'windshare-browser-matrix',
    issuer: 'https://token.actions.githubusercontent.com',
    repository: 'owner/repository',
    ref: 'refs/heads/main',
    workflowRef: 'owner/repository/.github/workflows/browser-network-matrix.yml@refs/heads/main',
    requestOrigin: 'https://pipelines.actions.githubusercontent.com',
    requestPath: '/oidc/token',
    requestQuery: '?api-version=2.0',
  })
  closeCalls = 0
  forceCalls = 0

  async issue(): Promise<Uint8Array> {
    return Buffer.from('aaa.bbb.ccc', 'ascii')
  }

  async closeAndWait(): Promise<{ readonly terminal: 'closed' }> {
    this.closeCalls += 1
    return Object.freeze({ terminal: 'closed' })
  }

  async forceTerminateAndWait(): Promise<{ readonly terminal: 'closed' }> {
    this.forceCalls += 1
    return Object.freeze({ terminal: 'closed' })
  }
}

class HangingCloseIdentity extends FakeIdentity {
  readonly closeStarted: Promise<void>
  #markCloseStarted: (() => void) | undefined
  #finishClose: (() => void) | undefined

  constructor() {
    super()
    this.closeStarted = new Promise<void>((resolve) => { this.#markCloseStarted = resolve })
  }

  override closeAndWait(): Promise<{ readonly terminal: 'closed' }> {
    this.closeCalls += 1
    this.#markCloseStarted?.()
    this.#markCloseStarted = undefined
    return new Promise((resolve) => {
      this.#finishClose = () => resolve(Object.freeze({ terminal: 'closed' }))
    })
  }

  override async forceTerminateAndWait(): Promise<{ readonly terminal: 'closed' }> {
    this.forceCalls += 1
    this.#finishClose?.()
    this.#finishClose = undefined
    return Object.freeze({ terminal: 'closed' })
  }
}

class FakePipe implements CredentialBrokerPipeExchange {
  readonly #handler: FakePipeHandler

  constructor(handler: FakePipeHandler) {
    this.#handler = handler
  }

  async exchange(input: {
    readonly stdin: Uint8Array
    readonly signal: AbortSignal
  }): Promise<Uint8Array> {
    return this.#handler({ metadata: parseFrameMetadata(input.stdin), ...input })
  }
}

async function createFixture(): Promise<{
  readonly config: NetworkMatrixExternalFixtureConfig
  readonly authority: (
    identity: ParentWorkloadIdentityAuthority,
    pipe: CredentialBrokerPipeExchange,
  ) => ProcessExternalFixtureControlCredentialAuthority
  readonly leaseResponse: (request: Record<string, unknown>) => Buffer
  readonly retirementResponse: (request: Record<string, unknown>) => Buffer
}> {
  const { privateKey, publicKey } = generateKeyPairSync('ed25519')
  const root = await mkdtemp(join(tmpdir(), 'windshare-credential-broker-'))
  const publicKeyFile = resolve(root, 'attestation-public-key.pem')
  const caFile = resolve(root, 'controller-ca.pem')
  await writeFile(publicKeyFile, publicKey.export({ type: 'spki', format: 'pem' }))
  await writeFile(caFile, 'test-only CA')
  const config: NetworkMatrixExternalFixtureConfig = Object.freeze({
    schemaVersion: EXTERNAL_FIXTURE_CONFIG_SCHEMA,
    publicStun: Object.freeze({
      control: Object.freeze({
        controllerOrigin: 'https://controller.example.test/',
        tlsCertificateSha256: 'b'.repeat(64),
        tlsCertificateAuthorityFile: caFile,
        attestationPublicKeyFile: publicKeyFile,
      }),
    }),
    restrictedUdp: null,
    coturn: null,
  })
  return Object.freeze({
    config,
    authority: (
      identity: ParentWorkloadIdentityAuthority,
      pipe: CredentialBrokerPipeExchange,
    ) => ProcessExternalFixtureControlCredentialAuthority.createTestHarness({
      helperPath: resolve(root, 'broker-client.exe'),
      workingDirectory: root,
      platform: 'win32',
      processOwner: Object.freeze({
        path: resolve(root, 'testprocessowner.exe'),
        byteLength: 1,
        sha256: 'c'.repeat(64),
      }),
      config,
      workloadIdentity: identity,
      pipeExchange: pipe,
    }),
    leaseResponse: (request: Record<string, unknown>) => leaseResponse(privateKey, request),
    retirementResponse: (request: Record<string, unknown>) =>
      retirementResponse(privateKey, request),
  })
}

function scope(): {
  readonly sampleAuthority: NetworkMatrixSampleAuthority
  readonly probeNonce: string
  readonly signal: AbortSignal
} {
  return Object.freeze({
    sampleAuthority: SAMPLE_AUTHORITY,
    probeNonce: PROBE_NONCE,
    signal: new AbortController().signal,
  })
}

function leaseResponse(privateKey: KeyObject, request: Record<string, unknown>): Buffer {
  const controlAuthority = Object.freeze({
    schemaVersion: NETWORK_MATRIX_CONTROL_AUTHORITY_SCHEMA,
    sampleAuthority: request.sampleAuthority,
    controlLeaseId: LEASE_ID,
  })
  const lease = Object.freeze({
    protocolVersion: EXTERNAL_FIXTURE_CONTROL_PROTOCOL,
    requestId: request.requestId,
    releaseRequestId: request.releaseRequestId,
    revokeRequestId: request.revokeRequestId,
    controlAuthority,
    probeNonce: request.probeNonce,
    authorityInstanceId: AUTHORITY_ID,
    attestationSha256: ATTESTATION_SHA256,
    issuedAt: LEASE_ISSUED_AT,
    expiresAt: LEASE_EXPIRES_AT,
    maxAttempts: 1,
    credentialByteLength: CREDENTIAL.byteLength,
    turnCapability: 'not-required',
    turnProviderLeaseId: '',
    turnCredentialId: '',
    turnUsername: '',
    turnExpiresAt: '',
  })
  return signedFrame(privateKey, 'lease', lease, CREDENTIAL)
}

function retirementResponse(privateKey: KeyObject, request: Record<string, unknown>): Buffer {
  const receipt = Object.freeze({
    protocolVersion: EXTERNAL_FIXTURE_CONTROL_PROTOCOL,
    operation: request.operation,
    requestId: request.requestId,
    releaseRequestId: request.releaseRequestId,
    revokeRequestId: request.revokeRequestId,
    controlAuthority: request.controlAuthority,
    probeNonce: request.probeNonce,
    authorityInstanceId: AUTHORITY_ID,
    attestationSha256: ATTESTATION_SHA256,
    leaseExpiresAt: LEASE_EXPIRES_AT,
    controlTerminal: 'revoked',
    turnProviderLeaseId: '',
    turnTerminal: 'not-required',
    terminal: 'revoked',
    retiredAt: new Date().toISOString(),
  })
  return signedFrame(privateKey, 'receipt', receipt, Buffer.alloc(0))
}

function signedFrame(
  privateKey: KeyObject,
  field: 'lease' | 'receipt',
  payload: Readonly<Record<string, unknown>>,
  rawPayload: Uint8Array,
): Buffer {
  const encoded = Buffer.from(`${JSON.stringify(payload)}\n`, 'utf8')
  const envelope = Object.freeze({
    protocolVersion: EXTERNAL_FIXTURE_CONTROL_PROTOCOL,
    [field]: payload,
    [`${field}Sha256`]: createHash('sha256').update(encoded).digest('hex'),
    signatureAlgorithm: EXTERNAL_FIXTURE_SIGNATURE_ALGORITHM,
    signature: sign(null, encoded, privateKey).toString('base64url'),
  })
  return encodeFrame(envelope, rawPayload)
}

function encodeFrame(metadata: Readonly<Record<string, unknown>>, payload: Uint8Array): Buffer {
  const encoded = Buffer.from(`${JSON.stringify(metadata)}\n`, 'utf8')
  const frame = Buffer.alloc(4 + encoded.byteLength + payload.byteLength)
  frame.writeUInt32BE(encoded.byteLength, 0)
  encoded.copy(frame, 4)
  frame.set(payload, 4 + encoded.byteLength)
  return frame
}

function parseFrameMetadata(frame: Uint8Array): Record<string, unknown> {
  const bytes = Buffer.from(frame.buffer, frame.byteOffset, frame.byteLength)
  const length = bytes.readUInt32BE(0)
  const metadata = JSON.parse(bytes.subarray(4, 4 + length).toString('utf8')) as unknown
  if (typeof metadata !== 'object' || metadata === null || Array.isArray(metadata)) {
    throw new Error('test broker request metadata is invalid')
  }
  const record = metadata as Record<string, unknown>
  expect(record.schemaVersion).toBe(EXTERNAL_FIXTURE_CREDENTIAL_BROKER_PROTOCOL)
  return record
}
