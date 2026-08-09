import { describe, expect, it } from 'vitest'

import { encodeBase64Url } from '../../../src/crypto/bytes'
import {
  decodePackagedArtifactV1,
  decodeSealedMaterializationV1,
  sealPackagedArtifact,
  sealWorkspaceMaterialization,
} from '../../../src/output/workspace/aggregate'

describe('PackagedArtifactV1 repository codec', () => {
  it('rehydrates the canonical package authority and rejects non-canonical bytes', async () => {
    const artifact = await sealPackagedArtifact({
      operationId: identity(16, 1),
      receiveIntentDigest: identity(32, 2),
      sealedMaterializationDigest: identity(32, 3),
      artifactSpecDigest: identity(32, 4),
      packageOwnedObjectId: identity(32, 5),
      exactBytes: 42n,
      artifactReceiptDigest: identity(32, 6),
      layoutDigest: identity(32, 7),
    })

    await expect(decodePackagedArtifactV1(artifact.canonicalBytes)).resolves.toEqual(artifact)
    await expect(decodePackagedArtifactV1(
      Uint8Array.from([...artifact.canonicalBytes, 0]),
    )).rejects.toThrow('trailing canonical bytes')
  })
})

describe('SealedMaterializationV1 repository codec', () => {
  it('rehydrates the exact preparation and generation binding', async () => {
    const seal = await sealWorkspaceMaterialization({
      operationId: identity(16, 1),
      receiveIntentDigest: identity(32, 2),
      workspaceBindingDigest: identity(32, 3),
      preparationBinding: { kind: 'present', preparationDigest: identity(32, 4) },
      materializedManifestDigest: identity(32, 5),
      generationTableDigest: identity(32, 6),
      artifactVersion: 1,
      layoutVersion: 1,
      rawWorkspaceReceiptDigest: identity(32, 7),
    })

    await expect(decodeSealedMaterializationV1(seal.canonicalBytes)).resolves.toEqual(seal)
  })
})

function identity(width: number, fill: number): string {
  return encodeBase64Url(new Uint8Array(width).fill(fill))
}
