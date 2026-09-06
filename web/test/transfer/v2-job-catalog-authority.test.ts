import { describe, expect, it, vi } from 'vitest'

import { V2SelectionPolicy } from '../../src/catalog/v2-selection'
import { V2DirectoryAncestry, V2DirectoryTraversalError } from '../../src/transfer/v2-job'
import {
  catalogFixture,
  directoryEntry,
  fileEntry,
  identity,
  identityText,
  testOutput,
  planAuthorityFixture,
  readerFixture,
  receiveIntentFixture,
  transferJobFixture,
} from './v2-job-fixture'

describe('v2 catalog and preparation authority', () => {
  it('releases sequential sibling identities from path-local ancestry', () => {
    const ancestry = new V2DirectoryAncestry()
    const leaveRoot = ancestry.enter('root')
    for (let index = 0; index < 10_000; index += 1) {
      const leaveSibling = ancestry.enter(`sibling-${index}`)
      leaveSibling()
    }

    expect(ancestry.depth).toBe(1)
    expect(ancestry.maximumDepth).toBe(2)
    leaveRoot()
    expect(ancestry.depth).toBe(0)
  })

  it.each(['workspace-then-publish', 'portable-handoff'] as const)(
    'keeps revision and block authority unreachable when %s admission is rejected',
    async (planKind) => {
      const root = identity(2)
      const file = fileEntry(identity(11), 'payload.bin', 4n)
      const selection = new V2SelectionPolicy(true)
      const catalog = catalogFixture([{ id: root, entries: [file], generation: identity(31) }])
      const readers = readerFixture([file])
      const plans = planAuthorityFixture({ rejectPreparation: true })
      const intent = await receiveIntentFixture({
        planKind,
        artifactKind: 'zip-archive',
        selection,
      })

      const result = await transferJobFixture({
        catalog: catalog.catalog,
        selection,
        intent,
        plans,
        revisions: readers.revisions,
        broker: readers.broker,
      }).run()

      expect(result.worker.status).toBe('Paused')
      expect(result.lifecycle.kind).toBe('discarded')
      expect(plans.routes).toEqual([planKind])
      expect(plans.preparations).toHaveLength(planKind === 'portable-handoff' ? 1 : 0)
      expect(catalog.loads).toEqual(planKind === 'portable-handoff' ? [identityText(2)] : [])
      expect(plans.settlements).toEqual([])
      expect(plans.output.requests).toEqual([])
      expect(readers.revisionRequests).toEqual([])
      expect(readers.blockRequests).toEqual([])
    },
  )

  it('passes exact authenticated generations and artifact entries to Portable ZIP preparation', async () => {
    const root = identity(2)
    const file = fileEntry(identity(11), 'payload.bin', 4n)
    const selection = new V2SelectionPolicy(true)
    const catalog = catalogFixture([{ id: root, entries: [file], generation: identity(31) }])
    const readers = readerFixture([file])
    const plans = planAuthorityFixture({ rejectPreparation: true })
    const intent = await receiveIntentFixture({
      planKind: 'portable-handoff',
      artifactKind: 'zip-archive',
      selection,
    })

    await transferJobFixture({
      catalog: catalog.catalog,
      selection,
      intent,
      plans,
      revisions: readers.revisions,
      broker: readers.broker,
    }).run()

    const evidence = plans.preparations[0]
    expect(evidence).toBeDefined()
    expect(evidence?.generations).toEqual([{
      directoryId: identityText(2),
      generation: identityText(31),
    }])
    expect(evidence?.entries.map(entry => entry.kind)).toEqual(['directory', 'file'])
    expect(evidence).toMatchObject({
      entryCount: 2n,
      fileCount: 1n,
      directoryCount: 1n,
      selectedRawBytes: 4n,
    })
    expect(evidence?.entries[0]).toMatchObject({
      kind: 'directory',
      sourcePath: [],
      role: 'result-root',
    })
    expect(evidence?.entries[1]).toMatchObject({
      kind: 'file',
      sourcePath: ['payload.bin'],
      exactSize: 4n,
    })
    expect(readers.revisionRequests).toEqual([])
    expect(readers.blockRequests).toEqual([])
  })

  it.each(['portable-handoff'] as const)(
    'does not prepare or open output when %s discovery is incomplete',
    async (planKind) => {
      const root = identity(2)
      const child = identity(3)
      const selection = new V2SelectionPolicy(true)
      const discoveryFailure = new V2DirectoryTraversalError('child generation unavailable')
      const catalog = catalogFixture([
        { id: root, entries: [directoryEntry(child, 'child')] },
        { id: child, entries: [], loadFailure: discoveryFailure },
      ])
      const readers = readerFixture([])
      const plans = planAuthorityFixture()
      const intent = await receiveIntentFixture({
        planKind,
        artifactKind: 'zip-archive',
        selection,
      })

      const result = await transferJobFixture({
        catalog: catalog.catalog,
        selection,
        intent,
        plans,
        revisions: readers.revisions,
        broker: readers.broker,
      }).run()

      expect(result.worker.status).toBe('CompletedWithErrors')
      expect(result.lifecycle.kind).toBe('discarded')
      expect(result.measure.discovery).toBe('failed')
      expect(plans.routes).toEqual([])
      expect(plans.preparations).toEqual([])
      expect(plans.settlements).toEqual([])
      expect(plans.admissionFailures).toEqual([expect.anything()])
      expect(plans.output.requests).toEqual([])
      expect(readers.revisionRequests).toEqual([])
      expect(readers.blockRequests).toEqual([])
    },
  )

  it.each(['failure', 'cancellation'] as const)(
    'retains authenticated Workspace ZIP progress after discovery %s without publishing',
    async (interruption) => {
      const root = identity(2)
      const child = identity(3)
      const file = fileEntry(identity(11), 'a.bin', 4n)
      const selection = new V2SelectionPolicy(true)
      const childReplayStarted = deferred()
      const releaseChildReplay = deferred()
      const fileCompleted = deferred()
      const catalog = catalogFixture([
        { id: root, entries: [file, directoryEntry(child, 'z-child')], generation: identity(31) },
        {
          id: child,
          generation: identity(32),
          entries: [],
          beforePages: async () => {
            childReplayStarted.resolve()
            await releaseChildReplay.promise
            if (interruption === 'failure') {
              throw new V2DirectoryTraversalError('child generation unavailable')
            }
          },
        },
      ])
      const readers = readerFixture([file])
      const output = testOutput([], { durability: 'ProcessRestart' })
      const plans = planAuthorityFixture({ output })
      const discoveryComplete = vi.fn(async () => {})
      const discoveryGeneration = vi.fn(async () => {})
      const openWorkspaceZip = plans.openWorkspaceZip
      plans.openWorkspaceZip = async (...args) => {
        const admitted = await openWorkspaceZip(...args)
        if (admitted.kind === 'rejected') return admitted
        return {
          ...admitted,
          execution: { ...admitted.execution, discoveryComplete, discoveryGeneration },
        }
      }
      const intent = await receiveIntentFixture({
        planKind: 'workspace-then-publish',
        artifactKind: 'zip-archive',
        selection,
      })
      const controller = new AbortController()
      const running = transferJobFixture({
        catalog: catalog.catalog,
        selection,
        intent,
        plans,
        revisions: readers.revisions,
        broker: readers.broker,
        onProgress: progress => {
          if (progress.completedFiles === 1) fileCompleted.resolve()
        },
      }).run(controller.signal)

      await Promise.all([childReplayStarted.promise, fileCompleted.promise])
      expect(output.commits).toEqual([file.idText])
      expect(plans.preparations).toEqual([])
      expect(plans.settlements).toEqual([])
      expect(discoveryComplete).not.toHaveBeenCalled()
      expect(output.requests).toHaveLength(1)
      expect(output.requests[0]).toMatchObject({
        source: { shareInstance: identityText(1), fileId: file.idText },
        sourceAuthenticationPath: ['a.bin'],
        expectedSize: 4n,
        parentAdmission: { directoryId: identityText(2), generation: identityText(31) },
      })
      expect(discoveryGeneration).toHaveBeenCalledWith(
        identityText(2), identityText(31), [], expect.any(AbortSignal),
      )
      if (interruption === 'cancellation') {
        controller.abort(new DOMException('cancel progressive discovery', 'AbortError'))
      }
      releaseChildReplay.resolve()
      const result = await running

      expect(result.worker.status).toBe(interruption === 'failure' ? 'CompletedWithErrors' : 'Paused')
      expect(result.lifecycle.kind).toBe('resumable-receive')
      expect(result.measure.discovery).toBe('failed')
      expect(plans.routes).toEqual(['workspace-then-publish'])
      expect(plans.pauses).toEqual(['workspace-then-publish'])
      expect(plans.pauseRequests[0]?.selectionFacts).toEqual({
        discoveredFileCount: 1n,
        discoveredBytes: 4n,
        discovery: 'failed',
      })
      expect(plans.admissionFailures).toEqual([])
      expect(plans.settlements).toEqual([])
      expect(discoveryComplete).not.toHaveBeenCalled()
      expect(readers.revisionRequests).toEqual([file.idText])
      expect(readers.blockRequests).toEqual([file.idText])
      expect(readers.releases).toEqual([file.idText])
      expect(output.retirements).toEqual([])
      expect(output.finalProofs).toHaveLength(1)
    },
  )

  it('rejects an omitted synthetic root before any directory, revision, or block mutation', async () => {
    const root = identity(2)
    const selection = new V2SelectionPolicy(true)
    const catalog = catalogFixture([{ id: root, entries: [], omittedCount: 1n }])
    const readers = readerFixture([])
    const plans = planAuthorityFixture()
    const intent = await receiveIntentFixture({
      planKind: 'direct-tree',
      artifactKind: 'directory-tree',
      selection,
    })

    const result = await transferJobFixture({
      catalog: catalog.catalog,
      selection,
      intent,
      plans,
      revisions: readers.revisions,
      broker: readers.broker,
    }).run()

    expect(result.worker.status).toBe('Paused')
    expect(result.lifecycle.kind).toBe('partial-directory')
    expect(plans.routes).toEqual(['direct-tree'])
    expect(plans.settlements).toEqual([])
    expect(plans.pauses).toEqual(['direct-tree'])
    expect(plans.output.requests).toEqual([])
    expect(readers.revisionRequests).toEqual([])
    expect(readers.blockRequests).toEqual([])
  })
})

function deferred(): Readonly<{ promise: Promise<void>; resolve(): void }> {
  let resolve!: () => void
  const promise = new Promise<void>(complete => { resolve = complete })
  return Object.freeze({ promise, resolve })
}
