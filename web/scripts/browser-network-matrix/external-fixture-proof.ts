import { createHash } from 'node:crypto'

import type { NetworkMatrixProfileId } from './vocabulary.ts'
import type { ExternalFixtureDeclaration } from './linux-topology/external-fixture-attestation.ts'
import { EXTERNAL_FIXTURE_CONTROL_PROTOCOL } from './linux-topology/external-fixture-attestation.ts'

const EXTERNAL_FIXTURE_CONFIGURATION_SCHEMA =
  'windshare.browser-network-matrix.external-fixture-configuration/v1'

export function externalFixturePublicConfigurationSha256(
  profileId: NetworkMatrixProfileId,
  attestationSha256: string,
  iceTransportPolicy: 'all' | 'relay',
  iceServerUrls: readonly (readonly string[])[],
): string {
  return createHash('sha256').update(`${JSON.stringify({
    schemaVersion: EXTERNAL_FIXTURE_CONFIGURATION_SCHEMA,
    profileId,
    attestationSha256,
    iceTransportPolicy,
    iceServerUrls,
  })}\n`).digest('hex')
}

export interface ExternalFixtureChallengeBinding {
  readonly runId: string
  readonly attestationSha256: string
  readonly remoteServiceInstanceId: string
  readonly networkBindingSha256: string
  readonly remotePeerBindingSha256: string
  readonly attemptId: string
  readonly challenge: string
}

export function externalFixtureChallengeBindingSha256(
  challenge: ExternalFixtureChallengeBinding,
): string {
  return createHash('sha256').update(`${JSON.stringify({
    protocolVersion: EXTERNAL_FIXTURE_CONTROL_PROTOCOL,
    runId: challenge.runId,
    attestationSha256: challenge.attestationSha256,
    remoteServiceInstanceId: challenge.remoteServiceInstanceId,
    networkBindingSha256: challenge.networkBindingSha256,
    remotePeerBindingSha256: challenge.remotePeerBindingSha256,
    attemptId: challenge.attemptId,
    challenge: challenge.challenge,
  })}\n`).digest('hex')
}

export function signedExternalFixtureConfigurationSha256(
  fixture: ExternalFixtureDeclaration,
  attestationSha256: string,
): string {
  const semantics = fixture.networkSemantics
  switch (semantics.kind) {
    case 'public-stun':
      return externalFixturePublicConfigurationSha256(
        fixture.profileId,
        attestationSha256,
        'all',
        [[semantics.stunEndpoint]],
      )
    case 'restricted-udp':
      return externalFixturePublicConfigurationSha256(
        fixture.profileId,
        attestationSha256,
        'all',
        [],
      )
    case 'coturn-relay':
      return externalFixturePublicConfigurationSha256(
        fixture.profileId,
        attestationSha256,
        'relay',
        [semantics.turnUrls],
      )
  }
}
