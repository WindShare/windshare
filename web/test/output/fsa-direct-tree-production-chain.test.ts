import { describe, expect, it } from 'vitest'

import { identityText } from '../transfer/v2-job-fixture'
import {
  FSA_PRODUCTION_PARTIAL_FILE_BYTES,
  runFSAProductionChain,
  runFSAProductionPersistenceChain,
  type FSAProductionChainScenarioKind,
  type FSAProductionPersistenceVariant,
  type ObservedDirectoryAdmission,
} from './fsa-direct-tree-production-chain-fixture'

interface ProductionExpectation {
  readonly scenario: FSAProductionChainScenarioKind
  readonly directoryAdmissions: readonly ObservedDirectoryAdmission[]
  readonly physicalEntries: readonly string[]
}

interface PersistenceExpectation {
  readonly scenario: Exclude<FSAProductionChainScenarioKind, 'single-file'>
  readonly variant: FSAProductionPersistenceVariant
  readonly rootDirectoryId: string
  readonly filePath: readonly string[]
}

const PRODUCTION_EXPECTATIONS: readonly ProductionExpectation[] = Object.freeze([
  Object.freeze({
    scenario: 'single-file',
    directoryAdmissions: Object.freeze([]),
    physicalEntries: Object.freeze(['report.bin']),
  }),
  Object.freeze({
    scenario: 'directory-anchor',
    directoryAdmissions: Object.freeze([
      Object.freeze({ directoryId: identityText(30), path: Object.freeze([]) }),
    ]),
    physicalEntries: Object.freeze([
      'photos/',
      'photos/image.bin',
    ]),
  }),
  Object.freeze({
    scenario: 'synthetic-root',
    directoryAdmissions: Object.freeze([
      Object.freeze({ directoryId: identityText(2), path: Object.freeze([]) }),
    ]),
    physicalEntries: Object.freeze([
      'windshare/',
      'windshare/payload.bin',
    ]),
  }),
])

const PERSISTENCE_EXPECTATIONS: readonly PersistenceExpectation[] = Object.freeze([
  Object.freeze({
    scenario: 'directory-anchor',
    variant: 'pause-resume',
    rootDirectoryId: identityText(30),
    filePath: Object.freeze(['image.bin']),
  }),
  Object.freeze({
    scenario: 'synthetic-root',
    variant: 'pause-resume',
    rootDirectoryId: identityText(2),
    filePath: Object.freeze(['payload.bin']),
  }),
  Object.freeze({
    scenario: 'directory-anchor',
    variant: 'collision-reserved-root',
    rootDirectoryId: identityText(30),
    filePath: Object.freeze(['image.bin']),
  }),
  Object.freeze({
    scenario: 'synthetic-root',
    variant: 'compatible-name-restoration',
    rootDirectoryId: identityText(2),
    filePath: Object.freeze(['blocked.txt']),
  }),
])

describe('FSA DirectTree production coordinate chain', () => {
  it.each(PRODUCTION_EXPECTATIONS)(
    'publishes $scenario through one reserved-root-relative layout',
    async ({ scenario, directoryAdmissions, physicalEntries }) => {
      const observed = await runFSAProductionChain(scenario)

      expect.soft(observed.settlementFailure).toBeUndefined()
      expect.soft(observed.workerStatus).toBe('Succeeded')
      expect.soft(observed.lifecycleKind).toBe('published')
      expect.soft(observed.directoryAdmissionAttempts).toBe(directoryAdmissions.length)
      expect.soft(observed.directoryAdmissions).toEqual(directoryAdmissions)
      expect.soft(observed.physicalEntries).toEqual(physicalEntries)
    },
  )

  it.each(PERSISTENCE_EXPECTATIONS)(
    'keeps $scenario coordinates durable through $variant',
    async ({ scenario, variant, rootDirectoryId, filePath }) => {
      const observed = await runFSAProductionPersistenceChain(scenario, variant)

      expect.soft(observed.firstWorkerStatus).toBe('Paused')
      expect.soft(observed.firstLifecycleKind).toBe('resumable-receive')
      expect.soft(observed.resumedWorkerStatus).toBe('Succeeded')
      expect.soft(observed.resumedLifecycleKind).toBe('published')
      expect.soft(observed.directoryAdmissions).toEqual([
        { phase: 'initial', directoryId: rootDirectoryId, path: [] },
        { phase: 'reopened', directoryId: rootDirectoryId, path: [] },
      ])
      expect.soft(observed.fileRequests).toEqual([
        { phase: 'initial', path: filePath },
        { phase: 'reopened', path: filePath },
      ])
      expect.soft(observed.checkpointLookups).toEqual([
        { phase: 'initial', path: filePath },
        { phase: 'reopened', path: filePath },
      ])
      expect.soft(observed.checkpointWrites.length).toBeGreaterThan(0)
      expect.soft(observed.checkpointWrites.every(value =>
        value.path.length === filePath.length &&
        value.path.every((segment, index) => segment === filePath[index]))).toBe(true)
      expect.soft(new Set(observed.checkpointWrites.map(value => value.phase))).toEqual(
        new Set(['initial', 'reopened']),
      )
      expect.soft(observed.reopenedDurableRanges).toEqual([{
        path: filePath,
        ranges: [{ start: 0n, end: FSA_PRODUCTION_PARTIAL_FILE_BYTES }],
      }])
      expect.soft(observed.pauseEvidenceKind).toBe('direct-tree-ledger')
      expect.soft(observed.settlementEvidenceKind).toBe('direct-tree-ledger')
      expect.soft(observed.materializationSealCount).toBe(2)
      expect.soft(observed.settlementFinalProofReadCount).toBe(0)
      expect.soft(observed.settlementReceiptPersisted).toBe(true)

      if (variant === 'collision-reserved-root') {
        expect.soft(observed.reservation.collisionIndex).toBe(1)
        expect.soft(observed.reservation.physicalName).not.toBe(observed.reservation.requestedName)
        expect.soft(observed.parentPhysicalEntries).toContain(`${observed.reservation.requestedName}/`)
        expect.soft(observed.parentPhysicalEntries).toContain(`${observed.reservation.physicalName}/`)
      } else {
        expect.soft(observed.reservation.collisionIndex).toBe(0)
      }

      const reopenedFileLookups = observed.physicalLookups
        .filter(value => value.phase === 'reopened' && value.kind === 'file')
        .map(value => value.name)
      if (variant === 'compatible-name-restoration') {
        expect.soft(observed.compatibleNameMapping).toMatchObject({
          logicalPath: filePath,
          commitState: 'committed',
        })
        expect.soft(observed.compatibleNameMapping?.physicalComponent).not.toBe(filePath[0])
        expect.soft(observed.taskRootPhysicalEntries).toContain(
          observed.compatibleNameMapping!.physicalComponent,
        )
        expect.soft(observed.taskRootPhysicalEntries).not.toContain(filePath[0])
        expect.soft(reopenedFileLookups).toContain(observed.compatibleNameMapping!.physicalComponent)
      } else {
        expect.soft(observed.taskRootPhysicalEntries).toEqual(filePath)
        expect.soft(reopenedFileLookups).toContain(filePath[0])
      }
    },
  )
})
