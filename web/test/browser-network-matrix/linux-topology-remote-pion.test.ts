import {
  createHash,
  generateKeyPairSync,
  sign as signBytes,
  type KeyObject,
} from 'node:crypto'

import { describe, expect, it, vi } from 'vitest'

import {
  RemotePionAuthorityExpiredError,
  RemotePionContainmentError,
  RemotePionControlClient,
  RemotePionProtocolError,
  REMOTE_PION_ATTESTATION_LEASE_MS,
  REMOTE_PION_PROTOCOL_VERSION,
  remotePionChallengeBindingSha256,
  type RemotePionAuthorityBinding,
  type RemotePionHttpRequest,
  type RemotePionHttpResponse,
} from '../../scripts/browser-network-matrix/linux-topology/remote-pion.ts'
import {
  canonicalExternalFixtureAttestationJson,
  EXTERNAL_FIXTURE_ATTESTATION_SCHEMA,
  EXTERNAL_FIXTURE_DECLARATION_SCHEMA,
  EXTERNAL_FIXTURE_SIGNATURE_ALGORITHM,
  externalFixtureNetworkBindingSha256,
  externalFixtureRemotePeerBindingSha256,
  type ExternalFixtureAttestation,
  type ExternalFixtureDeclaration,
  type ExternalFixtureNetworkSemantics,
  type SignedExternalFixtureAttestation,
} from '../../scripts/browser-network-matrix/linux-topology/external-fixture-attestation.ts'
import {
  canonicalExternalFixtureTerminalReceiptJson,
  type ExternalFixtureTerminalReceipt,
  type SignedExternalFixtureTerminalReceipt,
} from '../../scripts/browser-network-matrix/linux-topology/external-fixture-terminal-receipt.ts'
import {
  NETWORK_MATRIX_ATTEMPT_AUTHORITY_SCHEMA,
  NETWORK_MATRIX_ATTEMPT_REQUEST_AUTHORITY_SCHEMA,
  NETWORK_MATRIX_CONTROL_AUTHORITY_SCHEMA,
  NETWORK_MATRIX_SAMPLE_AUTHORITY_SCHEMA,
  parseNetworkMatrixAttemptRequestAuthority,
  parseNetworkMatrixControlAuthority,
  type NetworkMatrixAttemptAuthority,
  type NetworkMatrixControlAuthority,
  type NetworkMatrixFixtureAuthorityBinding,
  type NetworkMatrixSampleAuthority,
} from '../../scripts/browser-network-matrix/sample-authority.ts'
import type { NetworkMatrixProfileId } from '../../scripts/browser-network-matrix/vocabulary.ts'

const PROTOCOL = REMOTE_PION_PROTOCOL_VERSION
const RUN_ID = 'remote-pion-probe-test'
const OTHER_RUN_ID = 'remote-pion-other-run'
const NONCE = 'nonce012345678901'
const OTHER_NONCE = 'different-nonce-012345'
const LEASE_ID = 'lease012345678901'
const REQUEST_ID = 'request012345678'
const ATTEMPT_ID = 'attempt012345678'
const CHALLENGE = 'challenge01234567'
const CONTROL_CREDENTIAL = 'controlCredentialAuthority0123456789'
const CONTROLLER_ORIGIN = 'https://pion.example.test:8443/'
// Fixed public addresses keep socket-identity tests independent from mutable DNS.
// eslint-disable-next-line sonarjs/no-hardcoded-ip
const CONTROLLER_PUBLIC_IP = '93.184.216.34'
// eslint-disable-next-line sonarjs/no-hardcoded-ip
const OTHER_PUBLIC_IP = '1.1.1.1'
// eslint-disable-next-line sonarjs/no-hardcoded-ip
const REMOTE_PEER_PUBLIC_IP = '8.8.8.8'
const TLS_CERTIFICATE_SHA256 = '2'.repeat(64)
const IMPLEMENTATION_SHA256 = '3'.repeat(64)
const NOW = Date.parse('2030-01-01T00:00:00.000Z')
const AUTHORITY_EXPIRES_AT = timestamp(NOW + REMOTE_PION_ATTESTATION_LEASE_MS)
const ATTEMPT_EXPIRES_AT = timestamp(NOW + 1_000)
const AUTHORITY_KEYS = generateKeyPairSync('ed25519')
const HOSTILE_KEYS = generateKeyPairSync('ed25519')
const AUTHORITY_PUBLIC_KEY = pemPublicKey(AUTHORITY_KEYS.publicKey)
const HOSTILE_PUBLIC_KEY = pemPublicKey(HOSTILE_KEYS.publicKey)

describe('remote Pion signed external-fixture authority', () => {
  it('uses one signed public-STUN authority for the full attempt lifecycle', async () => {
    const server = attemptServer()
    const signal = new AbortController().signal

    const probe = await probeFixture(server.client)
    expect(probe).toMatchObject({
      outcome: 'satisfied',
      runId: RUN_ID,
      profileId: 'scheduled-public-stun',
      authorityInstanceId: 'pion-authority-alpha',
      remoteServiceInstanceId: 'pion-service-alpha',
      controllerPublicIp: CONTROLLER_PUBLIC_IP,
      leaseExpiresAt: AUTHORITY_EXPIRES_AT,
      rtcConfiguration: {
        iceTransportPolicy: 'all',
        iceServers: [{
          urls: ['stun:stun.example.test:3478'],
          username: null,
          credential: null,
        }],
      },
    })

    const binding = server.authorityBinding()
    expect(binding).toEqual({
      controlAuthority: controlAuthorityFor('scheduled-public-stun'),
      fixtureBinding: {
        attestationSha256: probe.outcome === 'satisfied' ? probe.attestationSha256 : '',
        authorityInstanceId: 'pion-authority-alpha',
        remoteServiceInstanceId: 'pion-service-alpha',
        networkBindingSha256: probe.outcome === 'satisfied' ? probe.networkBindingSha256 : '',
        remotePeerBindingSha256:
          probe.outcome === 'satisfied' ? probe.remotePeerBindingSha256 : '',
      },
    })
    const lease = await server.client.createAttempt(REQUEST_ID, 1_000, signal)
    expect(lease).toEqual({
      attemptAuthority: attemptAuthority(binding),
      leaseIssuedAt: timestamp(NOW),
      leaseExpiresAt: ATTEMPT_EXPIRES_AT,
      leaseMillis: 1_000,
    })

    await expect(server.client.offer(ATTEMPT_ID, 'v=0\r\noffer', signal))
      .resolves.toBe('v=0\r\nanswer')

    const challengeBindingSha256 = remotePionChallengeBindingSha256(lease.attemptAuthority)
    await expect(server.client.result(ATTEMPT_ID, signal)).resolves.toMatchObject({
      attemptAuthority: lease.attemptAuthority,
      state: 'established',
      selectedPair: { local: { protocol: 'udp' }, remote: { protocol: 'udp' } },
      challengeBindingSha256,
      failureCode: null,
      terminalReceipt: {
        receipt: {
          attemptAuthority: lease.attemptAuthority,
          state: 'established',
          challengeBindingSha256,
        },
      },
    })

    await expect(server.client.deleteAttempt(ATTEMPT_ID, signal)).resolves.toBeUndefined()
    await expect(server.client.result(ATTEMPT_ID, signal))
      .rejects.toThrow('remote Pion attempt has no issued authority')

    const calls = server.request.mock.calls.map((call) => call[0])
    expect(calls.map((call) => [call.method, call.path])).toEqual([
      ['POST', '/v2/authority-probe'],
      ['POST', '/v2/stun-probe'],
      ['POST', '/v2/attempts'],
      ['POST', '/v2/attempts/' + ATTEMPT_ID + '/offer'],
      ['GET', '/v2/attempts/' + ATTEMPT_ID],
      ['DELETE', '/v2/attempts/' + ATTEMPT_ID],
    ])
    expect(calls.every((call) =>
      Buffer.from(call.controlCredential).equals(Buffer.from(CONTROL_CREDENTIAL, 'ascii')),
    )).toBe(true)
    expect(requestBody(calls[0])).toEqual({
      protocolVersion: PROTOCOL,
      controlAuthority: binding.controlAuthority,
      nonce: NONCE,
      requestedLeaseMillis: REMOTE_PION_ATTESTATION_LEASE_MS,
    })
    expect(requestBody(calls[1])).toEqual({
      protocolVersion: PROTOCOL,
      ...binding,
      nonce: NONCE,
      stunUri: 'stun:stun.example.test:3478',
    })
    expect(requestBody(calls[2])).toEqual({
      protocolVersion: PROTOCOL,
      requestAuthority: lease.attemptAuthority.requestAuthority,
      leaseMillis: 1_000,
    })
  })

  it('rejects an attestation signed by a key other than the pinned Ed25519 authority', async () => {
    const server = signedFixtureServer({ signingKey: HOSTILE_KEYS.privateKey })

    await expect(probeFixture(server.client)).resolves.toEqual({
      outcome: 'invalid',
      failureCode: 'proof-invalid',
    })
  })

  it('does not negotiate trust from a self-issued key advertised by the fixture', async () => {
    const server = signedFixtureServer({
      signingKey: HOSTILE_KEYS.privateKey,
      authorityResponseBody: (signed) => canonicalJson({
        protocolVersion: signed.protocolVersion,
        attestation: signed.attestation,
        attestationSha256: signed.attestationSha256,
        signatureAlgorithm: signed.signatureAlgorithm,
        signature: signed.signature,
        attestationPublicKey: HOSTILE_PUBLIC_KEY,
      }),
    })

    await expect(probeFixture(server.client)).resolves.toEqual({
      outcome: 'invalid',
      failureCode: 'proof-invalid',
    })
  })

  it.each([
    {
      name: 'run',
      attestation: (request: Readonly<Record<string, unknown>>) => makeAttestation({
        runId: OTHER_RUN_ID,
        nonce: stringValue(request.nonce),
      }),
    },
    {
      name: 'nonce',
      attestation: (request: Readonly<Record<string, unknown>>) => makeAttestation({
        runId: authorityRequestSample(request).runId,
        nonce: OTHER_NONCE,
      }),
    },
    {
      name: 'profile',
      attestation: (request: Readonly<Record<string, unknown>>) => makeAttestation({
        runId: authorityRequestSample(request).runId,
        nonce: stringValue(request.nonce),
        profileId: 'scheduled-restricted-udp',
      }),
    },
  ])('rejects a correctly signed $name swap', async ({ attestation }) => {
    const server = signedFixtureServer({ attestation })

    await expect(probeFixture(server.client)).resolves.toEqual({
      outcome: 'invalid',
      failureCode: 'authority-binding-mismatch',
    })
  })

  it('classifies an expired signed lease as unavailable', async () => {
    const server = signedFixtureServer({
      controlLeaseExpiresAt: timestamp(NOW + 1_000),
      attestation: (request) => makeAttestation({
        runId: authorityRequestSample(request).runId,
        nonce: stringValue(request.nonce),
        issuedAt: timestamp(NOW - REMOTE_PION_ATTESTATION_LEASE_MS),
        expiresAt: timestamp(NOW),
      }),
    })

    await expect(probeFixture(server.client)).resolves.toEqual({
      outcome: 'unavailable',
      failureCode: 'authority-attestation-expired',
    })
  })

  it('rejects a TURN credential authority that outlives the signed attestation lease', async () => {
    const server = signedFixtureServer({
      profileId: 'scheduled-coturn',
      attestation: (request) => makeAttestation({
        runId: authorityRequestSample(request).runId,
        nonce: stringValue(request.nonce),
        profileId: 'scheduled-coturn',
        fixture: makeFixture('scheduled-coturn', {
          turnCredentialExpiresAt: timestamp(NOW + REMOTE_PION_ATTESTATION_LEASE_MS + 1_000),
        }),
      }),
    })

    await expect(probeFixture(server.client, 'scheduled-coturn')).resolves.toEqual({
      outcome: 'unavailable',
      failureCode: 'authority-attestation-expired',
    })
    expect(paths(server.request)).toEqual(['/v2/authority-probe'])
  })

  it.each([
    {
      name: 'loopback remote socket',
      observedRemoteAddress: '127.0.0.1',
      observedTlsCertificateSha256: TLS_CERTIFICATE_SHA256,
      localInterfaceAddresses: [] as readonly string[],
    },
    {
      name: 'private remote socket',
      // A literal RFC1918 peer proves that transport observation is fail-closed.
      // eslint-disable-next-line sonarjs/no-hardcoded-ip
      observedRemoteAddress: '10.1.2.3',
      observedTlsCertificateSha256: TLS_CERTIFICATE_SHA256,
      localInterfaceAddresses: [] as readonly string[],
    },
    {
      name: 'mapped local-interface alias',
      observedRemoteAddress: CONTROLLER_PUBLIC_IP,
      observedTlsCertificateSha256: TLS_CERTIFICATE_SHA256,
      localInterfaceAddresses: ['::ffff:' + CONTROLLER_PUBLIC_IP],
    },
    {
      name: 'different global socket peer',
      observedRemoteAddress: OTHER_PUBLIC_IP,
      observedTlsCertificateSha256: TLS_CERTIFICATE_SHA256,
      localInterfaceAddresses: [] as readonly string[],
    },
    {
      name: 'different TLS certificate pin',
      observedRemoteAddress: CONTROLLER_PUBLIC_IP,
      observedTlsCertificateSha256: 'f'.repeat(64),
      localInterfaceAddresses: [] as readonly string[],
    },
  ])('rejects a valid signature observed through $name', async ({
    observedRemoteAddress,
    observedTlsCertificateSha256,
    localInterfaceAddresses,
  }) => {
    const server = signedFixtureServer({
      observedRemoteAddress,
      observedTlsCertificateSha256,
      localInterfaceAddresses,
    })

    await expect(probeFixture(server.client)).resolves.toEqual({
      outcome: 'invalid',
      failureCode: 'authority-binding-mismatch',
    })
  })

  it.each([
    {
      name: 'missing final newline',
      encode: (signed: SignedExternalFixtureAttestation) => JSON.stringify(signed),
    },
    {
      name: 'insignificant JSON whitespace',
      encode: (signed: SignedExternalFixtureAttestation) => JSON.stringify(signed, null, 2) + '\n',
    },
    {
      name: 'reordered response fields',
      encode: (signed: SignedExternalFixtureAttestation) => canonicalJson({
        signature: signed.signature,
        protocolVersion: signed.protocolVersion,
        attestation: signed.attestation,
        attestationSha256: signed.attestationSha256,
        signatureAlgorithm: signed.signatureAlgorithm,
      }),
    },
  ])('rejects a noncanonical authority response with $name', async ({ encode }) => {
    const server = signedFixtureServer({ authorityResponseBody: encode })

    await expect(probeFixture(server.client)).resolves.toEqual({
      outcome: 'invalid',
      failureCode: 'proof-invalid',
    })
  })
})

describe('remote Pion issued attempt authority', () => {
  it('refuses a new attempt after the live attestation expires', async () => {
    const server = attemptServer()
    await probeFixture(server.client)
    server.clock.value = NOW + REMOTE_PION_ATTESTATION_LEASE_MS

    await expect(server.client.createAttempt(
      REQUEST_ID,
      1_000,
      new AbortController().signal,
    )).rejects.toBeInstanceOf(RemotePionAuthorityExpiredError)
    expect(paths(server.request).filter((path) => path === '/v2/attempts')).toHaveLength(0)
  })

  it('refuses attempt operations after their issued lease expires while still permitting reaping', async () => {
    const server = attemptServer()
    const signal = new AbortController().signal
    await probeFixture(server.client)
    await server.client.createAttempt(REQUEST_ID, 1_000, signal)
    server.clock.value = NOW + 1_000

    await expect(server.client.result(ATTEMPT_ID, signal))
      .rejects.toBeInstanceOf(RemotePionAuthorityExpiredError)
    expect(server.request.mock.calls.filter((call) =>
      call[0].method === 'GET' && call[0].path.endsWith(ATTEMPT_ID))).toHaveLength(0)
    await expect(server.client.deleteAttempt(ATTEMPT_ID, signal)).resolves.toBeUndefined()
  })

  it.each([
    {
      name: 'run ID',
      mutate: (authority: NetworkMatrixAttemptAuthority) => mutateSampleAuthority(
        authority,
        { runId: OTHER_RUN_ID },
      ),
    },
    {
      name: 'attestation digest',
      mutate: (authority: NetworkMatrixAttemptAuthority) => mutateFixtureBinding(
        authority,
        { attestationSha256: 'a'.repeat(64) },
      ),
    },
    {
      name: 'remote service instance',
      mutate: (authority: NetworkMatrixAttemptAuthority) => mutateFixtureBinding(
        authority,
        { remoteServiceInstanceId: 'other-service' },
      ),
    },
    {
      name: 'static network digest',
      mutate: (authority: NetworkMatrixAttemptAuthority) => mutateFixtureBinding(
        authority,
        { networkBindingSha256: 'b'.repeat(64) },
      ),
    },
    {
      name: 'remote peer digest',
      mutate: (authority: NetworkMatrixAttemptAuthority) => mutateFixtureBinding(
        authority,
        { remotePeerBindingSha256: 'c'.repeat(64) },
      ),
    },
  ] satisfies readonly {
    readonly name: string
    readonly mutate: (authority: NetworkMatrixAttemptAuthority) => NetworkMatrixAttemptAuthority
  }[])('rejects an attempt result with a mismatched $name binding', async ({ mutate }) => {
    const server = attemptServer({ resultAuthorityMutation: mutate })
    const signal = new AbortController().signal
    await probeFixture(server.client)
    await server.client.createAttempt(REQUEST_ID, 1_000, signal)

    await expect(server.client.result(ATTEMPT_ID, signal)).rejects.toMatchObject({
      name: RemotePionProtocolError.name,
      failureCode: 'authority-binding-mismatch',
    })
    await server.client.deleteAttempt(ATTEMPT_ID, signal)
  })

  it('replays and reaps a duplicate create when the first response is not authoritative', async () => {
    const server = attemptServer({ malformedFirstCreate: true })
    const signal = new AbortController().signal
    await probeFixture(server.client)

    await expect(server.client.createAttempt(
      REQUEST_ID,
      1_000,
      signal,
    )).rejects.toThrow('remote Pion attempt creation did not publish a bound response')

    const creates = server.request.mock.calls.filter((call) =>
      call[0].method === 'POST' && call[0].path === '/v2/attempts')
    expect(creates).toHaveLength(2)
    expect(creates[1]?.[0].body).toBe(creates[0]?.[0].body)
    expect(server.request.mock.calls.filter((call) =>
      call[0].method === 'DELETE' &&
      call[0].path === '/v2/attempts/' + ATTEMPT_ID)).toHaveLength(1)
    await expect(server.client.result(ATTEMPT_ID, signal))
      .rejects.toThrow('remote Pion attempt has no issued authority')
  })

  it('retains cleanup authority until a canonical reaped receipt is observed', async () => {
    const server = attemptServer({
      deleteReceipt: (deleteCall) => deleteCall === 1 ? 'still-live' : 'reaped',
    })
    const signal = new AbortController().signal
    await probeFixture(server.client)
    await server.client.createAttempt(REQUEST_ID, 1_000, signal)

    await expect(server.client.deleteAttempt(ATTEMPT_ID, signal))
      .rejects.toBeInstanceOf(RemotePionContainmentError)
    await expect(server.client.deleteAttempt(ATTEMPT_ID, signal)).resolves.toBeUndefined()
    expect(server.request.mock.calls.filter((call) =>
      call[0].method === 'DELETE' &&
      call[0].path === '/v2/attempts/' + ATTEMPT_ID)).toHaveLength(2)
  })

  it('serializes concurrent same-run probes and reuses the one live signed authority', async () => {
    let releaseAuthority!: () => void
    const authorityGate = new Promise<void>((resolve) => {
      releaseAuthority = resolve
    })
    const server = signedFixtureServer({
      beforeAuthorityResponse: () => authorityGate,
    })
    const input = fixtureProbeInput()
    const first = server.client.probe(input).result
    const second = server.client.probe(fixtureProbeInput()).result

    await vi.waitFor(() => {
      expect(paths(server.request).filter((path) => path === '/v2/authority-probe')).toHaveLength(1)
    })
    expect(paths(server.request).filter((path) => path === '/v2/stun-probe')).toHaveLength(0)

    releaseAuthority()
    const [firstResult, secondResult] = await Promise.all([first, second])
    const thirdResult = await probeFixture(server.client)

    expect(firstResult).toMatchObject({ outcome: 'satisfied' })
    expect(secondResult).toEqual(firstResult)
    expect(thirdResult).toEqual(firstResult)
    expect(paths(server.request)).toEqual([
      '/v2/authority-probe',
      '/v2/stun-probe',
    ])
  })
})

interface FixtureOptions {
  readonly controllerPublicIp?: string
  readonly tlsCertificateSha256?: string
  readonly turnCredentialExpiresAt?: string
}

function makeFixture(
  profileId: NetworkMatrixProfileId,
  options: FixtureOptions = {},
): ExternalFixtureDeclaration {
  const networkSemantics = networkSemanticsFor(profileId, options.turnCredentialExpiresAt)
  return {
    schemaVersion: EXTERNAL_FIXTURE_DECLARATION_SCHEMA,
    deploymentId: 'external-fixture-alpha',
    revision: 7,
    profileId,
    authorityInstanceId: 'pion-authority-alpha',
    implementationSha256: IMPLEMENTATION_SHA256,
    remoteServiceInstanceId: 'pion-service-alpha',
    operatorId: 'fixture-operator-alpha',
    fixtureHostId: 'fixture-host-alpha',
    fixtureNetworkBoundaryId: 'fixture-boundary-alpha',
    controllerOrigin: CONTROLLER_ORIGIN,
    controllerPublicIp: options.controllerPublicIp ?? CONTROLLER_PUBLIC_IP,
    tlsCertificateSha256: options.tlsCertificateSha256 ?? TLS_CERTIFICATE_SHA256,
    remotePeerPublicIp: REMOTE_PEER_PUBLIC_IP,
    remotePeerUdpPortMin: 40_000,
    remotePeerUdpPortMax: 40_127,
    networkSemantics,
  }
}

function networkSemanticsFor(
  profileId: NetworkMatrixProfileId,
  turnCredentialExpiresAt = AUTHORITY_EXPIRES_AT,
): ExternalFixtureNetworkSemantics {
  if (profileId === 'scheduled-public-stun') {
    return {
      kind: 'public-stun',
      policyId: 'public-stun-policy',
      policyVersion: 3,
      stunEndpoint: 'stun:stun.example.test:3478',
    }
  }
  if (profileId === 'scheduled-restricted-udp') {
    return {
      kind: 'restricted-udp',
      policyId: 'restricted-udp-policy',
      policyVersion: 4,
      outboundUdp: 'denied',
      unsolicitedInboundUdp: 'denied',
      relayAccess: 'denied',
    }
  }
  if (profileId === 'scheduled-coturn') {
    return {
      kind: 'coturn-relay',
      policyId: 'coturn-relay-policy',
      policyVersion: 5,
      turnServiceOwnerId: 'turn-owner-alpha',
      turnUrls: ['turn:turn.example.test:3478?transport=udp'],
      turnUsername: 'turn-user-alpha',
      turnCredentialId: 'turn-credential-alpha',
      turnCredentialExpiresAt,
    }
  }
  return {
    kind: 'operator-real-nat',
    policyId: 'operator-real-nat-policy',
    policyVersion: 6,
    senderHostId: 'operator-sender-alpha',
    senderNetworkBoundaryId: 'operator-network-alpha',
    stunEndpoint: 'stun:stun.example.test:3478',
  }
}

interface AttestationOptions {
  readonly runId: string
  readonly nonce: string
  readonly profileId?: NetworkMatrixProfileId
  readonly issuedAt?: string
  readonly expiresAt?: string
  readonly leaseMillis?: number
  readonly fixture?: ExternalFixtureDeclaration
}

function makeAttestation(options: AttestationOptions): ExternalFixtureAttestation {
  const leaseMillis = options.leaseMillis ?? REMOTE_PION_ATTESTATION_LEASE_MS
  const issuedAt = options.issuedAt ?? timestamp(NOW)
  const expiresAt = options.expiresAt ?? timestamp(Date.parse(issuedAt) + leaseMillis)
  const profileId = options.profileId ?? 'scheduled-public-stun'
  return {
    schemaVersion: EXTERNAL_FIXTURE_ATTESTATION_SCHEMA,
    runId: options.runId,
    nonce: options.nonce,
    leaseId: LEASE_ID,
    leaseMillis,
    issuedAt,
    expiresAt,
    fixture: options.fixture ?? makeFixture(profileId),
  }
}

function signAttestation(
  attestation: ExternalFixtureAttestation,
  privateKey: KeyObject,
): SignedExternalFixtureAttestation {
  const canonical = canonicalExternalFixtureAttestationJson(attestation)
  return {
    protocolVersion: PROTOCOL,
    attestation,
    attestationSha256: sha256(canonical),
    signatureAlgorithm: EXTERNAL_FIXTURE_SIGNATURE_ALGORITHM,
    signature: signBytes(null, Buffer.from(canonical, 'utf8'), privateKey).toString('base64url'),
  }
}

type NonProbeHandler = (
  input: RemotePionHttpRequest,
  body: Readonly<Record<string, unknown>>,
  signed: SignedExternalFixtureAttestation,
  respond: (
    statusCode: number,
    body: Readonly<Record<string, unknown>>,
  ) => RemotePionHttpResponse,
) => Promise<RemotePionHttpResponse> | RemotePionHttpResponse

interface SignedFixtureServerOptions {
  readonly profileId?: NetworkMatrixProfileId
  readonly signingKey?: KeyObject
  readonly trustedPublicKey?: string | Buffer
  readonly attestation?: (
    request: Readonly<Record<string, unknown>>,
  ) => ExternalFixtureAttestation
  readonly authorityResponseBody?: (
    signed: SignedExternalFixtureAttestation,
  ) => string
  readonly observedRemoteAddress?: string
  readonly observedTlsCertificateSha256?: string
  readonly localInterfaceAddresses?: readonly string[]
  readonly now?: () => number
  readonly controlLeaseIssuedAt?: string
  readonly controlLeaseExpiresAt?: string
  readonly beforeAuthorityResponse?: () => Promise<void>
  readonly nonProbe?: NonProbeHandler
}

function signedFixtureServer(options: SignedFixtureServerOptions = {}) {
  const profileId = options.profileId ?? 'scheduled-public-stun'
  const controlAuthority = controlAuthorityFor(profileId)
  const authorityRequest = {
    protocolVersion: PROTOCOL,
    controlAuthority,
    nonce: NONCE,
    requestedLeaseMillis: REMOTE_PION_ATTESTATION_LEASE_MS,
  }
  const issuedAttestation = options.attestation?.(authorityRequest) ?? makeAttestation({
    runId: RUN_ID,
    nonce: NONCE,
    profileId,
  })
  const issuedSigned = signAttestation(
    issuedAttestation,
    options.signingKey ?? AUTHORITY_KEYS.privateKey,
  )
  let latestSigned: SignedExternalFixtureAttestation | null = null
  const observedRemoteAddress = options.observedRemoteAddress ?? CONTROLLER_PUBLIC_IP
  const observedTlsCertificateSha256 =
    options.observedTlsCertificateSha256 ?? TLS_CERTIFICATE_SHA256
  const respond = (
    statusCode: number,
    body: Readonly<Record<string, unknown>>,
  ): RemotePionHttpResponse => canonicalResponse(statusCode, body, {
    observedRemoteAddress,
    observedTlsCertificateSha256,
  })

  const request = vi.fn(async (
    input: RemotePionHttpRequest,
  ): Promise<RemotePionHttpResponse> => {
    const body = requestBody(input)
    if (input.path === '/v2/authority-probe') {
      await options.beforeAuthorityResponse?.()
      latestSigned = issuedSigned
      return rawResponse(
        200,
        options.authorityResponseBody?.(latestSigned) ?? canonicalJson(latestSigned),
        { observedRemoteAddress, observedTlsCertificateSha256 },
      )
    }
    if (latestSigned === null) throw new Error('test server has no signed authority')
    if (input.path === '/v2/stun-probe') {
      return respond(200, {
        protocolVersion: PROTOCOL,
        controlAuthority: body.controlAuthority,
        fixtureBinding: body.fixtureBinding,
        nonce: body.nonce,
        serverReflexiveObserved: true,
      })
    }
    if (input.path === '/v2/turn-credential') {
      return respond(200, {
        protocolVersion: PROTOCOL,
        controlAuthority: body.controlAuthority,
        fixtureBinding: body.fixtureBinding,
        credentialId: 'turn-credential-alpha',
        expiresAt: AUTHORITY_EXPIRES_AT,
        username: 'turn-user-alpha',
        credential: 'turn-secret-value-alpha',
      })
    }
    if (options.nonProbe !== undefined) {
      return options.nonProbe(input, body, latestSigned, respond)
    }
    throw new Error('unexpected test control request: ' + input.method + ' ' + input.path)
  })

  const client = new RemotePionControlClient({
    controllerOrigin: CONTROLLER_ORIGIN,
    tlsCertificateAuthority: 'test certificate authority',
    tlsCertificateSha256: TLS_CERTIFICATE_SHA256,
    attestationPublicKey: options.trustedPublicKey ?? AUTHORITY_PUBLIC_KEY,
    controlCredential: Buffer.from(CONTROL_CREDENTIAL, 'ascii'),
    controlLease: {
      controlAuthority,
      probeNonce: NONCE,
      authorityInstanceId: issuedAttestation.fixture.authorityInstanceId,
      attestationSha256: issuedSigned.attestationSha256,
      issuedAt: options.controlLeaseIssuedAt ?? timestamp(NOW),
      expiresAt: options.controlLeaseExpiresAt ?? issuedAttestation.expiresAt,
      maxAttempts: 1,
    },
    request,
    now: options.now ?? (() => NOW),
    localInterfaceAddresses: () => options.localInterfaceAddresses ?? [],
  })

  return {
    client,
    request,
    authorityBinding: (): RemotePionAuthorityBinding => {
      if (latestSigned === null) throw new Error('test server has no signed authority')
      return bindingFor(latestSigned, controlAuthority)
    },
  }
}

interface AttemptServerOptions {
  readonly malformedFirstCreate?: boolean
  readonly resultAuthorityMutation?: (
    authority: NetworkMatrixAttemptAuthority,
  ) => NetworkMatrixAttemptAuthority
  readonly deleteReceipt?: (deleteCall: number) => string
}

function attemptServer(options: AttemptServerOptions = {}) {
  const clock = { value: NOW }
  const attempts = new Map<string, {
    readonly attemptAuthority: NetworkMatrixAttemptAuthority
    readonly leaseIssuedAt: string
    readonly leaseExpiresAt: string
    readonly leaseMillis: number
  }>()
  let createCall = 0
  let deleteCall = 0
  const server = signedFixtureServer({
    now: () => clock.value,
    nonProbe: (input, body, _signed, respond) => {
      if (input.method === 'POST' && input.path === '/v2/attempts') {
        createCall += 1
        const requestAuthority = parseNetworkMatrixAttemptRequestAuthority(body.requestAuthority)
        const issuedAuthority: NetworkMatrixAttemptAuthority = Object.freeze({
          schemaVersion: NETWORK_MATRIX_ATTEMPT_AUTHORITY_SCHEMA,
          requestAuthority,
          attemptId: ATTEMPT_ID,
          challenge: CHALLENGE,
        })
        const leaseIssuedAt = timestamp(clock.value)
        const leaseExpiresAt = timestamp(clock.value + 1_000)
        const leaseMillis = 1_000
        attempts.set(ATTEMPT_ID, {
          attemptAuthority: issuedAuthority,
          leaseIssuedAt,
          leaseExpiresAt,
          leaseMillis,
        })
        if (options.malformedFirstCreate === true && createCall === 1) {
          return respond(201, {
            protocolVersion: PROTOCOL,
            attemptAuthority: {
              schemaVersion: NETWORK_MATRIX_ATTEMPT_AUTHORITY_SCHEMA,
              requestAuthority,
              attemptId: ATTEMPT_ID,
            },
            leaseIssuedAt,
            leaseExpiresAt,
            leaseMillis,
          })
        }
        return respond(201, {
          protocolVersion: PROTOCOL,
          attemptAuthority: issuedAuthority,
          leaseIssuedAt,
          leaseExpiresAt,
          leaseMillis,
        })
      }

      const offerMatch = /^\/v2\/attempts\/([^/]+)\/offer$/u.exec(input.path)
      if (input.method === 'POST' && offerMatch !== null) {
        const attemptId = offerMatch[1] as string
        const attempt = requireAttempt(attempts, attemptId)
        return respond(200, {
          protocolVersion: PROTOCOL,
          attemptAuthority: attempt.attemptAuthority,
          type: 'answer',
          sdp: 'v=0\r\nanswer',
        })
      }

      const attemptMatch = /^\/v2\/attempts\/([^/]+)$/u.exec(input.path)
      if (attemptMatch === null) {
        throw new Error('unexpected attempt route: ' + input.method + ' ' + input.path)
      }
      const attemptId = attemptMatch[1] as string
      const attempt = requireAttempt(attempts, attemptId)
      if (input.method === 'GET') {
        const resultAuthority = options.resultAuthorityMutation?.(attempt.attemptAuthority) ??
          attempt.attemptAuthority
        const selectedPair = { local: { protocol: 'udp' }, remote: { protocol: 'udp' } }
        const challengeBindingSha256 = remotePionChallengeBindingSha256(
          attempt.attemptAuthority,
        )
        return respond(200, {
          protocolVersion: PROTOCOL,
          attemptAuthority: resultAuthority,
          state: 'established',
          selectedPair,
          challengeBindingSha256,
          failureCode: null,
          terminalReceipt: signTerminalReceipt({
            attemptAuthority: attempt.attemptAuthority,
            leaseIssuedAt: attempt.leaseIssuedAt,
            leaseExpiresAt: attempt.leaseExpiresAt,
            leaseMillis: attempt.leaseMillis,
            selectedPair,
            challengeBindingSha256,
          }),
        })
      }
      if (input.method === 'DELETE') {
        deleteCall += 1
        const terminal = options.deleteReceipt?.(deleteCall) ?? 'reaped'
        if (terminal === 'reaped') attempts.delete(attemptId)
        return respond(200, {
          protocolVersion: PROTOCOL,
          attemptAuthority: attempt.attemptAuthority,
          terminal,
        })
      }
      throw new Error('unexpected attempt method: ' + input.method)
    },
  })

  return { ...server, clock }
}

function requireAttempt<T>(
  attempts: ReadonlyMap<string, T>,
  attemptId: string,
): T {
  const attempt = attempts.get(attemptId)
  if (attempt === undefined) throw new Error('test attempt does not exist')
  return attempt
}

function bindingFor(
  signed: SignedExternalFixtureAttestation,
  controlAuthority: NetworkMatrixControlAuthority,
): RemotePionAuthorityBinding {
  return {
    controlAuthority,
    fixtureBinding: fixtureBindingFor(signed),
  }
}

function fixtureBindingFor(
  signed: SignedExternalFixtureAttestation,
): NetworkMatrixFixtureAuthorityBinding {
  return Object.freeze({
    attestationSha256: signed.attestationSha256,
    authorityInstanceId: signed.attestation.fixture.authorityInstanceId,
    remoteServiceInstanceId: signed.attestation.fixture.remoteServiceInstanceId,
    networkBindingSha256: externalFixtureNetworkBindingSha256(signed.attestation.fixture),
    remotePeerBindingSha256: externalFixtureRemotePeerBindingSha256(signed.attestation.fixture),
  })
}

function sampleAuthorityFor(profileId: NetworkMatrixProfileId): NetworkMatrixSampleAuthority {
  return Object.freeze({
    schemaVersion: NETWORK_MATRIX_SAMPLE_AUTHORITY_SCHEMA,
    runId: RUN_ID,
    profileId,
    browser: 'chromium',
    sampleOrdinal: 1,
    processInstanceId: `browser-${profileId}`,
    operationId: `remote-pion-${profileId}-operation`,
  })
}

function controlAuthorityFor(profileId: NetworkMatrixProfileId): NetworkMatrixControlAuthority {
  return Object.freeze({
    schemaVersion: NETWORK_MATRIX_CONTROL_AUTHORITY_SCHEMA,
    sampleAuthority: sampleAuthorityFor(profileId),
    controlLeaseId: 'controllease0123456789',
  })
}

function attemptAuthority(
  binding: RemotePionAuthorityBinding,
  requestId = REQUEST_ID,
  attemptId = ATTEMPT_ID,
  challenge = CHALLENGE,
): NetworkMatrixAttemptAuthority {
  return Object.freeze({
    schemaVersion: NETWORK_MATRIX_ATTEMPT_AUTHORITY_SCHEMA,
    requestAuthority: Object.freeze({
      schemaVersion: NETWORK_MATRIX_ATTEMPT_REQUEST_AUTHORITY_SCHEMA,
      controlAuthority: binding.controlAuthority,
      requestId,
      fixtureBinding: binding.fixtureBinding,
    }),
    attemptId,
    challenge,
  })
}

function mutateSampleAuthority(
  authority: NetworkMatrixAttemptAuthority,
  override: Partial<NetworkMatrixSampleAuthority>,
): NetworkMatrixAttemptAuthority {
  return Object.freeze({
    ...authority,
    requestAuthority: Object.freeze({
      ...authority.requestAuthority,
      controlAuthority: Object.freeze({
        ...authority.requestAuthority.controlAuthority,
        sampleAuthority: Object.freeze({
          ...authority.requestAuthority.controlAuthority.sampleAuthority,
          ...override,
        }),
      }),
    }),
  })
}

function mutateFixtureBinding(
  authority: NetworkMatrixAttemptAuthority,
  override: Partial<NetworkMatrixFixtureAuthorityBinding>,
): NetworkMatrixAttemptAuthority {
  return Object.freeze({
    ...authority,
    requestAuthority: Object.freeze({
      ...authority.requestAuthority,
      fixtureBinding: Object.freeze({
        ...authority.requestAuthority.fixtureBinding,
        ...override,
      }),
    }),
  })
}

function fixtureProbeInput(profileId: NetworkMatrixProfileId = 'scheduled-public-stun') {
  return {
    sampleAuthority: sampleAuthorityFor(profileId),
    signal: new AbortController().signal,
  }
}

function authorityRequestSample(
  value: Readonly<Record<string, unknown>>,
): NetworkMatrixSampleAuthority {
  return parseNetworkMatrixControlAuthority(value.controlAuthority).sampleAuthority
}

function probeFixture(
  client: RemotePionControlClient,
  profileId: NetworkMatrixProfileId = 'scheduled-public-stun',
) {
  return client.probe(fixtureProbeInput(profileId)).result
}

function signTerminalReceipt(input: {
  readonly attemptAuthority: NetworkMatrixAttemptAuthority
  readonly leaseIssuedAt: string
  readonly leaseExpiresAt: string
  readonly leaseMillis: number
  readonly selectedPair: unknown
  readonly challengeBindingSha256: string
}): SignedExternalFixtureTerminalReceipt {
  const receipt: ExternalFixtureTerminalReceipt = Object.freeze({
    protocolVersion: PROTOCOL,
    attemptAuthority: input.attemptAuthority,
    terminalAt: timestamp(Date.parse(input.leaseIssuedAt) + 500),
    attemptLeaseIssuedAt: input.leaseIssuedAt,
    attemptLeaseExpiresAt: input.leaseExpiresAt,
    attemptLeaseMillis: input.leaseMillis,
    state: 'established',
    selectedPair: input.selectedPair,
    challengeBindingSha256: input.challengeBindingSha256,
    failureCode: null,
  })
  const canonical = canonicalExternalFixtureTerminalReceiptJson(receipt)
  return Object.freeze({
    protocolVersion: PROTOCOL,
    receipt,
    receiptSha256: sha256(canonical),
    signatureAlgorithm: EXTERNAL_FIXTURE_SIGNATURE_ALGORITHM,
    signature: signBytes(
      null,
      Buffer.from(canonical, 'utf8'),
      AUTHORITY_KEYS.privateKey,
    ).toString('base64url'),
  })
}

function requestBody(
  input: RemotePionHttpRequest | undefined,
): Readonly<Record<string, unknown>> {
  if (input === undefined || input.body === null) return {}
  const value: unknown = JSON.parse(input.body)
  if (typeof value !== 'object' || value === null || Array.isArray(value)) {
    throw new Error('test request body is not an object')
  }
  return value as Readonly<Record<string, unknown>>
}

function paths(
  request: ReturnType<typeof vi.fn<(input: RemotePionHttpRequest) => Promise<RemotePionHttpResponse>>>,
): readonly string[] {
  return request.mock.calls.map((call) => call[0].path)
}

function canonicalResponse(
  statusCode: number,
  body: Readonly<Record<string, unknown>>,
  observation: {
    readonly observedRemoteAddress: string
    readonly observedTlsCertificateSha256: string
  } = {
    observedRemoteAddress: CONTROLLER_PUBLIC_IP,
    observedTlsCertificateSha256: TLS_CERTIFICATE_SHA256,
  },
): RemotePionHttpResponse {
  return rawResponse(statusCode, canonicalJson(body), observation)
}

function rawResponse(
  statusCode: number,
  body: string,
  observation: {
    readonly observedRemoteAddress: string
    readonly observedTlsCertificateSha256: string
  },
): RemotePionHttpResponse {
  return {
    statusCode,
    body,
    observedRemoteAddress: observation.observedRemoteAddress,
    observedTlsCertificateSha256: observation.observedTlsCertificateSha256,
  }
}

function canonicalJson(value: unknown): string {
  return JSON.stringify(value) + '\n'
}

function pemPublicKey(key: KeyObject): string {
  return key.export({ type: 'spki', format: 'pem' }).toString()
}

function sha256(value: string): string {
  return createHash('sha256').update(value).digest('hex')
}

function timestamp(value: number): string {
  return new Date(value).toISOString()
}

function stringValue(value: unknown): string {
  if (typeof value !== 'string') throw new Error('test protocol value is not a string')
  return value
}
