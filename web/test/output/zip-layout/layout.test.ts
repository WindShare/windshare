import { describe, expect, it } from 'vitest'

import { encodeBase64Url } from '../../../src/crypto/bytes'
import {
  ZipLayoutLedgerV1,
  planZipLayout,
  validateSealedZipLayoutPlan,
} from '../../../src/output/zip-layout/layout'
import {
  MAX_ZIP_SPOOL_BYTES,
  MAX_ZIP_SPOOL_ENTRIES,
  ZIP_UINT64_MAXIMUM,
  requireZipSpoolBudget,
} from '../../../src/output/zip-layout/policy'

const RECEIVE_INTENT_DIGEST = digest(0x11)
const ARTIFACT_DIGEST = digest(0x22)
const PREPARATION_DIGEST = digest(0x33)
const DISCOVERY_DIGEST = digest(0x44)

describe('sealed ZIP layout planning', () => {
  it('sorts unsigned UTF-8 paths, retains directories, and accounts every byte once', async () => {
    const plan = await planZipLayout({
      receiveIntentDigest: RECEIVE_INTENT_DIGEST,
      artifactDigest: ARTIFACT_DIGEST,
      preparationManifestDigest: PREPARATION_DIGEST,
      entries: [
        { kind: 'file', path: ['root', 'z'], exactSize: 2n },
        { kind: 'directory', path: ['root', 'empty'] },
        { kind: 'directory', path: ['root'] },
        { kind: 'file', path: ['root', 'a'], exactSize: 1n },
      ],
    })

    expect(plan.entries.map((entry) => `${entry.kind}:${entry.artifactPath}`)).toEqual([
      'directory:root',
      'file:root/a',
      'directory:root/empty',
      'file:root/z',
    ])
    expect(plan.centralDirectoryOffset).toBe(
      plan.entries.reduce((total, entry) => total + entry.entryStreamBytes, 0n),
    )
    expect(plan.centralDirectoryBytes).toBe(
      plan.entries.reduce((total, entry) => total + entry.centralRecordBytes, 0n),
    )
    expect(plan.maximumSpoolBytes).toBe(plan.centralDirectoryBytes)
    expect(plan.exactArchiveBytes).toBe(
      plan.centralDirectoryOffset + plan.centralDirectoryBytes +
      plan.zip64EndBytes + plan.classicEndBytes,
    )
    await expect(validateSealedZipLayoutPlan(plan)).resolves.toEqual(plan)
  })

  it('rejects missing result roots, missing parents, and uint64 layout overflow before content', async () => {
    await expect(planZipLayout({
      receiveIntentDigest: RECEIVE_INTENT_DIGEST,
      artifactDigest: ARTIFACT_DIGEST,
      preparationManifestDigest: PREPARATION_DIGEST,
      entries: [{ kind: 'file', path: ['root', 'a'], exactSize: 1n }],
    })).rejects.toThrow(/result-root directory/u)
    await expect(planZipLayout({
      receiveIntentDigest: RECEIVE_INTENT_DIGEST,
      artifactDigest: ARTIFACT_DIGEST,
      preparationManifestDigest: PREPARATION_DIGEST,
      entries: [
        { kind: 'directory', path: ['root'] },
        { kind: 'file', path: ['root', 'missing', 'a'], exactSize: 1n },
      ],
    })).rejects.toThrow(/parent directory/u)
    await expect(planZipLayout({
      receiveIntentDigest: RECEIVE_INTENT_DIGEST,
      artifactDigest: ARTIFACT_DIGEST,
      preparationManifestDigest: PREPARATION_DIGEST,
      entries: [
        { kind: 'directory', path: ['root'] },
        { kind: 'file', path: ['root', 'too-large'], exactSize: ZIP_UINT64_MAXIMUM },
      ],
    })).rejects.toThrow(/addition overflow/u)
  })

  it('orders a directory before a file at the same unsigned UTF-8 artifact path', async () => {
    const plan = await planZipLayout({
      receiveIntentDigest: RECEIVE_INTENT_DIGEST,
      artifactDigest: ARTIFACT_DIGEST,
      preparationManifestDigest: PREPARATION_DIGEST,
      entries: [
        { kind: 'file', path: ['root', 'same'], exactSize: 0n },
        { kind: 'directory', path: ['root'] },
        { kind: 'directory', path: ['root', 'same'] },
      ],
    })

    expect(plan.entries.slice(1).map((entry) => entry.kind)).toEqual(['directory', 'file'])
  })

  it('binds identity and layout evidence into the digest', async () => {
    const base = await planZipLayout({
      receiveIntentDigest: RECEIVE_INTENT_DIGEST,
      artifactDigest: ARTIFACT_DIGEST,
      preparationManifestDigest: PREPARATION_DIGEST,
      entries: [{ kind: 'directory', path: ['root'] }],
    })
    const changedEvidence = await planZipLayout({
      receiveIntentDigest: RECEIVE_INTENT_DIGEST,
      artifactDigest: ARTIFACT_DIGEST,
      preparationManifestDigest: digest(0x55),
      entries: [{ kind: 'directory', path: ['root'] }],
    })
    const changedBytes = {
      ...base,
      exactArchiveBytes: base.exactArchiveBytes + 1n,
    }

    expect(changedEvidence.digest).not.toBe(base.digest)
    await expect(validateSealedZipLayoutPlan(changedBytes)).rejects.toThrow(/canonical fields/u)
  })

  it('propagates an exact ZIP64-size member through global layout accounting', async () => {
    const plan = await planZipLayout({
      receiveIntentDigest: RECEIVE_INTENT_DIGEST,
      artifactDigest: ARTIFACT_DIGEST,
      preparationManifestDigest: PREPARATION_DIGEST,
      entries: [
        { kind: 'directory', path: ['root'] },
        { kind: 'file', path: ['root', 'large'], exactSize: 0xffff_ffffn },
      ],
    })
    const large = plan.entries[1]

    expect(large).toMatchObject({
      zip64Size: true,
      descriptorBytes: 24n,
      localExtraBytes: 20n,
    })
    expect(plan.centralDirectoryOffset).toBeGreaterThanOrEqual(0xffff_ffffn)
    expect(plan.zip64EndRequired).toBe(true)
    expect(plan.zip64EndBytes).toBe(76n)
    expect(plan.exactArchiveBytes).toBe(
      plan.centralDirectoryOffset + plan.centralDirectoryBytes + 76n + 22n,
    )
  })

  it('checks spool ceilings without constructing periodic-scale fixtures', () => {
    expect(() => requireZipSpoolBudget(
      BigInt(MAX_ZIP_SPOOL_ENTRIES),
      MAX_ZIP_SPOOL_BYTES,
    )).not.toThrow()
    expect(() => requireZipSpoolBudget(
      BigInt(MAX_ZIP_SPOOL_ENTRIES) + 1n,
      0n,
    )).toThrow(/entry budget/u)
    expect(() => requireZipSpoolBudget(1n, MAX_ZIP_SPOOL_BYTES + 1n))
      .toThrow(/byte budget/u)
  })
})

describe('progressive ZIP layout ledger', () => {
  it('seals only a complete canonical discovery ledger', async () => {
    const ledger = new ZipLayoutLedgerV1(RECEIVE_INTENT_DIGEST, ARTIFACT_DIGEST)
    const root = ledger.append({ kind: 'directory', path: ['root'] })
    const file = ledger.append({ kind: 'file', path: ['root', 'a'], exactSize: 3n })

    expect(file.localHeaderOffset).toBe(root.entryStreamBytes)
    await expect(ledger.seal()).rejects.toThrow(/successful discovery/u)
    ledger.completeDiscovery(DISCOVERY_DIGEST)
    const plan = await ledger.seal()

    expect(plan.evidence).toEqual({
      kind: 'progressive',
      discoveryLedgerDigest: DISCOVERY_DIGEST,
    })
    expect(ledger.acceptsSealedPlan(plan)).toBe(true)
    await expect(validateSealedZipLayoutPlan(plan)).resolves.toEqual(plan)
  })

  it('rejects out-of-order discovery and permanently invalidates member or discovery failures', async () => {
    const unordered = new ZipLayoutLedgerV1(RECEIVE_INTENT_DIGEST, ARTIFACT_DIGEST)
    unordered.append({ kind: 'directory', path: ['root'] })
    unordered.append({ kind: 'file', path: ['root', 'z'], exactSize: 0n })
    expect(() => unordered.append({ kind: 'file', path: ['root', 'a'], exactSize: 0n }))
      .toThrow(/canonical order/u)

    const failed = new ZipLayoutLedgerV1(RECEIVE_INTENT_DIGEST, ARTIFACT_DIGEST)
    failed.append({ kind: 'directory', path: ['root'] })
    failed.recordSelectedMemberFailure()
    expect(() => failed.completeDiscovery(DISCOVERY_DIGEST)).toThrow(/cannot be completed/u)
    await expect(failed.seal()).rejects.toThrow(/successful discovery/u)

    const discoveryFailed = new ZipLayoutLedgerV1(RECEIVE_INTENT_DIGEST, ARTIFACT_DIGEST)
    discoveryFailed.append({ kind: 'directory', path: ['root'] })
    discoveryFailed.recordDiscoveryFailure()
    expect(() => discoveryFailed.completeDiscovery(DISCOVERY_DIGEST)).toThrow(/cannot be completed/u)
    await expect(discoveryFailed.seal()).rejects.toThrow(/successful discovery/u)
  })
})

function digest(fill: number): string {
  return encodeBase64Url(new Uint8Array(32).fill(fill))
}
