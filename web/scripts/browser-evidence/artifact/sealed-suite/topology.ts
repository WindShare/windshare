import type { BrowserSampleResult } from '../../result.ts'
import { freezeRecord } from '../../contract/json.ts'
import {
  parseTestIceTopologyJson,
  parseTestIceTopologyResolutionJson,
} from '../../test-ice-topology.ts'
import { sha256Bytes } from '../manifest.ts'
import {
  GUARD_UPLOAD_TOPOLOGY_PROFILE_PATH,
  GUARD_UPLOAD_TOPOLOGY_RESOLUTION_PATH,
  MAXIMUM_TOPOLOGY_BYTES,
  type GuardUploadSampleInput,
  type GuardUploadTopologyManifest,
  type GuardUploadTopologySnapshots,
} from './contract.ts'
import { fileAuthority } from './manifest-codec.ts'
import { decodeUtf8 } from './prepared-tree-io.ts'

export interface CanonicalTopology {
  readonly profileBytes: Uint8Array
  readonly resolutionBytes: Uint8Array
  readonly manifest: GuardUploadTopologyManifest
}

export function canonicalTopology(
  value: GuardUploadTopologySnapshots,
  samples: readonly GuardUploadSampleInput[],
): CanonicalTopology {
  const profileBytes = Uint8Array.from(value.profileBytes)
  const resolutionBytes = Uint8Array.from(value.resolutionBytes)
  validateTopologyBytes(profileBytes, resolutionBytes, samples.map(({ sample }) => sample))
  return Object.freeze({
    profileBytes,
    resolutionBytes,
    manifest: freezeRecord({
      profile: fileAuthority(
        GUARD_UPLOAD_TOPOLOGY_PROFILE_PATH,
        profileBytes,
      ) as GuardUploadTopologyManifest['profile'],
      resolution: fileAuthority(
        GUARD_UPLOAD_TOPOLOGY_RESOLUTION_PATH,
        resolutionBytes,
      ) as GuardUploadTopologyManifest['resolution'],
    }),
  })
}

export function validateTopologyBytes(
  profileBytes: Uint8Array,
  resolutionBytes: Uint8Array,
  samples: readonly Pick<BrowserSampleResult, 'topologyProfileSha256' | 'topologyResolutionSha256'>[] = [],
): void {
  if (profileBytes.byteLength < 1 || profileBytes.byteLength > MAXIMUM_TOPOLOGY_BYTES ||
      resolutionBytes.byteLength < 1 || resolutionBytes.byteLength > MAXIMUM_TOPOLOGY_BYTES) {
    throw new Error('guard topology snapshot exceeds its byte authority')
  }
  const profileSha256 = sha256Bytes(profileBytes)
  const resolutionSha256 = sha256Bytes(resolutionBytes)
  const profile = parseTestIceTopologyJson(decodeUtf8(profileBytes, 'guard topology profile'))
  parseTestIceTopologyResolutionJson(
    decodeUtf8(resolutionBytes, 'guard topology resolution'),
    profile,
    profileSha256,
  )
  if (samples.some((sample) =>
    sample.topologyProfileSha256 !== profileSha256 ||
    sample.topologyResolutionSha256 !== resolutionSha256)) {
    throw new Error('guard topology snapshots do not bind every sample result')
  }
}
