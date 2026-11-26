import { describe, expect, it, vi } from 'vitest'
import type { BrowseDirectory, BrowsePage, JoinedBrowserShare } from '../../src/application/browser-share/gateway'
import { findReplacementFile, ReplacementFileNotFoundError } from '../../src/application/browser-share/source-replacement'
import {
  admitReplacementDownload,
  hasRetainedReplacementOutput,
  type RetainedAdmissionContext,
} from '../../src/application/receive/admission'
import type { ReceiveAction, ReceiveAdmission, RetainedTaskSnapshot } from '../../src/application/receive/contracts'
import type { ReceiveCoordinator } from '../../src/application/receive/coordinator'
import { ReceiveBlockedError } from '../../src/application/receive/single-file'
import type { CatalogEntry } from '../../src/catalog/records'
import { SelectionPolicy } from '../../src/catalog/selection'
import { initialReceiveState, transitionReceiveState, type SourceRevisionFailure } from '../../src/transfer/state'
import { mapReceiverFailure } from '../../src/ui/controller/failures'
import { initialReceiverSnapshot } from '../../src/ui/controller-state'
import type { ReceiverFailure, ReceiverSnapshot } from '../../src/ui/model'
import { ReceiveOperationTransitions } from '../../src/ui/operation-ownership/transitions'

const SHARE_A = 'share-a'
const SHARE_B = 'share-b'

const STALE_FAILURE: SourceRevisionFailure = Object.freeze({
  entryId: 'entry-stale',
  path: Object.freeze(['docs', 'stale.txt']),
  sourcePath: Object.freeze(['docs', 'stale.txt']),
  reason: 'source-changed',
})

const BUSY_FAILURE: SourceRevisionFailure = Object.freeze({
  entryId: 'entry-busy',
  path: Object.freeze(['docs', 'locked.txt']),
  sourcePath: Object.freeze(['docs', 'locked.txt']),
  reason: 'source-busy',
})

function makeTask(
  kind: 'partial' | 'paused-retained' | 'paused-empty' | 'failed',
  options: Readonly<{ activeElsewhere?: boolean; includeFailures?: boolean }> = {},
): RetainedTaskSnapshot {
  const base = initialReceiveState('task-1')
  let lifecycle = transitionReceiveState(base, {
    kind: 'partial',
    reason: 'failures',
    savedFiles: 1n,
    failedFiles: 1n,
  })
  if (kind === 'paused-retained') {
    lifecycle = transitionReceiveState(base, {
      kind: 'paused',
      reason: 'user',
      completedFiles: 1n,
      completedBytes: 128n,
      retainedBytes: 128n,
    })
  } else if (kind === 'paused-empty') {
    lifecycle = transitionReceiveState(base, {
      kind: 'paused',
      reason: 'user',
      completedFiles: 0n,
      completedBytes: 0n,
      retainedBytes: 0n,
    })
  } else if (kind === 'failed') {
    lifecycle = transitionReceiveState(base, {
      kind: 'failed',
      reason: 'source-changed',
    })
  }
  const actions: readonly ReceiveAction[] = kind === 'paused-retained' || kind === 'paused-empty'
    ? ['continue', 'discard']
    : ['save-partial', 'discard']
  return Object.freeze({
    taskId: 'task-1',
    shareInstance: SHARE_A,
    selectionKey: 'sel-a',
    backend: 'opfs',
    lifecycle,
    actions,
    activeElsewhere: options.activeElsewhere ?? false,
    canSavePartial: kind === 'partial' || kind === 'paused-retained',
    ...(options.includeFailures === false ? {} : {
      sourceRevisionFailures: Object.freeze({
        shareInstance: SHARE_A,
        count: 2n,
        files: Object.freeze([STALE_FAILURE, BUSY_FAILURE]),
      }),
    }),
  })
}

function makeShareFixture(options: Readonly<{
  shareInstanceId?: string
  pages: (directory: BrowseDirectory, pageIndex: number, signal?: AbortSignal) => Promise<BrowsePage>
}>) {
  const root: BrowseDirectory = Object.freeze({
    id: new Uint8Array(16).fill(1),
    idText: 'dir-root',
    path: Object.freeze([]),
    ancestry: Object.freeze([]),
  })
  const docsDir: BrowseDirectory = Object.freeze({
    id: new Uint8Array(16).fill(2),
    idText: 'dir-docs',
    path: Object.freeze(['docs']),
    ancestry: Object.freeze(['dir-docs']),
  })
  const selection = new SelectionPolicy(false)
  const selectOnlyFile = vi.fn((entry: CatalogEntry, ancestry: readonly string[]) => {
    selection.set(entry, ancestry, true)
  })
  const page = vi.fn((directory: BrowseDirectory, pageIndex: number, opts?: { signal?: AbortSignal }) =>
    options.pages(directory, pageIndex, opts?.signal),
  )
  const joined = {
    descriptor: { shareInstanceId: options.shareInstanceId ?? SHARE_A, syntheticRootId: root.idText },
    selection,
    rootDirectory: () => root,
    childDirectory: (_parent: BrowseDirectory, entry: CatalogEntry) =>
      entry.name === 'docs' ? docsDir : Object.freeze({ ...docsDir, idText: entry.idText, path: Object.freeze([entry.name]) }),
    page,
    selectOnlyFile,
  } as unknown as JoinedBrowserShare
  return { joined, root, docsDir, page, selectOnlyFile }
}

function makeTransitionsHarness(input: Readonly<{
  joined: () => JoinedBrowserShare | undefined
  admission: () => ReceiveAdmission
  shareOpen?: boolean
  loadPage?: () => Promise<void>
}>) {
  const baseSnapshot = initialReceiverSnapshot()
  let snapshot: ReceiverSnapshot = Object.freeze({
    ...baseSnapshot,
    phase: input.shareOpen === false ? 'awaiting-key' : 'browsing',
    status: input.shareOpen === false ? 'enter-key' : null,
    share: input.shareOpen === false
      ? null
      : Object.freeze({
          kind: 'browser' as const,
          shareInstance: SHARE_A,
          name: 'Docs',
          homeDirectoryId: 'dir-root',
          singleFolder: false,
        }),
  })
  const startNewDownload = vi.fn()
  const beginProjection = vi.fn()
  const onActionError = vi.fn()
  const loadPage = vi.fn(input.loadPage ?? (async () => {
    snapshot = Object.freeze({ ...snapshot, browse: Object.freeze({ kind: 'ready' as const, error: null }) })
  }))
  const coordinator = {
    replacementAdmission: vi.fn(() => input.admission()),
    activeActionAdmission: vi.fn(() => ({ allowed: true, reason: null })),
    retainedActionAdmission: vi.fn(() => ({ allowed: true, reason: null })),
    performActiveAction: vi.fn(),
    performRetainedAction: vi.fn(),
    startNewDownload,
    canRetainCurrent: false,
  } as unknown as ReceiveCoordinator
  const transitions = new ReceiveOperationTransitions({
    coordinator,
    currentJoinedShare: input.joined,
    isDisposed: () => false,
    snapshot: () => snapshot,
    publish: next => { snapshot = Object.freeze(next) },
    mapFailure: error => mapReceiverFailure(error, err => (err instanceof Error ? err.message : String(err))),
    onActionError,
    recordIntent: vi.fn(),
    beginProjection,
    browse: { loadPage },
    previews: { yieldToReceiving: vi.fn() },
    experienceTrace: { intent: vi.fn() },
  })
  return {
    transitions,
    coordinator,
    startNewDownload,
    beginProjection,
    onActionError,
    loadPage,
    getSnapshot: () => snapshot,
    setSnapshot: (next: Partial<ReceiverSnapshot>) => { snapshot = Object.freeze({ ...snapshot, ...next }) },
  }
}

function deferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (reason?: unknown) => void
  const promise = new Promise<T>((res, rej) => { resolve = res; reject = rej })
  return { promise, resolve, reject }
}

describe('source replacement admission and transitions', () => {
  it('applies structural-before-transient admission precedence for replacement downloads', () => {
    const partial = makeTask('partial')
    const pausedRetained = makeTask('paused-retained')
    const pausedEmpty = makeTask('paused-empty')
    const failed = makeTask('failed')

    expect(hasRetainedReplacementOutput(partial)).toBe(true)
    expect(hasRetainedReplacementOutput(pausedRetained)).toBe(true)
    expect(hasRetainedReplacementOutput(pausedEmpty)).toBe(false)
    expect(hasRetainedReplacementOutput(failed)).toBe(false)

    const idleMatching: RetainedAdmissionContext = {
      currentShareInstance: SHARE_A,
      hasActiveDownload: false,
      hasPendingAction: false,
      connectionBlock: null,
    }
    expect(admitReplacementDownload(partial, idleMatching)).toEqual({ allowed: true, reason: null })
    expect(admitReplacementDownload(pausedRetained, idleMatching)).toEqual({ allowed: true, reason: null })
    // Replacement creates a new download task, so an active lock in another tab on the old task does not block it.
    const lockedElsewhere = makeTask('paused-retained', { activeElsewhere: true })
    expect(admitReplacementDownload(lockedElsewhere, idleMatching)).toEqual({
      allowed: true,
      reason: null,
    })
    expect(admitReplacementDownload(pausedEmpty, idleMatching)).toEqual({ allowed: false, reason: 'actionUnavailable' })
    expect(admitReplacementDownload(failed, idleMatching)).toEqual({ allowed: false, reason: 'actionUnavailable' })
    expect(admitReplacementDownload(makeTask('partial', { includeFailures: false }), idleMatching)).toEqual({
      allowed: false,
      reason: 'actionUnavailable',
    })

    // Share mismatch beats transient busy/downloading blockers.
    expect(admitReplacementDownload(partial, {
      currentShareInstance: null,
      hasActiveDownload: true,
      hasPendingAction: true,
      connectionBlock: null,
    })).toEqual({ allowed: false, reason: 'matchingShare' })
    expect(admitReplacementDownload(partial, {
      currentShareInstance: SHARE_B,
      hasActiveDownload: true,
      hasPendingAction: true,
      connectionBlock: null,
    })).toEqual({ allowed: false, reason: 'matchingShare' })

    // Terminal connection failure beats transient busy/downloading blockers.
    expect(admitReplacementDownload(partial, {
      currentShareInstance: SHARE_A,
      hasActiveDownload: true,
      hasPendingAction: true,
      connectionBlock: 'shareEnded',
    })).toEqual({ allowed: false, reason: 'shareEnded' })

    expect(admitReplacementDownload(partial, {
      ...idleMatching,
      hasPendingAction: true,
      hasActiveDownload: true,
    })).toEqual({ allowed: false, reason: 'busy' })

    expect(admitReplacementDownload(partial, {
      ...idleMatching,
      hasActiveDownload: true,
    })).toEqual({ allowed: false, reason: 'downloading' })
  })

  it('maps replacement not-found and share mismatch errors to structured ReceiverFailures', async () => {
    const task = makeTask('partial')
    const wrongShare = makeShareFixture({
      shareInstanceId: SHARE_B,
      pages: async () => { throw new Error('should not query pages') },
    })
    await expect(
      findReplacementFile(wrongShare.joined, task, STALE_FAILURE, new AbortController().signal),
    ).rejects.toBeInstanceOf(ReceiveBlockedError)

    const emptyShare = makeShareFixture({
      pages: async (directory) => ({
        directory,
        pageIndex: 0,
        pageCount: 1,
        entries: [],
        entryCount: 0,
        omittedCount: 0n,
      }),
    })
    let thrown: unknown
    try {
      await findReplacementFile(emptyShare.joined, task, STALE_FAILURE, new AbortController().signal)
    } catch (error) {
      thrown = error
    }
    expect(thrown).toBeInstanceOf(ReplacementFileNotFoundError)
    expect(mapReceiverFailure(thrown, String)).toEqual({
      kind: 'replacement-not-found',
      path: 'docs/stale.txt',
    })
  })

  it('gates replacement lookups on admission without running page queries or polluting global error', () => {
    const task = makeTask('partial')
    const fixture = makeShareFixture({
      pages: async () => { throw new Error('should not run') },
    })
    const harness = makeTransitionsHarness({
      joined: () => fixture.joined,
      admission: () => ({ allowed: false, reason: 'matchingShare' }),
    })
    const onSettled = vi.fn()
    const onAsyncFailure = vi.fn(() => true)

    harness.transitions.prepareReplacementDownload(task, STALE_FAILURE, { onAsyncFailure, onSettled })

    expect(fixture.page).not.toHaveBeenCalled()
    expect(onSettled).toHaveBeenCalledTimes(1)
    expect(onAsyncFailure).not.toHaveBeenCalled()
    expect(harness.onActionError).not.toHaveBeenCalled()
    expect(harness.getSnapshot().status).toBeNull()
  })

  it('selects the replacement file, updates status, and invokes onPrepared on success', async () => {
    const task = makeTask('partial')
    const docsEntry: CatalogEntry = {
      kind: 'directory',
      id: new Uint8Array(16).fill(2),
      idText: 'dir-docs',
      name: 'docs',
    }
    const staleEntry: CatalogEntry = {
      kind: 'file',
      id: new Uint8Array(16).fill(3),
      idText: 'file-stale',
      name: 'stale.txt',
      expectedSize: 64n,
    }
    const fixture = makeShareFixture({
      pages: async (directory) => ({
        directory,
        pageIndex: 0,
        pageCount: 1,
        entries: directory.idText === 'dir-root' ? [docsEntry] : [staleEntry],
        entryCount: 1,
        omittedCount: 0n,
      }),
    })
    const harness = makeTransitionsHarness({
      joined: () => fixture.joined,
      admission: () => ({ allowed: true, reason: null }),
    })
    const onPrepared = vi.fn()
    const onSettled = vi.fn()

    harness.transitions.prepareReplacementDownload(task, STALE_FAILURE, { onPrepared, onSettled })
    await vi.waitFor(() => expect(onSettled).toHaveBeenCalledTimes(1))

    expect(harness.startNewDownload).toHaveBeenCalledTimes(1)
    expect(fixture.selectOnlyFile).toHaveBeenCalledWith(staleEntry, fixture.docsDir.ancestry)
    expect(harness.loadPage).toHaveBeenCalledWith(fixture.docsDir, 0, [fixture.root, fixture.docsDir])
    expect(harness.beginProjection).toHaveBeenCalledWith(fixture.joined)
    expect(harness.getSnapshot().status).toBe('replacement-selected')
    expect(onPrepared).toHaveBeenCalledTimes(1)
    expect(harness.onActionError).not.toHaveBeenCalled()
  })

  it('routes async failures locally when visible and falls back to share error when unmounted', async () => {
    const task = makeTask('partial')
    const fixture = makeShareFixture({
      pages: async (directory) => ({
        directory,
        pageIndex: 0,
        pageCount: 1,
        entries: [],
        entryCount: 0,
        omittedCount: 0n,
      }),
    })
    const harness = makeTransitionsHarness({
      joined: () => fixture.joined,
      admission: () => ({ allowed: true, reason: null }),
    })

    const localFailures: ReceiverFailure[] = []
    const settledLocal = vi.fn()
    harness.transitions.prepareReplacementDownload(task, STALE_FAILURE, {
      onAsyncFailure: failure => { localFailures.push(failure); return true },
      onSettled: settledLocal,
    })
    await vi.waitFor(() => expect(settledLocal).toHaveBeenCalledTimes(1))

    expect(localFailures).toEqual([{ kind: 'replacement-not-found', path: 'docs/stale.txt' }])
    expect(harness.onActionError).not.toHaveBeenCalled()
    expect(harness.getSnapshot().status).toBeNull()

    // Unmounted modal returns false -> falls back to global share error banner.
    const settledClosed = vi.fn()
    harness.transitions.prepareReplacementDownload(task, STALE_FAILURE, {
      onAsyncFailure: () => false,
      onSettled: settledClosed,
    })
    await vi.waitFor(() => expect(settledClosed).toHaveBeenCalledTimes(1))

    expect(harness.onActionError).toHaveBeenCalledTimes(1)
    expect(harness.onActionError.mock.calls[0]?.[0]).toBeInstanceOf(ReplacementFileNotFoundError)
    expect(harness.getSnapshot().status).toBeNull()
  })

  it('aborts silently on cancellation and re-checks admission after async lookup', async () => {
    const task = makeTask('partial')
    const docsEntry: CatalogEntry = {
      kind: 'directory',
      id: new Uint8Array(16).fill(2),
      idText: 'dir-docs',
      name: 'docs',
    }
    const staleEntry: CatalogEntry = {
      kind: 'file',
      id: new Uint8Array(16).fill(3),
      idText: 'file-stale',
      name: 'stale.txt',
      expectedSize: 64n,
    }
    const firstLookup = deferred<BrowsePage>()
    let currentAdmission: ReceiveAdmission = { allowed: true, reason: null }
    let callCount = 0
    const fixture = makeShareFixture({
      pages: async (directory, _pageIndex, signal) => {
        callCount += 1
        if (callCount === 1) {
          return new Promise<BrowsePage>((resolve, reject) => {
            signal?.addEventListener('abort', () => reject(signal.reason), { once: true })
            void firstLookup.promise.then(resolve, reject)
          })
        }
        return {
          directory,
          pageIndex: 0,
          pageCount: 1,
          entries: directory.idText === 'dir-root' ? [docsEntry] : [staleEntry],
          entryCount: 1,
          omittedCount: 0n,
        }
      },
    })
    const harness = makeTransitionsHarness({
      joined: () => fixture.joined,
      admission: () => currentAdmission,
    })

    // Cancelling while in flight must stay silent and clear finding-replacement status.
    const abortedFailure = vi.fn(() => true)
    const abortedSettled = vi.fn()
    harness.transitions.prepareReplacementDownload(task, STALE_FAILURE, {
      onAsyncFailure: abortedFailure,
      onSettled: abortedSettled,
    })
    expect(harness.getSnapshot().status).toEqual({ kind: 'finding-replacement', path: 'docs/stale.txt' })
    harness.transitions.cancelPendingReplacement()
    await vi.waitFor(() => expect(abortedSettled).toHaveBeenCalledTimes(1))

    expect(abortedFailure).not.toHaveBeenCalled()
    expect(harness.onActionError).not.toHaveBeenCalled()
    expect(harness.getSnapshot().status).toBeNull()

    // Admission flipping to blocked mid-lookup must not clobber workspace selection.
    const midLookupPage = deferred<BrowsePage>()
    fixture.page.mockImplementationOnce(async () => midLookupPage.promise)
    const postBlockFailures: ReceiverFailure[] = []
    const postBlockSettled = vi.fn()
    harness.transitions.prepareReplacementDownload(task, STALE_FAILURE, {
      onAsyncFailure: failure => { postBlockFailures.push(failure); return true },
      onSettled: postBlockSettled,
    })
    currentAdmission = { allowed: false, reason: 'shareEnded' }
    midLookupPage.resolve({
      directory: fixture.root,
      pageIndex: 0,
      pageCount: 1,
      entries: [docsEntry],
      entryCount: 1,
      omittedCount: 0n,
    })
    await vi.waitFor(() => expect(postBlockSettled).toHaveBeenCalledTimes(1))

    expect(harness.startNewDownload).not.toHaveBeenCalled()
    expect(fixture.selectOnlyFile).not.toHaveBeenCalled()
    expect(postBlockFailures).toEqual([{ kind: 'blocked', reason: 'shareEnded' }])
    expect(harness.onActionError).not.toHaveBeenCalled()
    expect(harness.getSnapshot().status).toBeNull()
  })
})
