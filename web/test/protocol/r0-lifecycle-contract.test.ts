import { describe, expect, it } from 'vitest'

import {
  classifyConnectionSize,
  requireReceiveLifecycleSemanticsVector,
  semantics,
  type CheckpointCut,
  type ConnectionSizeCase,
  type SemanticsVector,
} from './r0-semantics-fixture'

describe('R0 scheduling, recovery, and lifecycle contract', () => {
  it('keeps connection sizing separate from the artifact shape proof', () => {
    const connectionSizes = semantics.find((value) => value.name === 'connection-size-classification')
      ?.cases as readonly ConnectionSizeCase[]
    for (const value of connectionSizes) {
      expect(classifyConnectionSize(value)).toBe(value.class)
    }

    expect(semantics.find((value) => value.name === 'artifact-shape-proof')).toEqual({
      name: 'artifact-shape-proof',
      proofs: [
        { proof: 'unknown', byte: 1 },
        { proof: 'none', byte: 2 },
        { proof: 'single-file', byte: 3 },
        { proof: 'tree', byte: 4 },
      ],
      allowedTransitions: ['unknown->none', 'unknown->single-file', 'unknown->tree'],
      treeForcingFacts: [
        'authenticated-selected-directory',
        'explicit-empty-directory',
        'second-authenticated-file',
      ],
      singleFileRequires: [
        'one-authenticated-file',
        'frozen-rule-exclusion',
        'empty-unsettled-targets',
      ],
      noneRequires: ['complete-negative-evidence'],
      finalProofImmutable: true,
    })
  })

  it('allows picker acquisition only inside a final artifact action', () => {
    expect(semantics.find((value) => value.name === 'artifact-action-picker-timing')).toEqual({
      name: 'artifact-action-picker-timing',
      events: [
        { event: 'background-projection', startsP2P: false, picker: 'forbidden' },
        { event: 'preview-click', startsP2P: true, picker: 'none' },
        { event: 'final-artifact-action-without-picker', startsP2P: true, picker: 'none' },
        {
          event: 'final-artifact-action-with-picker',
          startsP2P: true,
          picker: 'synchronous-before-click-stack-unwinds',
        },
      ],
      p2pStartSeconds: '0',
      applicationRelayDeadlineSeconds: '8',
      backgroundCompletionCannotInvokeAction: true,
      authorityValidationMayContinueAfterPickerStart: true,
      bindRechecks: ['projection-epoch', 'shape-proof', 'artifact-offer', 'capability-facts'],
    })
  })

  it('freezes operation finals, frame sequencing, and lane epochs', () => {
    expect(semantics.find((value) => value.name === 'operation-final-matrix')).toEqual({
      name: 'operation-final-matrix',
      operations: [
        { request: 'renew-lease', legalFinals: ['lease-result', 'operation-error'] },
        { request: 'release-lease', legalFinals: ['operation-complete', 'operation-error'] },
        { request: 'request-blocks', legalFinals: ['operation-complete', 'operation-error'] },
      ],
    })

    const strictSequence = semantics.find((value) => value.name === 'strict-sequence') as
      | (SemanticsVector & {
          readonly cases: readonly {
            readonly expected: string
            readonly candidate: string
            readonly accepted: boolean
          }[]
        })
      | undefined
    for (const value of strictSequence?.cases ?? []) {
      expect(value.accepted).toBe(
        value.expected !== 'closed' && BigInt(value.candidate) === BigInt(value.expected),
      )
    }

    const laneEpochs = semantics.find((value) => value.name === 'lane-epoch-acceptance') as
      | (SemanticsVector & {
          readonly globallyAllocated: readonly number[]
          readonly cases: readonly {
            readonly lastAccepted: number | null
            readonly candidate: number
            readonly accepted: boolean
          }[]
        })
      | undefined
    expect(new Set(laneEpochs?.globallyAllocated).size).toBe(laneEpochs?.globallyAllocated.length)
    for (const value of laneEpochs?.cases ?? []) {
      expect(value.accepted).toBe(
        value.lastAccepted === null || value.candidate > value.lastAccepted,
      )
    }
  })

  it('selects a new FileCheckpointV2 generation only after reopen verification', () => {
    const checkpoints = semantics.find(
      (value) => value.name === 'file-checkpoint-v2-crash-cuts',
    ) as SemanticsVector & {
      readonly ownershipMarker: string
      readonly namespace: string
      readonly selectionAuthority: string
    }
    expect(checkpoints.ownershipMarker).toBe('windshare/file-checkpoint/v2')
    expect(checkpoints.namespace).toBe('.windshare-output/checkpoints-v2')
    expect(checkpoints.selectionAuthority).toBe('highest-reopened-verified-generation')
    expect(checkpoints.order).toEqual([
      'data-write',
      'data-flush',
      'candidate-record-write',
      'candidate-record-flush',
      'verified-record-install',
      'reopen-verify',
    ])
    const cuts = checkpoints.cuts as readonly CheckpointCut[]
    expect(cuts.filter((cut) => cut.newGenerationSelectable).map((cut) => cut.cut)).toEqual([
      'after-reopen-verify',
    ])
  })

  it('uses the closed artifact, plan, binding, and guarantee rows', () => {
    expect(semantics.find((value) => value.name === 'artifact-plan-guarantee-matrix')).toEqual({
      name: 'artifact-plan-guarantee-matrix',
      rows: [
        {
          artifact: 'directory-tree',
          layout: 'single-file-or-result-root',
          plan: 'direct-tree',
          binding: 'named-container-entry',
          guaranteeProfiles: ['native-tree', 'fsa-tree'],
          preparation: 'none',
          completion: 'prefix-visible-partial-legal',
        },
        {
          artifact: 'directory-tree',
          layout: 'catalog-root',
          plan: 'direct-tree',
          binding: 'container-root',
          guaranteeProfiles: ['native-tree'],
          preparation: 'none',
          completion: 'prefix-visible',
        },
        {
          artifact: 'original-file',
          layout: 'single-file-proof',
          plan: 'direct-atomic',
          binding: 'atomic-target',
          guaranteeProfiles: ['managed-atomic'],
          preparation: 'none',
          completion: 'published-after-verified-commit',
        },
        {
          artifact: 'zip-archive',
          layout: 'result-root',
          plan: 'direct-atomic',
          binding: 'atomic-target',
          guaranteeProfiles: ['managed-atomic'],
          preparation: 'progressive-immutable-ledger',
          completion: 'complete-only',
        },
        {
          artifact: 'original-file',
          layout: 'single-file-proof',
          plan: 'workspace-then-publish',
          binding: 'origin-private-workspace',
          guaranteeProfiles: ['managed-atomic', 'browser-handoff'],
          preparation: 'none',
          completion: 'sealed-then-waiting-to-save',
        },
        {
          artifact: 'zip-archive',
          layout: 'result-root',
          plan: 'workspace-then-publish',
          binding: 'origin-private-workspace',
          guaranteeProfiles: ['managed-atomic', 'browser-handoff'],
          preparation: 'exact-zip',
          completion: 'complete-only-sealed-then-waiting-to-save',
        },
        {
          artifact: 'original-file-or-zip-archive',
          layout: 'explicit-artifact',
          plan: 'portable-handoff',
          binding: 'portable',
          guaranteeProfiles: ['browser-handoff'],
          preparation: 'exact-artifact',
          completion: 'download-started-only',
        },
      ],
    })
  })

  it('freezes WorkspaceBudget accounting and complete-only ZIP failure', () => {
    expect(semantics.find((value) => value.name === 'workspace-budget-v1')).toEqual({
      name: 'workspace-budget-v1',
      components: [
        'uniqueRawBytes',
        'packageBytes',
        'peakTemporaryBytes',
        'durableMetadataBytes',
      ],
      derivedPeak: 'checked-sum-components',
      ownedObjectCountedOnce: true,
      quotaEstimateIsReservation: false,
      limits: {
        DEFAULT_OPFS_JOB_WORKSPACE_LIMIT: '8589934592',
        DEFAULT_OPFS_PROCESS_WORKSPACE_LIMIT: '17179869184',
        MINIMUM_OPFS_QUOTA_RESERVE: '536870912',
        DEFAULT_PORTABLE_HANDOFF_ARTIFACT_LIMIT: '67108864',
      },
      admissionChecks: [
        'job-peak',
        'process-active-job-peaks',
        'quota-minus-usage-minus-reserve',
        'every-allocation',
      ],
    })
    expect(semantics.find((value) => value.name === 'zip-complete-only')).toEqual({
      name: 'zip-complete-only',
      encoding: 'store',
      completeness: 'complete-only',
      cases: [
        {
          failure: 'discovery',
          action: 'abort-artifact',
          artifactOutcome: 'failed',
          publicationAllowed: false,
          partialResult: false,
        },
        {
          failure: 'member-before-header',
          action: 'abort-artifact',
          artifactOutcome: 'failed',
          publicationAllowed: false,
          partialResult: false,
        },
        {
          failure: 'member-after-header',
          action: 'abort-artifact',
          artifactOutcome: 'failed',
          publicationAllowed: false,
          partialResult: false,
        },
      ],
    })
  })

  it('distinguishes receiver terminal projections without inventing publication', () => {
    const lifecycle = requireReceiveLifecycleSemanticsVector(semantics.find(
      (value) => value.name === 'receive-lifecycle-terminal-states',
    ))
    expect(lifecycle.states.map((state) => state.state)).toEqual([
      'published',
      'download-started',
      'partial-directory',
      'restart-required',
      'discarded',
      'expired',
      'needs-attention',
    ])
    expect(lifecycle.states.map((state) => state.byte)).toEqual([14, 15, 16, 17, 18, 19, 20])
    expect(lifecycle.deadlineWritingStates).toEqual([
      'resumable-receive',
      'resumable-package',
      'waiting-to-save',
    ])
    expect(lifecycle.publishedCleanupPendingRemains).toBe('published')
    expect(lifecycle.handoffNeverMeans).toBe('published')
    expect(lifecycle.completeArtifactsExclude).toEqual(['partial-directory'])
  })

  it('preserves unrelated catalog, source, and sender lifecycle contracts', () => {
    const catalog = semantics.find((value) => value.name === 'catalog-transaction')
    expect(catalog?.preCommitCrashVisible).toBe(false)
    expect(catalog?.publishOnlyAfter?.at(-1)).toBe('atomic-commit')
    expect(semantics.find((value) => value.name === 'stable-source-platforms')).toEqual({
      name: 'stable-source-platforms',
      platforms: [
        {
          platform: 'windows-local-ntfs-refs',
          mechanism: 'deny-share-write-handle+volume-file-id',
          supported: true,
        },
        {
          platform: 'linux-local-regular',
          mechanism: 'device+inode+size+mtime-ns+ctime-ns',
          supported: true,
        },
        {
          platform: 'darwin-local-regular',
          mechanism: 'device+inode+size+mtime-ns+ctime-ns',
          supported: true,
        },
        {
          platform: 'other-network-pseudo',
          mechanism: 'unsupported-stability',
          supported: false,
        },
      ],
    })
    const senderLifecycle = semantics.find((value) => value.name === 'offline-lifecycle') as
      | (SemanticsVector & {
          readonly states: readonly string[]
          readonly transitions: readonly { from: string; event: string; to: string }[]
          readonly explicitStopEffects: readonly string[]
          readonly unexpectedDisconnectStates: readonly string[]
        })
      | undefined
    expect(senderLifecycle?.states?.at(-1)).toBe('stopped')
    expect(senderLifecycle?.explicitStopUsesCrashGrace).toBe(false)
    expect(senderLifecycle?.transitions).toEqual([
      { from: 'preparing', event: 'registered', to: 'live-only' },
      { from: 'preparing', event: 'stop', to: 'stopping' },
      { from: 'live-only', event: 'begin-offline', to: 'offline-uploading' },
      { from: 'live-only', event: 'stop', to: 'stopping' },
      { from: 'offline-uploading', event: 'commit-ack', to: 'offline-committed' },
      { from: 'offline-uploading', event: 'stop', to: 'stopping' },
      { from: 'stopping', event: 'cleanup-complete', to: 'stopped' },
      { from: 'offline-committed', event: 'sender-exit', to: 'offline-committed' },
    ])
    expect(senderLifecycle?.explicitStopEffects).toContain('challenged-signed-stop')
    expect(senderLifecycle?.unexpectedDisconnectStates).toEqual(['live-only', 'offline-uploading'])
  })
})
