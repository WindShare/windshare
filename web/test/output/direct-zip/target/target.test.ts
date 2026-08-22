import { describe, expect, it } from 'vitest'

import {
  createDirectZipBrowserFileSystemPort,
  createDirectZipTarget,
  directZipStableTargetName,
} from '../../../../src/output/direct-zip/target'
import {
  encodeDirectZipBootstrapPrefixV1,
  type DirectZipOwnershipMarkerInputV1,
} from '../../../../src/output/direct-zip/format'
import {
  StagedFsaModel,
  bootstrapCheckpoint,
  bytes,
} from './staged-fsa-model'

const ROOT = 'photos'
const OPERATION_ID = bytes(16, 0x71)

describe('DirectZip exact-name reservation and bootstrap cut', () => {
  it('persists each exact candidate before lookup, retires occupation, and coordinates both locks', async () => {
    const model = new StagedFsaModel()
    const occupiedName = directZipStableTargetName(ROOT, bytes(16, 1))
    model.occupyDirectory(occupiedName)

    const result = await reserve(model)

    expect(result.kind).toBe('ready')
    if (result.kind !== 'ready') return
    expect(model.retired).toMatchObject([{ reason: 'occupied-name' }])
    expect(result.value.binding.stableName).toBe(directZipStableTargetName(ROOT, bytes(16, 3)))
    expect(model.calls.indexOf(`candidate:persist:${occupiedName}`)).toBeLessThan(
      model.calls.indexOf(`lookup:${occupiedName}`),
    )
    expect(model.calls.slice(0, 2)).toEqual(['lock:operation:acquire', 'lock:parent:acquire'])
    expect(model.calls.slice(-2)).toEqual(['lock:parent:release', 'lock:operation:release'])
    expect(model.calls).not.toContain(`writable:${occupiedName}:false`)
  })

  it('fails closed after the bounded reservation set is entirely occupied', async () => {
    const model = new StagedFsaModel()
    for (let seed = 1; seed <= 15; seed += 2) {
      model.occupyDirectory(directZipStableTargetName(ROOT, bytes(16, seed)))
    }

    const result = await reserve(model)

    expect(result).toMatchObject({
      kind: 'gated',
      decision: { kind: 'needs-attention', reason: 'reservation-exhausted' },
    })
    expect(model.retired).toHaveLength(8)
    expect(model.calls.some(call => call.startsWith('writable:'))).toBe(false)
  })

  it('allocates one portable exact name with the complete 128-bit candidate token', () => {
    const stableName = directZipStableTargetName('界'.repeat(80), bytes(16, 0xee))
    expect(new TextEncoder().encode(stableName).byteLength).toBeLessThanOrEqual(255)
    expect(stableName).toMatch(/\.windshare-[A-Za-z0-9_-]{22}\.zip$/u)
    expect(stableName).not.toContain('=')
  })

  it.each([
    { label: 'partial prefix', contents: bytes(12, 0x50), reason: 'occupied-name' },
    { label: 'wrong marker', contents: foreignPrefix(), reason: 'bootstrap-marker-mismatch' },
  ])('never opens a writer on an occupied $label', async ({ contents, reason }) => {
    const model = new StagedFsaModel()
    const occupiedName = directZipStableTargetName(ROOT, bytes(16, 1))
    const occupied = model.installFile(occupiedName, contents)

    const result = await reserve(model)

    expect(result.kind).toBe('ready')
    expect(model.retired[0]?.reason).toBe(reason)
    expect(model.calls).not.toContain(`writable:${occupied.node.id}:false`)
    expect(model.fileBytes(occupiedName)).toEqual(contents)
  })

  it('accepts a close exception only after fresh readback proves the complete prefix', async () => {
    const model = new StagedFsaModel()
    model.faultOnce('close-after-publication', domError('OperationError'))

    const result = await reserve(model)

    expect(result.kind).toBe('ready')
    if (result.kind !== 'ready') return
    expect(result.value.observation.marker.kind).toBe('matching')
    expect(result.value.observation.size).toBe(result.value.binding.bootstrapPrefixLength)
    expect(model.trace).toContainEqual(expect.objectContaining({
      name: 'direct_zip.target.bootstrap',
      outcome: 'throw-after-publication-proven',
      native_error_name: 'OperationError',
    }))
  })

  it('retains an unpublished bootstrap and reports destination space without blind close retry', async () => {
    const model = new StagedFsaModel()
    model.faultOnce('close-before-publication', domError('QuotaExceededError'))

    const result = await reserve(model)

    expect(result).toMatchObject({
      kind: 'gated',
      decision: { kind: 'destination-space-required', stage: 'bootstrap-close' },
      retainedEffect: { stableName: directZipStableTargetName(ROOT, bytes(16, 1)) },
    })
    expect(model.retired).toHaveLength(0)
    expect(model.calls.filter(call => call.startsWith('snapshot:'))).toHaveLength(2)
  })

  it('retains an absent candidate for retry when exact creation reports destination space', async () => {
    const model = new StagedFsaModel()
    model.faultOnce('create', domError('QuotaExceededError'))

    const result = await reserve(model)

    expect(result).toMatchObject({
      kind: 'gated',
      decision: { kind: 'destination-space-required', stage: 'exact-name-create' },
      retainedEffect: { stableName: directZipStableTargetName(ROOT, bytes(16, 1)) },
    })
    expect(model.retired).toHaveLength(0)
  })

  it('resumes an exact persisted candidate idempotently without rewriting its prefix', async () => {
    const first = new StagedFsaModel()
    const reserved = await reserve(first)
    if (reserved.kind !== 'ready') throw new Error('test bootstrap failed')
    const candidate = reserved.value.binding

    const recovered = await createDirectZipTarget(first.dependencies()).resumeBootstrap({
      candidate,
      currentParent: first.parent,
      trustedAction: false,
    })
    expect(recovered).toMatchObject({
      kind: 'ready',
      value: { recoveredExistingPrefix: true },
    })
    expect(first.calls.filter(call => call.startsWith('writable:'))).toHaveLength(1)
  })

  it('keeps the real browser adapter default-off until reviewed support is injected', () => {
    expect(() => createDirectZipBrowserFileSystemPort()).toThrowError(
      expect.objectContaining({ name: 'NotSupportedError' }),
    )
  })
})

describe('DirectZip permission, reopen, truncate, and close observation', () => {
  it('requests permission only in a trusted action and returns authorization-required otherwise', async () => {
    const prompt = new StagedFsaModel()
    prompt.queryPermissionState = 'prompt'
    const untrusted = await reserve(prompt, false)
    expect(untrusted).toMatchObject({
      kind: 'gated',
      decision: { kind: 'authorization-required', reason: 'permission-prompt' },
    })
    expect(prompt.calls).not.toContain('permission:request')
    expect(prompt.candidates).toHaveLength(0)

    const denied = new StagedFsaModel()
    denied.queryPermissionState = 'prompt'
    denied.requestPermissionState = 'denied'
    const trusted = await reserve(denied, true)
    expect(trusted).toMatchObject({
      kind: 'gated',
      decision: { kind: 'authorization-required', reason: 'permission-denied' },
    })
    expect(denied.calls).toContain('permission:request')
  })

  it('classifies target deletion as restart-required and never recreates it on reopen', async () => {
    const fixture = await ownedFixture()
    fixture.model.deleteFile(fixture.binding.stableName)

    const reopened = await fixture.target.reopen(fixture.request())

    expect(reopened).toMatchObject({
      kind: 'gated',
      decision: { kind: 'restart-required', reason: 'target-deleted' },
    })
    expect(fixture.model.calls.filter(call => call.startsWith('create:'))).toHaveLength(1)
  })

  it('detects same-name same-size valid foreign replacement without trusting handle or mtime', async () => {
    const fixture = await ownedFixture()
    fixture.model.replaceFile(fixture.binding.stableName, foreignPrefix())

    const reopened = await fixture.target.reopen({
      ...fixture.request(),
      verifyChangedEvidence: false,
    })

    expect(reopened).toMatchObject({
      kind: 'gated',
      decision: { kind: 'needs-attention', reason: 'foreign-replacement' },
    })
  })

  it('holds a partial established marker at target-verification-required', async () => {
    const fixture = await ownedFixture()
    fixture.model.replaceFile(fixture.binding.stableName, bytes(20, 0x50))

    const reopened = await fixture.target.reopen({
      ...fixture.request(),
      verifyChangedEvidence: false,
    })

    expect(reopened).toMatchObject({
      kind: 'gated',
      decision: {
        kind: 'target-verification-required',
        reason: 'ownership-marker-incomplete',
      },
    })

    await expect(fixture.target.reopen({
      ...fixture.request(),
      verifyChangedEvidence: true,
    })).resolves.toMatchObject({
      kind: 'gated',
      decision: { kind: 'needs-attention', reason: 'committed-prefix-lost' },
    })
  })

  it('verifies the predecessor before truncating unknown tail and rechecks after an ambiguous close', async () => {
    const fixture = await ownedFixture()
    fixture.model.replaceFile(
      fixture.binding.stableName,
      concatenate(fixture.archive, bytes(7, 0xa5)),
    )
    fixture.model.faultOnce('close-after-publication', domError('OperationError'))

    const truncated = await fixture.target.truncateToPredecessor(fixture.request())

    expect(truncated).toMatchObject({ kind: 'ready', value: { disposition: 'truncated' } })
    expect(fixture.model.fileBytes(fixture.binding.stableName)).toEqual(fixture.archive)
  })

  it('keeps the verified predecessor when truncate close runs out of destination space', async () => {
    const fixture = await ownedFixture()
    const tailed = concatenate(fixture.archive, bytes(5, 0xa6))
    fixture.model.replaceFile(fixture.binding.stableName, tailed)
    fixture.model.faultOnce('close-before-publication', domError('QuotaExceededError'))

    const result = await fixture.target.truncateToPredecessor(fixture.request())

    expect(result).toMatchObject({
      kind: 'gated',
      decision: { kind: 'destination-space-required', stage: 'epoch-close' },
    })
    expect(fixture.model.fileBytes(fixture.binding.stableName)).toEqual(tailed)
  })

  it('observes an external replacement after close and does not translate a thrown close into rollback', async () => {
    const fixture = await ownedFixture()
    const opened = await fixture.target.openEpoch(fixture.request())
    if (opened.kind !== 'ready') throw new Error('epoch did not open')
    await expect(opened.value.write(BigInt(fixture.archive.byteLength), bytes(4, 0xaa)))
      .resolves.toMatchObject({ kind: 'ready' })
    const replacement = foreignPrefix()
    fixture.model.hookOnce('close-after-publication', () => {
      fixture.model.replaceFile(fixture.binding.stableName, replacement)
    })
    fixture.model.faultOnce('close-after-publication', domError('OperationError'))

    const closed = await opened.value.closeAndObserve()

    expect(closed).toMatchObject({
      kind: 'ready',
      value: {
        observation: { marker: { kind: 'foreign' }, fileLocator: 'different' },
        nativeCloseError: { name: 'OperationError' },
      },
    })
    expect(fixture.model.fileBytes(fixture.binding.stableName)).toEqual(replacement)
  })

  it('reports destination-space-required at writable open and write stages', async () => {
    const opening = await ownedFixture()
    opening.model.faultOnce('writable-open', domError('QuotaExceededError'))
    await expect(opening.target.openEpoch(opening.request())).resolves.toMatchObject({
      kind: 'gated',
      decision: { kind: 'destination-space-required', stage: 'epoch-open' },
    })

    const writing = await ownedFixture()
    const opened = await writing.target.openEpoch(writing.request())
    if (opened.kind !== 'ready') throw new Error('epoch did not open')
    writing.model.faultOnce('write', domError('QuotaExceededError'))
    await expect(opened.value.write(0n, bytes(1, 9))).resolves.toMatchObject({
      kind: 'gated',
      decision: { kind: 'destination-space-required', stage: 'epoch-write' },
    })
    await opened.value.abortAndObserve()
  })
})

describe('DirectZip ownership-proven cleanup', () => {
  it('rehashes every committed epoch under both locks before deleting and proves absence', async () => {
    const fixture = await ownedFixture()

    const result = await fixture.target.deleteProvenTarget({
      binding: fixture.binding,
      currentParent: fixture.model.parent,
      predecessor: fixture.checkpoint,
      trustedAction: false,
    })

    expect(result).toEqual({ kind: 'ready', value: { disposition: 'deleted' } })
    expect(fixture.model.fileBytes(fixture.binding.stableName)).toBeUndefined()
    expect(fixture.model.trace).toContainEqual(expect.objectContaining({
      name: 'direct_zip.target.cleanup',
      outcome: 'deleted-and-absence-proven',
    }))
  })

  it.each(['remove-before', 'remove-after'] as const)(
    'returns needs-attention when native cleanup refuses at $stage',
    async stage => {
      const fixture = await ownedFixture()
      fixture.model.faultOnce(stage, domError('OperationError'))

      const result = await fixture.target.deleteProvenTarget({
        binding: fixture.binding,
        currentParent: fixture.model.parent,
        predecessor: fixture.checkpoint,
        trustedAction: false,
      })

      expect(result).toMatchObject({
        kind: 'gated',
        decision: { kind: 'needs-attention', reason: 'cleanup-refused' },
      })
    },
  )

  it('refuses cleanup after replacement and preserves the foreign target', async () => {
    const fixture = await ownedFixture()
    const replacement = foreignPrefix()
    fixture.model.replaceFile(fixture.binding.stableName, replacement)

    const result = await fixture.target.deleteProvenTarget({
      binding: fixture.binding,
      currentParent: fixture.model.parent,
      predecessor: fixture.checkpoint,
      trustedAction: false,
    })

    expect(result).toMatchObject({
      kind: 'gated',
      decision: { kind: 'needs-attention', reason: 'foreign-replacement' },
    })
    expect(fixture.model.fileBytes(fixture.binding.stableName)).toEqual(replacement)
    expect(fixture.model.calls.some(call => call.startsWith('remove:'))).toBe(false)
  })
})

async function reserve(model: StagedFsaModel, trustedAction = false) {
  return createDirectZipTarget(model.dependencies()).reserveBootstrap({
    operationId: OPERATION_ID,
    resultRootComponent: ROOT,
    parentBinding: model.parentBinding(),
    currentParent: model.parent,
    trustedAction,
  })
}

async function ownedFixture() {
  const model = new StagedFsaModel()
  const target = createDirectZipTarget(model.dependencies())
  const reserved = await target.reserveBootstrap({
    operationId: OPERATION_ID,
    resultRootComponent: ROOT,
    parentBinding: model.parentBinding(),
    currentParent: model.parent,
    trustedAction: false,
  })
  if (reserved.kind !== 'ready') throw new Error('fixture reservation failed')
  const binding = reserved.value.binding
  const archive = model.fileBytes(binding.stableName)
  if (archive === undefined) throw new Error('fixture archive is missing')
  const checkpoint = bootstrapCheckpoint(archive, reserved.value.observation)
  return Object.freeze({
    model,
    target,
    binding,
    archive,
    checkpoint,
    request: () => ({
      binding,
      currentParent: model.parent,
      predecessor: checkpoint,
      trustedAction: false,
    }),
  })
}

function foreignPrefix(): Uint8Array {
  const marker: DirectZipOwnershipMarkerInputV1 = {
    operationId: bytes(16, 0x81),
    candidateId: bytes(16, 0x82),
    ownershipNonce: bytes(32, 0x83),
    bindingDigest: bytes(32, 0x84),
  }
  return encodeDirectZipBootstrapPrefixV1(ROOT, marker)
}

function concatenate(...parts: readonly Uint8Array[]): Uint8Array {
  const result = new Uint8Array(parts.reduce((sum, part) => sum + part.byteLength, 0))
  let offset = 0
  for (const part of parts) {
    result.set(part, offset)
    offset += part.byteLength
  }
  return result
}

function domError(name: string): DOMException {
  return new DOMException(name, name)
}
