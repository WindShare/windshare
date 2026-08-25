import { describe, expect, it, vi } from 'vitest'
import { V2SelectionPolicy } from '../../src/catalog/v2-selection'
import {
  OutputSessionBindingError,
  VerifiedDurableRanges,
  VerifiedFinalOutputFile,
  disabledOutputExecutionProfile,
  outputCapabilities,
  outputExecutionProfile,
  snapshotOpenedOutputRevision,
  snapshotOutputFileRequest,
  validatePlanExecutionBinding,
  type DirectTreeExecution,
  type OutputSession,
} from '../../src/transfer/output-session'
import {
  createDirectTreePlan,
  createFSANamedEntryReservation,
  createNativeNamedEntryReservation,
  createReceiveIntent,
  createSelectionSpec,
  createSingleFileDirectoryTreeArtifact,
} from '../../src/transfer/intent'
import {
  createDirectTreeCoordinateContract,
  snapshotLogicalArtifactPath,
  snapshotMaterializationRootRelativePath,
  snapshotSourceAuthenticationPath,
} from '../../src/transfer/job/coordinate/direct-tree'
import {
  fileEntry,
  identity,
  identityText,
  digestIdentity,
  planAuthorityFixture,
  receiveIntentFixture,
} from './v2-job-fixture'

describe('plan-specific output boundary', () => {
  it('has no representation format and preserves every semantic path coordinate', () => {
    const openRevision = vi.fn(async () => snapshotOpenedOutputRevision({
      shareInstance: identityText(1),
      fileId: identityText(4),
      fileRevision: identityText(5),
      exactSize: 3n,
    }))
    const request = snapshotOutputFileRequest({
      source: { shareInstance: identityText(1), fileId: identityText(4) },
      sourceAuthenticationPath: snapshotSourceAuthenticationPath(['source', 'file.bin']),
      logicalArtifactPath: snapshotLogicalArtifactPath(['result', 'renamed.bin']),
      materializationRelativePath: snapshotMaterializationRootRelativePath(['renamed.bin']),
      expectedSize: 3n,
      openRevision,
    })
    expect(request.sourceAuthenticationPath).toEqual(['source', 'file.bin'])
    expect(request.logicalArtifactPath).toEqual(['result', 'renamed.bin'])
    expect(request.materializationRelativePath).toEqual(['renamed.bin'])
    expect('format' in request).toBe(false)
    expect(request.openRevision).toBe(openRevision)
  })

  it('admits the reserved-root file coordinate only from its validated FSA single-file plan', async () => {
    const artifact = await createSingleFileDirectoryTreeArtifact({
      fileId: identityText(4),
      sourcePath: 'source/file.bin',
      outputName: 'file.bin',
    })
    const selection = await createSelectionSpec({
      shareInstance: identityText(1),
      syntheticRoot: identityText(2),
      rules: { mode: 'node-id', defaultSelected: true, rules: [] },
    })
    const reservation = await createFSANamedEntryReservation({
      operationId: identityText(6),
      reservationId: identityText(7),
      artifact,
      authorityRef: digestIdentity(8),
      logicalReservedName: 'file.bin',
      physicalName: 'file.bin',
      collisionIndex: 0,
    })
    const intent = await createReceiveIntent({
      selection,
      artifact,
      plan: await createDirectTreePlan(artifact, reservation),
    })
    const coordinates = await createDirectTreeCoordinateContract(intent)
    const nativeReservation = await createNativeNamedEntryReservation({
      operationId: identityText(9),
      reservationId: identityText(10),
      artifact,
      authorityRef: digestIdentity(11),
      logicalReservedName: 'file.bin',
      collisionIndex: 0,
    })
    const nativeCoordinates = await createDirectTreeCoordinateContract(await createReceiveIntent({
      selection,
      artifact,
      plan: await createDirectTreePlan(artifact, nativeReservation),
    }))
    const request = {
      source: { shareInstance: identityText(1), fileId: identityText(4) },
      sourceAuthenticationPath: snapshotSourceAuthenticationPath(['source', 'file.bin']),
      logicalArtifactPath: snapshotLogicalArtifactPath(['file.bin']),
      materializationRelativePath: snapshotMaterializationRootRelativePath([]),
      expectedSize: 3n,
      openRevision: vi.fn(),
    }

    expect(() => snapshotOutputFileRequest(request)).toThrow(/coordinates must identify a file/u)
    expect(() => snapshotOutputFileRequest(request, nativeCoordinates))
      .toThrow(/coordinates must identify a file/u)
    expect(snapshotOutputFileRequest(request, coordinates).materializationRelativePath).toEqual([])
  })

  it('rejects malformed opened revisions and noncanonical output identities', () => {
    expect(() => snapshotOpenedOutputRevision({
      shareInstance: identityText(1),
      fileId: identityText(4),
      fileRevision: identityText(5),
      exactSize: -1n,
    })).toThrow(/size is invalid/)
    expect(() => new VerifiedDurableRanges({
      backend: 'test',
      outputSessionId: 'session',
      canonicalPath: ['file.bin'],
      ownedFileIdentity: 'owned',
    }, {
      shareInstance: identityText(1),
      fileId: identityText(4),
      fileRevision: identityText(5),
    }, 4n, [{ start: 2n, end: 4n }, { start: 1n, end: 2n }]))
      .toThrow(/sorted, non-overlapping/)
  })

  it('validates and snapshots execution policy independently from capabilities', () => {
    const profile = {
      maximumConcurrentFilePipelines: 3,
      maximumOutstandingWriteBytes: 8n,
      maximumBufferedBytes: 5n,
      automaticCheckpoint: {
        kind: 'bounded' as const,
        trigger: { pendingBytes: 4n, pendingMilliseconds: 50 },
        costBudget: {
          maximumPrefixCopyBytes: 2n,
          maximumCumulativeWriteAmplificationBytes: 3n,
          maximumPeakTemporaryBytes: 4n,
        },
      },
    }
    const validated = outputExecutionProfile(profile)
    profile.automaticCheckpoint.trigger.pendingBytes = 7n
    expect(validated).toMatchObject({
      maximumConcurrentFilePipelines: 3,
      automaticCheckpoint: { trigger: { pendingBytes: 4n } },
    })
    expect(() => outputExecutionProfile({ ...profile, maximumConcurrentFilePipelines: 0 }))
      .toThrow(/concurrent file-pipeline limit/u)
    expect(() => outputExecutionProfile({
      ...profile,
      automaticCheckpoint: {
        ...profile.automaticCheckpoint,
        trigger: { ...profile.automaticCheckpoint.trigger, pendingMilliseconds: 0 },
      },
    })).toThrow(/checkpoint trigger/u)
  })

  it('snapshots final proof identity for durable ledger consumers', () => {
    const canonicalPath = ['chosen', 'result.bin']
    const proof = new VerifiedFinalOutputFile({
      backend: 'test',
      outputSessionId: 'session',
      canonicalPath,
      ownedFileIdentity: 'owned',
    }, {
      shareInstance: identityText(1),
      fileId: identityText(4),
      fileRevision: identityText(5),
    }, 4n)
    canonicalPath[0] = 'mutated'
    expect(proof.ownership.canonicalPath).toEqual(['chosen', 'result.bin'])
    expect(Object.isFrozen(proof)).toBe(true)
  })

  it('requires DirectTree to expose incremental directory authority', async () => {
    const selection = new V2SelectionPolicy()
    const intent = await receiveIntentFixture({
      planKind: 'direct-tree',
      artifactKind: 'directory-tree',
      selection,
    })
    const output: OutputSession = {
      identity: { backend: 'test', outputSessionId: 'session' },
      capabilities: outputCapabilities({
        durability: 'None',
        randomWrite: false,
        fileFailureIsolation: true,
        modificationTime: false,
      }),
      executionProfile: disabledOutputExecutionProfile(1),
      beginFile: async () => { throw new Error('not used') },
    }
    const malformed = {
      planKind: 'direct-tree',
      output,
      settle: async () => { throw new Error('not used') },
      pause: async () => { throw new Error('not used') },
    } as unknown as DirectTreeExecution
    expect(() => validatePlanExecutionBinding(intent, malformed))
      .toThrow(OutputSessionBindingError)
  })

  it('accepts the explicit plan adapter without consulting a session format', async () => {
    const file = fileEntry(identity(4), 'file.bin', 0n)
    const selection = new V2SelectionPolicy(false)
    selection.toggle(file, [identityText(2)])
    const intent = await receiveIntentFixture({
      planKind: 'direct-atomic',
      artifactKind: 'original-file',
      selection,
      file,
    })
    const authority = planAuthorityFixture()
    const execution = await authority.openDirectAtomic(
      intent as Parameters<typeof authority.openDirectAtomic>[0],
      new AbortController().signal,
    )
    expect(validatePlanExecutionBinding(intent, execution).planKind).toBe('direct-atomic')
    expect('format' in execution.output).toBe(false)
  })
})
