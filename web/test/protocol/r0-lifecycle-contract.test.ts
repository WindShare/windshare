import { describe, expect, it } from 'vitest'

import {
  classifySelection,
  semantics,
  type CheckpointCut,
  type SelectionCase,
  type SemanticsVector,
} from './r0-semantics-fixture'

describe('R0 scheduling, recovery, and lifecycle contract', () => {
  it('enforces connection, output, source, and lifecycle state tables', () => {
    const selections = semantics.find((value) => value.name === 'selection-classification')
      ?.cases as readonly SelectionCase[]
    for (const selection of selections) {
      expect(classifySelection(selection)).toBe(selection.class)
    }

    expect(semantics.find((value) => value.name === 'operation-final-matrix')).toEqual({
      name: 'operation-final-matrix',
      operations: [
        { request: 'renew-lease', legalFinals: ['lease-result', 'operation-error'] },
        { request: 'release-lease', legalFinals: ['operation-complete', 'operation-error'] },
        { request: 'request-blocks', legalFinals: ['operation-complete', 'operation-error'] },
      ],
    })
    expect(semantics.find((value) => value.name === 'connection-timing')).toEqual({
      name: 'connection-timing',
      triggers: [
        {
          trigger: 'browse',
          startsP2P: false,
          p2pStartSeconds: null,
          applicationRelayDeadlineSeconds: null,
          outputPicker: 'none',
        },
        {
          trigger: 'preview-click',
          startsP2P: true,
          p2pStartSeconds: '0',
          applicationRelayDeadlineSeconds: '8',
          outputPicker: 'none',
        },
        {
          trigger: 'download-click',
          startsP2P: true,
          p2pStartSeconds: '0',
          applicationRelayDeadlineSeconds: '8',
          outputPicker: 'synchronous',
        },
      ],
      independentTimers: true,
      discoveryCannotDelay: true,
      unknownUsesNonSmallTiming: true,
      turnInsertionOnly: true,
    })

    const strictSequence = semantics.find((value) => value.name === 'strict-sequence') as
      | (SemanticsVector & {
          readonly cases: readonly {
            readonly epoch: number
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
    expect(new Set(laneEpochs?.globallyAllocated).size).toBe(
      laneEpochs?.globallyAllocated.length,
    )
    for (const value of laneEpochs?.cases ?? []) {
      expect(value.accepted).toBe(
        value.lastAccepted === null || value.candidate > value.lastAccepted,
      )
    }

    const checkpoints = semantics.find(
      (value) => value.name === 'output-checkpoint-crash-cuts',
    )
    expect(checkpoints?.order).toEqual([
      'data-write',
      'data-flush',
      'journal-write',
      'journal-flush',
      'atomic-install',
      'reopen-verify',
    ])
    const cuts = checkpoints?.cuts as readonly CheckpointCut[]
    expect(cuts.filter((cut) => cut.published).map((cut) => cut.cut)).toEqual([
      'after-reopen-verify',
    ])

    expect(semantics.find((value) => value.name === 'output-backend-capabilities')).toEqual({
      name: 'output-backend-capabilities',
      backends: [
        {
          backend: 'fsa',
          durability: 'none-until-reauthorization-and-reopen-proof',
          randomWrite: true,
          fileFailureIsolation: true,
          mtime: false,
          powerLoss: false,
        },
        {
          backend: 'opfs-staging',
          durability: 'process-restart',
          randomWrite: true,
          fileFailureIsolation: true,
          mtime: false,
          powerLoss: false,
        },
        {
          backend: 'single-file-stream',
          durability: 'none',
          randomWrite: false,
          fileFailureIsolation: false,
          mtime: false,
          failureAfterFirstByte: 'pause-job',
        },
        {
          backend: 'zip-stream',
          durability: 'none',
          randomWrite: false,
          fileFailureIsolation: false,
          mtime: false,
          memberStart: 'first-local-file-header-byte',
        },
        {
          backend: 'cli-osfs',
          durability: 'process-restart',
          randomWrite: true,
          fileFailureIsolation: true,
          mtime: true,
          powerLoss: false,
        },
      ],
    })
    expect(semantics.find((value) => value.name === 'zip-member-failure')).toEqual({
      name: 'zip-member-failure',
      cases: [
        {
          memberStarted: false,
          action: 'skip-and-report',
          jobOutcome: 'completed-with-errors',
        },
        { memberStarted: true, action: 'pause-job', jobOutcome: 'paused' },
      ],
    })

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
    const lifecycle = semantics.find((value) => value.name === 'offline-lifecycle') as
      | (SemanticsVector & {
          readonly transitions: readonly {
            readonly from: string
            readonly event: string
            readonly to: string
          }[]
          readonly explicitStopEffects: readonly string[]
          readonly unexpectedDisconnectStates: readonly string[]
        })
      | undefined
    expect(lifecycle?.states?.at(-1)).toBe('stopped')
    expect(lifecycle?.explicitStopUsesCrashGrace).toBe(false)
    expect(lifecycle?.transitions).toEqual([
      { from: 'preparing', event: 'registered', to: 'live-only' },
      { from: 'preparing', event: 'stop', to: 'stopping' },
      { from: 'live-only', event: 'begin-offline', to: 'offline-uploading' },
      { from: 'live-only', event: 'stop', to: 'stopping' },
      { from: 'offline-uploading', event: 'commit-ack', to: 'offline-committed' },
      { from: 'offline-uploading', event: 'stop', to: 'stopping' },
      { from: 'stopping', event: 'cleanup-complete', to: 'stopped' },
      { from: 'offline-committed', event: 'sender-exit', to: 'offline-committed' },
    ])
    expect(lifecycle?.explicitStopEffects).toContain('challenged-signed-stop')
    expect(lifecycle?.unexpectedDisconnectStates).toEqual([
      'live-only',
      'offline-uploading',
    ])
  })
})
