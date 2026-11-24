import { describe, expect, it } from 'vitest'
import {
  actionsForState,
  admitRetainedAction,
  type RetainedAdmissionContext,
} from '../../src/application/receive/admission'
import type {
  PendingRetainedAction,
  RetainedInventorySnapshot,
  RetainedTaskSnapshot,
} from '../../src/application/receive/contracts'
import { resolveWorkspaceRetainedMatch } from '../../src/application/receive/retained'
import type { ArtifactOffers, SaveBackendKind } from '../../src/output/model'
import {
  initialReceiveState,
  transitionReceiveState,
  type ReceiveContentWarning,
  type ReceiveLifecycleState,
} from '../../src/transfer/state'
import { composeTasks } from '../../src/ui/experience/task-composition'
import {
  dismissedTaskKey,
  resolveWorkspaceTaskLayout,
  type WorkspaceTaskSnapshot,
} from '../../src/ui/experience/retained-match'
import { EMPTY_PROGRESS, type ReceiverSnapshot, type ShareIdentity } from '../../src/ui/model'
import { EMPTY_OUTPUT_PRESENTATION, type OutputPresentationSnapshot } from '../../src/ui/output'

const SHARE_A = 'share-a'
const SHARE_B = 'share-b'
const SEL_A = 'sel-a'
const SEL_B = 'sel-b'
const SEL_C = 'sel-c'

const PHOTO_SHARE: ShareIdentity = Object.freeze({
  kind: 'photo',
  shareInstance: SHARE_A,
  name: 'photo.jpg',
  file: { id: 'f1', kind: 'file' as const, name: 'photo.jpg', expectedSize: 1024n, selection: 'selected' as const },
})

const FOLDER_SHARE: ShareIdentity = Object.freeze({
  kind: 'browser',
  shareInstance: SHARE_A,
  name: 'Photos',
  homeDirectoryId: 'root',
  singleFolder: true,
})

const PARTIAL_WARNING: ReceiveContentWarning = Object.freeze({
  selectedFileCount: 2n,
  completedFileCount: 1n,
  missingFiles: Object.freeze([{ path: ['b.txt'], reason: 'source-changed' as const }]),
})

function offersFor(selectionKey: string): ArtifactOffers {
  return Object.freeze({ kind: 'confirming', epoch: 1n, selectionKey })
}

function makeRetainedTask(input: Readonly<{
  taskId: string
  lifecycle: ReceiveLifecycleState
  backend?: SaveBackendKind
  shareInstance?: string
  selectionKey?: string
  createdAtMilliseconds?: number
  activeElsewhere?: boolean
  canSavePartial?: boolean
}>): RetainedTaskSnapshot {
  const backend = input.backend ?? 'opfs'
  const canSavePartial = input.canSavePartial ?? false
  return Object.freeze({
    taskId: input.taskId,
    shareInstance: input.shareInstance ?? SHARE_A,
    selectionKey: input.selectionKey ?? SEL_A,
    backend,
    lifecycle: input.lifecycle,
    display: Object.freeze({
      objectLabel: 'photo.jpg',
      createdAtMilliseconds: input.createdAtMilliseconds ?? 1_000,
    }),
    actions: actionsForState(input.lifecycle, backend, canSavePartial),
    activeElsewhere: input.activeElsewhere ?? false,
    canSavePartial,
  })
}

function completeLocalTask(
  taskId: string,
  options: Readonly<{
    kind?: 'download-started' | 'ready-to-save'
    createdAt?: number
    selectionKey?: string
    shareInstance?: string
    activeElsewhere?: boolean
    partial?: boolean
  }> = {},
): RetainedTaskSnapshot {
  const base = initialReceiveState(taskId, options.createdAt ?? 1_000)
  const payload = options.kind === 'ready-to-save'
    ? { kind: 'ready-to-save' as const, size: 1024n, fileName: 'photo.jpg' }
    : { kind: 'download-started' as const, retained: true, fileName: 'photo.jpg' }
  const lifecycle = transitionReceiveState(base, {
    ...payload,
    ...(options.partial ? { contentWarning: PARTIAL_WARNING } : {}),
  })
  return makeRetainedTask({
    taskId,
    lifecycle,
    ...(options.createdAt === undefined ? {} : { createdAtMilliseconds: options.createdAt }),
    ...(options.selectionKey === undefined ? {} : { selectionKey: options.selectionKey }),
    ...(options.shareInstance === undefined ? {} : { shareInstance: options.shareInstance }),
    ...(options.activeElsewhere === undefined ? {} : { activeElsewhere: options.activeElsewhere }),
  })
}

function pausedTask(
  taskId: string,
  options: Readonly<{
    createdAt?: number
    selectionKey?: string
    shareInstance?: string
    activeElsewhere?: boolean
    canSavePartial?: boolean
  }> = {},
): RetainedTaskSnapshot {
  const base = initialReceiveState(taskId, options.createdAt ?? 2_000)
  const lifecycle = transitionReceiveState(base, {
    kind: 'paused',
    reason: 'user',
    completedFiles: options.canSavePartial ? 1n : 0n,
    completedBytes: options.canSavePartial ? 512n : 0n,
    retainedBytes: 512n,
    discoveredFiles: 2n,
    discoveredBytes: 1024n,
    discoveryComplete: true,
  })
  return makeRetainedTask({
    taskId,
    lifecycle,
    ...(options.createdAt === undefined ? {} : { createdAtMilliseconds: options.createdAt }),
    ...(options.selectionKey === undefined ? {} : { selectionKey: options.selectionKey }),
    ...(options.shareInstance === undefined ? {} : { shareInstance: options.shareInstance }),
    ...(options.activeElsewhere === undefined ? {} : { activeElsewhere: options.activeElsewhere }),
    ...(options.canSavePartial === undefined ? {} : { canSavePartial: options.canSavePartial }),
  })
}

function workspaceSnapshot(overrides: Readonly<{
  share?: ShareIdentity | null
  selectionKey?: string | null
  connection?: ReceiverSnapshot['connection']
  tasks?: readonly RetainedTaskSnapshot[]
  pending?: PendingRetainedAction | null
  output?: Partial<OutputPresentationSnapshot>
  activeReceiveOperationId?: string | null
}> = {}): WorkspaceTaskSnapshot {
  const share = overrides.share === undefined ? PHOTO_SHARE : overrides.share
  const connection: ReceiverSnapshot['connection'] = overrides.connection ?? Object.freeze({ kind: 'connected' })
  const pending = overrides.pending ?? null
  const retained: RetainedInventorySnapshot = Object.freeze({
    kind: 'available',
    tasks: Object.freeze([...(overrides.tasks ?? [])]),
    pending,
  })
  const selectionKey = overrides.selectionKey === undefined ? SEL_A : overrides.selectionKey
  const output: OutputPresentationSnapshot = Object.freeze({
    ...EMPTY_OUTPUT_PRESENTATION,
    ...(selectionKey === null ? {} : { offers: offersFor(selectionKey) }),
    ...overrides.output,
  })
  const activeReceiveOperationId = overrides.activeReceiveOperationId ?? null
  const admissionContext: RetainedAdmissionContext = {
    currentShareInstance: share?.shareInstance ?? null,
    hasActiveDownload: activeReceiveOperationId !== null || output.activation !== null || pending?.action === 'continue',
    hasPendingAction: pending !== null,
    connectionUnavailable: connection.kind === 'unavailable' || connection.kind === 'ended',
    shareEnded: connection.kind === 'ended',
  }
  const taskInputs = {
    output,
    progress: EMPTY_PROGRESS,
    taskDisplay: null,
    connection,
    share,
    retained,
    activeReceiveOperationId,
    startAdmission: Object.freeze({ allowed: true, reason: null, canReleaseCurrent: false, localChoice: null }),
  }
  const composed = composeTasks(
    taskInputs,
    (task, action) => admitRetainedAction(task, action, admissionContext),
  )
  return Object.freeze({
    ...taskInputs,
    draft: Object.freeze({
      mode: 'scope',
      scope: 'whole-share',
      label: 'photo.jpg',
      excludedItems: 0,
      empty: false,
    }),
    hasContent: share !== null,
    ...composed,
  })
}

describe('admitRetainedAction structural vs transient blocker precedence', () => {
  it('keeps structural share/connection blockers ahead of transient busy contention for remote continue', () => {
    const paused = pausedTask('paused-1', { canSavePartial: true })
    expect(admitRetainedAction(paused, 'continue', {
      currentShareInstance: SHARE_A,
      hasActiveDownload: false,
      hasPendingAction: true,
      connectionUnavailable: true,
      shareEnded: true,
    })).toEqual({ allowed: false, reason: 'shareEnded' })

    expect(admitRetainedAction(paused, 'continue', {
      currentShareInstance: SHARE_A,
      hasActiveDownload: false,
      hasPendingAction: true,
      connectionUnavailable: true,
      shareEnded: false,
    })).toEqual({ allowed: false, reason: 'connectionUnavailable' })

    expect(admitRetainedAction(paused, 'continue', {
      currentShareInstance: SHARE_B,
      hasActiveDownload: false,
      hasPendingAction: true,
      connectionUnavailable: false,
      shareEnded: false,
    })).toEqual({ allowed: false, reason: 'matchingShare' })

    expect(admitRetainedAction(paused, 'continue', {
      currentShareInstance: SHARE_A,
      hasActiveDownload: false,
      hasPendingAction: true,
      connectionUnavailable: false,
      shareEnded: false,
    })).toEqual({ allowed: false, reason: 'busy' })
  })
})

describe('resolveWorkspaceTaskLayout', () => {
  it('promotes complete retained OPFS copies (download-started and ready-to-save) and stays stable during re-export', () => {
    for (const kind of ['download-started', 'ready-to-save'] as const) {
      const local = completeLocalTask(`opfs-${kind}`, { kind })
      const idle = resolveWorkspaceTaskLayout(workspaceSnapshot({ tasks: [local] }))
      expect(idle.primaryTask?.operationId).toBe(local.taskId)
      expect(idle.primaryTask?.primaryAction?.id).toBe('save')
      expect(idle.collapseRemoteSaving).toBe(true)
      expect(idle.repeatRemoteLabel).toBe(true)
      expect(idle.banner).toBeNull()

      const exporting = resolveWorkspaceTaskLayout(workspaceSnapshot({
        tasks: [local],
        pending: { taskId: local.taskId, action: 'save' },
      }))
      expect(exporting.primaryTask?.operationId).toBe(local.taskId)
      expect(exporting.primaryTask?.primaryAction?.disabledReason).not.toBeNull()
      expect(exporting.collapseRemoteSaving).toBe(true)
      expect(exporting.banner).toBeNull()
    }
  })

  it('ignores activeElsewhere copies, partial OPFS ZIPs, non-retained portable records, and other shares', () => {
    const locked = completeLocalTask('locked', { activeElsewhere: true })
    expect(resolveWorkspaceTaskLayout(workspaceSnapshot({ tasks: [locked] }))).toMatchObject({
      primaryTask: null,
      collapseRemoteSaving: false,
      banner: null,
    })

    const partialHandoff = completeLocalTask('partial-zip', { partial: true })
    expect(resolveWorkspaceTaskLayout(workspaceSnapshot({ tasks: [partialHandoff] }))).toMatchObject({
      primaryTask: null,
      collapseRemoteSaving: false,
      banner: null,
    })

    const partialReady = completeLocalTask('partial-ready', { kind: 'ready-to-save', partial: true })
    const partialReadyLayout = resolveWorkspaceTaskLayout(workspaceSnapshot({ tasks: [partialReady] }))
    expect(partialReadyLayout.primaryTask).toBeNull()
    expect(partialReadyLayout.collapseRemoteSaving).toBe(false)
    expect(partialReadyLayout.banner?.kind).toBe('ready-to-save')

    const partialOpfs = makeRetainedTask({
      taskId: 'opfs-partial',
      lifecycle: transitionReceiveState(initialReceiveState('opfs-partial'), {
        kind: 'partial',
        reason: 'failures',
        savedFiles: 1n,
        failedFiles: 1n,
      }),
      canSavePartial: true,
    })
    expect(resolveWorkspaceTaskLayout(workspaceSnapshot({ tasks: [partialOpfs] }))).toMatchObject({
      primaryTask: null,
      collapseRemoteSaving: false,
      banner: null,
    })

    const portableHistory = makeRetainedTask({
      taskId: 'portable-1',
      backend: 'portable',
      lifecycle: transitionReceiveState(initialReceiveState('portable-1'), {
        kind: 'download-started',
        retained: false,
        fileName: 'photo.jpg',
      }),
    })
    expect(resolveWorkspaceTaskLayout(workspaceSnapshot({ tasks: [portableHistory] }))).toMatchObject({
      primaryTask: null,
      collapseRemoteSaving: false,
      banner: null,
    })

    const otherSharePaused = pausedTask('other-share', { shareInstance: SHARE_B })
    expect(resolveWorkspaceTaskLayout(workspaceSnapshot({ tasks: [otherSharePaused] }))).toMatchObject({
      primaryTask: null,
      banner: null,
    })
  })

  it('matches folder shares strictly by selectionKey and unfolds remote saving on selection change', () => {
    const localA = completeLocalTask('folder-a', { selectionKey: SEL_A })
    const matching = resolveWorkspaceTaskLayout(workspaceSnapshot({
      share: FOLDER_SHARE,
      selectionKey: SEL_A,
      tasks: [localA],
    }))
    expect(matching.primaryTask?.operationId).toBe(localA.taskId)
    expect(matching.collapseRemoteSaving).toBe(true)

    const switched = resolveWorkspaceTaskLayout(workspaceSnapshot({
      share: FOLDER_SHARE,
      selectionKey: SEL_B,
      tasks: [localA],
    }))
    expect(switched.primaryTask).toBeNull()
    expect(switched.collapseRemoteSaving).toBe(false)
    expect(switched.banner).toBeNull()
  })

  it('resolves continuation vs local copy precedence online, offline, and during pending actions', () => {
    const olderLocal = completeLocalTask('older-local', { createdAt: 1_000 })
    const newerPaused = pausedTask('newer-paused', { createdAt: 2_000, canSavePartial: true })

    // Online: newer paused attempt supersedes older complete local copy.
    const online = resolveWorkspaceTaskLayout(workspaceSnapshot({ tasks: [olderLocal, newerPaused] }))
    expect(online.primaryTask).toBeNull()
    expect(online.collapseRemoteSaving).toBe(false)
    expect(online.banner?.kind).toBe('continue')
    expect(online.banner?.task.operationId).toBe(newerPaused.taskId)
    expect(online.banner?.action.id).toBe('continue')

    // Online while continuing newerPaused: stays suppressed without flashing olderLocal.
    const continuing = resolveWorkspaceTaskLayout(workspaceSnapshot({
      tasks: [olderLocal, newerPaused],
      pending: { taskId: newerPaused.taskId, action: 'continue' },
    }))
    expect(continuing.primaryTask).toBeNull()
    expect(continuing.collapseRemoteSaving).toBe(false)
    expect(continuing.banner).toBeNull()

    // Newer complete local copy supersedes older paused task.
    const olderPaused = pausedTask('older-paused', { createdAt: 1_000 })
    const newerLocal = completeLocalTask('newer-local', { createdAt: 3_000 })
    const newerLocalLayout = resolveWorkspaceTaskLayout(workspaceSnapshot({ tasks: [olderPaused, newerLocal] }))
    expect(newerLocalLayout.primaryTask?.operationId).toBe(newerLocal.taskId)
    expect(newerLocalLayout.collapseRemoteSaving).toBe(true)
    expect(newerLocalLayout.banner).toBeNull()

    // Offline / share ended: uncontinuable newerPaused falls back to olderLocal and stays stable during re-export or save-partial.
    const offline = Object.freeze({ kind: 'unavailable' as const, reason: 'recovery-exhausted' as const })
    const offlineLayout = resolveWorkspaceTaskLayout(workspaceSnapshot({
      connection: offline,
      tasks: [newerPaused, olderLocal],
    }))
    expect(offlineLayout.primaryTask?.operationId).toBe(olderLocal.taskId)
    expect(offlineLayout.collapseRemoteSaving).toBe(true)
    expect(offlineLayout.banner).toBeNull()

    const offlineExporting = resolveWorkspaceTaskLayout(workspaceSnapshot({
      connection: offline,
      tasks: [newerPaused, olderLocal],
      pending: { taskId: olderLocal.taskId, action: 'save' },
    }))
    expect(offlineExporting.primaryTask?.operationId).toBe(olderLocal.taskId)
    expect(offlineExporting.collapseRemoteSaving).toBe(true)

    // Offline paused task with save-partial alone must never surface as a Continue banner.
    const offlinePausedOnly = resolveWorkspaceTaskLayout(workspaceSnapshot({
      connection: offline,
      tasks: [newerPaused],
    }))
    expect(offlinePausedOnly.primaryTask).toBeNull()
    expect(offlinePausedOnly.banner).toBeNull()
  })

  it('prefers exact-selection banners over newer tasks from another selection and suppresses other banners while continuing', () => {
    const olderLocalA = completeLocalTask('local-a', { createdAt: 1_000, selectionKey: SEL_A })
    const pausedA = pausedTask('paused-a', { createdAt: 2_000, selectionKey: SEL_A })
    const newerPausedB = pausedTask('paused-b', { createdAt: 3_000, selectionKey: SEL_B })
    const readyB = completeLocalTask('ready-b', { kind: 'ready-to-save', createdAt: 2_500, selectionKey: SEL_B })
    const tasks = [olderLocalA, pausedA, newerPausedB]

    const onA = resolveWorkspaceTaskLayout(workspaceSnapshot({
      share: FOLDER_SHARE,
      selectionKey: SEL_A,
      tasks,
    }))
    expect(onA.banner?.kind).toBe('continue')
    expect(onA.banner?.task.operationId).toBe(pausedA.taskId)

    const continuingA = resolveWorkspaceTaskLayout(workspaceSnapshot({
      share: FOLDER_SHARE,
      selectionKey: SEL_A,
      tasks,
      pending: { taskId: pausedA.taskId, action: 'continue' },
    }))
    expect(continuingA.primaryTask).toBeNull()
    expect(continuingA.banner).toBeNull()

    const onUnmatchedC = resolveWorkspaceTaskLayout(workspaceSnapshot({
      share: FOLDER_SHARE,
      selectionKey: SEL_C,
      tasks,
    }))
    expect(onUnmatchedC.banner?.kind).toBe('continue')
    expect(onUnmatchedC.banner?.task.operationId).toBe(newerPausedB.taskId)

    const readyBannerOnC = resolveWorkspaceTaskLayout(workspaceSnapshot({
      share: FOLDER_SHARE,
      selectionKey: SEL_C,
      tasks: [readyB],
    }))
    expect(readyBannerOnC.banner?.kind).toBe('ready-to-save')
    expect(readyBannerOnC.banner?.task.operationId).toBe(readyB.taskId)
    expect(readyBannerOnC.banner?.action.id).toBe('save')
  })

  it('handles active ready-to-save collapse, single-file repeat labels, and generation-scoped dismissal', () => {
    const activeReady = transitionReceiveState(initialReceiveState('active-opfs'), {
      kind: 'ready-to-save',
      size: 1024n,
      fileName: 'photo.jpg',
    })
    const activeReadySnap = workspaceSnapshot({
      output: {
        backend: 'opfs',
        lifecycle: activeReady,
        activeSelectionKey: SEL_A,
        actions: actionsForState(activeReady, 'opfs'),
      },
    })
    const activeReadyLayout = resolveWorkspaceTaskLayout(
      activeReadySnap,
      new Set([dismissedTaskKey('active-opfs', activeReady.generation)]),
    )
    expect(activeReadyLayout.primaryTask?.operationId).toBe('active-opfs')
    expect(activeReadyLayout.collapseRemoteSaving).toBe(true)

    const activeSaved = transitionReceiveState(initialReceiveState('active-fsa'), {
      kind: 'saved',
      savedFiles: 1n,
      savedBytes: 1024n,
    })
    const activeSavedSnap = workspaceSnapshot({
      output: {
        backend: 'fsa',
        lifecycle: activeSaved,
        activeSelectionKey: SEL_A,
        actions: actionsForState(activeSaved, 'fsa'),
      },
    })
    const visibleSaved = resolveWorkspaceTaskLayout(activeSavedSnap)
    expect(visibleSaved.collapseRemoteSaving).toBe(false)
    expect(visibleSaved.repeatRemoteLabel).toBe(true)
    const dismissedSaved = resolveWorkspaceTaskLayout(
      activeSavedSnap,
      new Set([dismissedTaskKey('active-fsa', activeSaved.generation)]),
    )
    expect(dismissedSaved.primaryTask).toBeNull()
    expect(dismissedSaved.repeatRemoteLabel).toBe(false)

    // Domain matcher directly returns none when all share candidates are ignored.
    const local = completeLocalTask('opfs-1')
    expect(resolveWorkspaceRetainedMatch({
      tasks: [local],
      shareInstance: SHARE_A,
      selectionKey: SEL_A,
      admission: {
        currentShareInstance: SHARE_A,
        hasActiveDownload: false,
        hasPendingAction: false,
        connectionUnavailable: false,
        shareEnded: false,
      },
      isIgnored: () => true,
    })).toEqual({ kind: 'none' })
  })
})
