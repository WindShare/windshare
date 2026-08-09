import { describe, expect, it } from 'vitest'

import { encodeBase64Url } from '../../../src/crypto/bytes'
import { createSelectionSpec, type NodeSelectionRule } from '../../../src/transfer/intent'
import {
  MAX_PROJECTION_SELECTED_ROOT_FACTS,
  SelectionProjectionController,
  createAuthenticatedProjectionEvidence,
  reduceSelectionProjection,
  type AuthenticatedProjectionEvidence,
  type ProjectedDirectoryAnchor,
  type ProjectionTraceEvent,
  type SelectedRootFact,
  type SelectionProjectionState,
} from '../../../src/transfer/projection'

const SHARE_INSTANCE = identity(60_001)
const SYNTHETIC_ROOT = identity(60_002)

interface EvidenceInput {
  readonly generationSeed: number
  readonly files?: number
  readonly directories?: number
  readonly bytes?: bigint
  readonly representativeFile?: Readonly<{
    fileId: string
    sourcePath: string
    portableName: string
  }>
  readonly roots?: readonly SelectedRootFact[]
  readonly selectedRootCount?: number
  readonly settledTargets?: SelectionProjectionState['projection']['unsettledTargets']
  readonly syntheticLayout?: boolean
}

describe('epoch-scoped selection projection', () => {
  const shapeCases: readonly Readonly<{
    name: string
    run: () => Promise<SelectionProjectionState>
    expected: 'unknown' | 'none' | 'single-file' | 'tree'
  }>[] = [
    {
      name: 'Unknown while authenticated selection evidence is insufficient',
      expected: 'unknown',
      run: async () => started(await selection()),
    },
    {
      name: 'None after complete negative evidence',
      expected: 'none',
      run: async () => {
        const controller = startedController(await selection())
        return controller.apply({ kind: 'discovery-completed', epoch: controller.state.projection.epoch })
      },
    },
    {
      name: 'explicit empty directory is Tree',
      expected: 'tree',
      run: explicitEmptyDirectoryCase,
    },
    {
      name: 'one file is SingleFile only after its target settles',
      expected: 'single-file',
      run: async () => {
        const controller = startedController(await selection({ defaultSelected: true }))
        const file = fileRoot(101, 'only.txt')
        controller.apply(evidenceEvent(controller, evidence({
          generationSeed: 1,
          files: 1,
          bytes: 9n,
          representativeFile: file,
          roots: [file],
          settledTargets: controller.state.projection.unsettledTargets,
        })))
        return controller.apply({ kind: 'discovery-completed', epoch: controller.state.projection.epoch })
      },
    },
    {
      name: 'one file plus an empty directory is Tree',
      expected: 'tree',
      run: async () => {
        const controller = startedController(await selection({ defaultSelected: true }))
        const file = fileRoot(102, 'one.txt')
        const directory = directoryRoot(103, 'empty')
        controller.apply(evidenceEvent(controller, evidence({
          generationSeed: 2,
          files: 1,
          directories: 1,
          bytes: 3n,
          representativeFile: file,
          roots: [file, directory],
          settledTargets: controller.state.projection.unsettledTargets,
          syntheticLayout: true,
        })))
        return controller.apply({
          kind: 'discovery-completed',
          epoch: controller.state.projection.epoch,
          layoutBasis: { kind: 'synthetic-selection' },
        })
      },
    },
    {
      name: 'full directory is Tree with complete-directory basis',
      expected: 'tree',
      run: async () => selectedDirectoryCase('complete-directory'),
    },
    {
      name: 'partial directory is Tree with directory-selection basis',
      expected: 'tree',
      run: async () => {
        const firstId = identity(201)
        const secondId = identity(202)
        const controller = startedController(await selection({ rules: [
          { kind: 'file', id: firstId, selected: true },
          { kind: 'file', id: secondId, selected: true },
        ] }))
        controller.apply(evidenceEvent(controller, evidence({
          generationSeed: 3,
          files: 2,
          bytes: 8n,
          roots: [
            { kind: 'file', fileId: firstId, sourcePath: 'folder/a.txt', portableName: 'a.txt' },
            { kind: 'file', fileId: secondId, sourcePath: 'folder/b.txt', portableName: 'b.txt' },
          ],
          settledTargets: controller.state.projection.unsettledTargets,
        })))
        return controller.apply({
          kind: 'discovery-completed',
          epoch: controller.state.projection.epoch,
          layoutBasis: {
            kind: 'directory-selection',
            anchor: directoryAnchor(203, 'folder'),
          },
        })
      },
    },
    {
      name: 'multiple synthetic roots are Tree',
      expected: 'tree',
      run: async () => {
        const controller = startedController(await selection({ defaultSelected: true }))
        controller.apply(evidenceEvent(controller, evidence({
          generationSeed: 4,
          files: 2,
          roots: [fileRoot(301, 'a.txt'), fileRoot(302, 'b.txt')],
          settledTargets: controller.state.projection.unsettledTargets,
          syntheticLayout: true,
        })))
        return controller.apply({
          kind: 'discovery-completed',
          epoch: controller.state.projection.epoch,
          layoutBasis: { kind: 'synthetic-selection' },
        })
      },
    },
    {
      name: 'opaque unresolved target keeps one observed file Unknown',
      expected: 'unknown',
      run: async () => {
        const firstId = identity(401)
        const controller = startedController(await selection({ rules: [
          { kind: 'file', id: firstId, selected: true },
          { kind: 'file', id: identity(402), selected: true },
        ] }))
        const firstTarget = controller.state.projection.unsettledTargets[0]
        if (firstTarget === undefined) throw new Error('test target is missing')
        controller.apply(evidenceEvent(controller, evidence({
          generationSeed: 5,
          files: 1,
          representativeFile: fileRoot(401, 'found.txt'),
          roots: [fileRoot(401, 'found.txt')],
          settledTargets: [firstTarget],
        })))
        return controller.state
      },
    },
  ]

  it.each(shapeCases)('$name', async ({ run, expected }) => {
    const state = await run()
    expect(state.projection.proof.kind).toBe(expected)
  })

  it('retains authenticated evidence across retry in the same epoch', async () => {
    const controller = startedController(await selection({ rules: [
      { kind: 'file', id: identity(501), selected: true },
      { kind: 'file', id: identity(502), selected: true },
    ] }))
    const target = controller.state.projection.unsettledTargets[0]
    if (target === undefined) throw new Error('test target is missing')
    controller.apply(evidenceEvent(controller, evidence({
      generationSeed: 6,
      files: 1,
      bytes: 12n,
      representativeFile: fileRoot(501, 'kept.txt'),
      roots: [fileRoot(501, 'kept.txt')],
      settledTargets: [target],
    })))
    const retained = controller.state.projection
    controller.apply({
      kind: 'retryable-failure',
      epoch: retained.epoch,
      reason: 'receiver-reconnecting',
    })
    expect(controller.state.discovery).toEqual({
      kind: 'retryable-failure',
      reason: 'receiver-reconnecting',
    })
    expect(controller.state.projection).toBe(retained)

    controller.apply({ kind: 'retry-started', epoch: retained.epoch })
    expect(controller.state.discovery.kind).toBe('discovering')
    expect(controller.state.projection).toBe(retained)
  })

  it('drops old-epoch progress, failure, and completion without side effects', async () => {
    const traces: ProjectionTraceEvent[] = []
    const controller = new SelectionProjectionController((event) => traces.push(event))
    const spec = await selection({ defaultSelected: true })
    const first = controller.beginSelection(spec)
    controller.apply({ kind: 'discovery-started', epoch: first.projection.epoch })
    const staleEvidence = evidence({
      generationSeed: 7,
      files: 1,
      representativeFile: fileRoot(601, 'stale.txt'),
      roots: [fileRoot(601, 'stale.txt')],
      settledTargets: first.projection.unsettledTargets,
    })

    const current = controller.beginSelection(spec)
    expect(current.projection.epoch).toBe(first.projection.epoch + 1n)
    expect(controller.apply({
      kind: 'authenticated-evidence',
      epoch: first.projection.epoch,
      evidence: staleEvidence,
    })).toBe(current)
    expect(controller.apply({
      kind: 'retryable-failure',
      epoch: first.projection.epoch,
      reason: 'generation-replay-interrupted',
    })).toBe(current)
    expect(controller.apply({
      kind: 'discovery-completed',
      epoch: first.projection.epoch,
      settledTargets: first.projection.unsettledTargets,
    })).toBe(current)
    expect(controller.state).toBe(current)
    expect(traces.filter((event) => event.name === 'receive.projection.stale_event_dropped'))
      .toHaveLength(3)
  })

  it('proves Tree early and never revokes it while unrelated targets remain', async () => {
    const directoryId = identity(701)
    const controller = startedController(await selection({ rules: [
      { kind: 'directory', id: directoryId, selected: true },
      { kind: 'file', id: identity(702), selected: true },
    ] }))
    const directoryTarget = controller.state.projection.unsettledTargets.find((target) =>
      target.kind === 'node-id' && target.nodeKind === 'directory')
    if (directoryTarget === undefined) throw new Error('directory target is missing')
    controller.apply(evidenceEvent(controller, evidence({
      generationSeed: 8,
      directories: 1,
      roots: [directoryRoot(701, 'selected')],
      settledTargets: [directoryTarget],
    })))

    expect(controller.state.discovery.kind).toBe('discovering')
    expect(controller.state.projection.unsettledTargets).toHaveLength(1)
    expect(controller.state.projection.proof).toMatchObject({
      kind: 'tree',
      layoutBasis: { kind: 'unsettled' },
    })

    const before = controller.state.projection.proof
    controller.apply(evidenceEvent(controller, evidence({
      generationSeed: 9,
      files: 1,
      representativeFile: fileRoot(702, 'late.txt'),
      roots: [fileRoot(702, 'late.txt')],
      settledTargets: controller.state.projection.unsettledTargets,
      syntheticLayout: true,
    })))
    expect(before.kind).toBe('tree')
    expect(controller.state.projection.proof).toMatchObject({
      kind: 'tree',
      layoutBasis: { kind: 'synthetic-selection' },
    })
  })

  it('keeps final scalar proof monotonic while treating an authenticated replay as idempotent', async () => {
    const controller = startedController(await selection({ defaultSelected: true }))
    const file = fileRoot(750, 'only.txt')
    const firstEvidence = evidence({
      generationSeed: 12,
      files: 1,
      representativeFile: file,
      roots: [file],
      settledTargets: controller.state.projection.unsettledTargets,
    })
    controller.apply(evidenceEvent(controller, firstEvidence))
    const proven = controller.state
    expect(proven.projection.proof.kind).toBe('single-file')
    expect(controller.apply(evidenceEvent(controller, firstEvidence))).toBe(proven)

    expect(() => controller.apply(evidenceEvent(controller, evidence({
      generationSeed: 13,
      files: 1,
      representativeFile: fileRoot(751, 'contradiction.txt'),
      roots: [fileRoot(751, 'contradiction.txt')],
    })))).toThrow(/final scalar proof/u)
    expect(controller.state).toBe(proven)
  })

  it('retains bounded root topology even when the authenticated lower bound is larger', async () => {
    const controller = startedController(await selection({ defaultSelected: true }))
    const roots = Array.from({ length: MAX_PROJECTION_SELECTED_ROOT_FACTS }, (_, index) =>
      fileRoot(10_000 + index, `f-${index}.txt`))
    const selectedRootCount = MAX_PROJECTION_SELECTED_ROOT_FACTS + 50
    controller.apply(evidenceEvent(controller, evidence({
      generationSeed: 10,
      files: selectedRootCount,
      roots,
      selectedRootCount,
      settledTargets: controller.state.projection.unsettledTargets,
      syntheticLayout: true,
    })))

    expect(controller.state.projection.selectedRoots).toHaveLength(MAX_PROJECTION_SELECTED_ROOT_FACTS)
    expect(controller.state.projection.selectedRootCountLowerBound).toBe(selectedRootCount)
    expect(controller.state.projection.selectedRootsTruncated).toBe(true)
    expect(controller.state.projection.proof.kind).toBe('tree')
  })

  it('rejects structurally forged catalog evidence', async () => {
    const state = started(await selection())
    const forged = {
      generations: [{ directoryId: identity(801), generation: identity(802) }],
      metrics: { fileCountLowerBound: 0, directoryCountLowerBound: 0, byteCountLowerBound: 0n },
      selectedRoots: [],
      selectedRootCount: 0,
      settledTargets: [],
    } as unknown as AuthenticatedProjectionEvidence
    expect(() => reduceSelectionProjection(state, {
      kind: 'authenticated-evidence',
      epoch: state.projection.epoch,
      evidence: forged,
    })).toThrow(/authenticated construction/u)
  })
})

async function selectedDirectoryCase(
  basis: 'complete-directory',
): Promise<SelectionProjectionState> {
  const directoryId = identity(901)
  const controller = startedController(await selection({ rules: [
    { kind: 'directory', id: directoryId, selected: true },
  ] }))
  controller.apply(evidenceEvent(controller, evidence({
    generationSeed: 11,
    directories: 1,
    roots: [directoryRoot(901, 'folder')],
    settledTargets: controller.state.projection.unsettledTargets,
  })))
  return controller.apply({
    kind: 'discovery-completed',
    epoch: controller.state.projection.epoch,
    layoutBasis: { kind: basis, anchor: directoryAnchor(901, 'folder') },
  })
}

async function explicitEmptyDirectoryCase(): Promise<SelectionProjectionState> {
  const directoryId = identity(950)
  const controller = startedController(await selection({ rules: [
    { kind: 'directory', id: directoryId, selected: true },
  ] }))
  controller.apply(evidenceEvent(controller, evidence({
    generationSeed: 14,
    directories: 1,
    roots: [directoryRoot(950, 'empty')],
    settledTargets: controller.state.projection.unsettledTargets,
  })))
  controller.apply(evidenceEvent(controller, evidence({
    generationSeed: 15,
  })))
  return controller.apply({
    kind: 'discovery-completed',
    epoch: controller.state.projection.epoch,
    layoutBasis: {
      kind: 'complete-directory',
      anchor: directoryAnchor(950, 'empty'),
    },
  })
}

async function selection(input: {
  readonly defaultSelected?: boolean
  readonly rules?: readonly NodeSelectionRule[]
} = {}) {
  return createSelectionSpec({
    shareInstance: SHARE_INSTANCE,
    syntheticRoot: SYNTHETIC_ROOT,
    rules: {
      mode: 'node-id',
      defaultSelected: input.defaultSelected ?? false,
      rules: input.rules ?? [],
    },
  })
}

function startedController(
  spec: Awaited<ReturnType<typeof selection>>,
): SelectionProjectionController {
  const controller = new SelectionProjectionController()
  const state = controller.beginSelection(spec)
  controller.apply({ kind: 'discovery-started', epoch: state.projection.epoch })
  return controller
}

function started(spec: Awaited<ReturnType<typeof selection>>): SelectionProjectionState {
  return startedController(spec).state
}

function evidence(input: EvidenceInput): AuthenticatedProjectionEvidence {
  return createAuthenticatedProjectionEvidence({
    generations: [{
      directoryId: identity(50_000 + input.generationSeed),
      generation: identity(55_000 + input.generationSeed),
    }],
    metrics: {
      fileCountLowerBound: input.files ?? 0,
      directoryCountLowerBound: input.directories ?? 0,
      byteCountLowerBound: input.bytes ?? 0n,
    },
    ...(input.representativeFile === undefined ? {} : { representativeFile: input.representativeFile }),
    selectedRoots: input.roots ?? [],
    ...(input.selectedRootCount === undefined ? {} : { selectedRootCount: input.selectedRootCount }),
    settledTargets: input.settledTargets ?? [],
    ...(input.syntheticLayout === true
      ? { earlyLayoutBasis: { kind: 'synthetic-selection' as const } }
      : {}),
  })
}

function evidenceEvent(
  controller: SelectionProjectionController,
  authenticated: AuthenticatedProjectionEvidence,
) {
  return Object.freeze({
    kind: 'authenticated-evidence' as const,
    epoch: controller.state.projection.epoch,
    evidence: authenticated,
  })
}

function fileRoot(seed: number, portableName: string): Extract<SelectedRootFact, { kind: 'file' }> {
  return Object.freeze({
    kind: 'file',
    fileId: identity(seed),
    sourcePath: portableName,
    portableName,
  })
}

function directoryRoot(
  seed: number,
  portableName: string,
): Extract<SelectedRootFact, { kind: 'directory' }> {
  return Object.freeze({
    kind: 'directory',
    directoryId: identity(seed),
    sourcePath: portableName,
    portableName,
  })
}

function directoryAnchor(seed: number, sourcePath: string): ProjectedDirectoryAnchor {
  return Object.freeze({ directoryId: identity(seed), sourcePath })
}

function identity(seed: number): string {
  const bytes = new Uint8Array(16)
  new DataView(bytes.buffer).setUint32(12, seed, false)
  return encodeBase64Url(bytes)
}
