import { describe, expect, it } from 'vitest'

import type { V2CatalogEntry } from '../../src/catalog/v2-records'
import { V2SelectionPolicy } from '../../src/catalog/v2-selection'
import { encodeBase64Url } from '../../src/crypto/bytes'
import { browserBuildSnapshot } from '../../src/diagnostics/build-identity'
import { createBrowserDiagnosticsComposition } from '../../src/diagnostics/browser-composition'
import {
  createIncidentScopeIssuer,
  type FailureFactRelation,
  type IncidentScopeKind,
  type PresentationDecision,
} from '../../src/diagnostics/incident'
import { ArtifactPlanningContractError } from '../../src/output/planning'
import { V2ActivationStateContractError } from '../../src/ui/controller/activation-model'
import { V2ControllerObservability } from '../../src/ui/controller/controller-observability'
import type { V2ReceiverIncidentPort } from '../../src/ui/controller/contracts'
import { V2PresentationAttempt } from '../../src/ui/controller/presentation-attempt'
import { projectBrowsePage } from '../../src/ui/v2-controller-state'
import type { V2BrowseDirectory, V2BrowsePage } from '../../src/ui/v2-gateway'
import type { V2RouteCommitResult } from '../../src/ui/v2-receive-runtime'

describe('v2 browse page presentation', () => {
  it('reports only visible selection facts and leaves artifact semantics to projection', () => {
    const root = directory()
    const selected = file(2, 'selected.txt', 1_024n)
    const notSelected = file(3, 'other.txt', 2_048n)
    const selection = new V2SelectionPolicy(false)
    selection.toggle(selected, root.ancestry)

    const projection = projectBrowsePage(page(root, [selected, notSelected]), selection, [root])

    expect(projection.snapshot).toMatchObject({
      phase: 'browsing',
      selectedVisibleFiles: 1,
      selectedVisibleBytes: 1_024n,
    })
    expect(projection.snapshot.rows.map((row) => row.selection)).toEqual(['selected', 'unselected'])
  })
})

describe('v2 controller diagnostics', () => {
  it('keeps clean pre-cut replacement distinct from post-cut ownership', () => {
    const result = Object.freeze({
      kind: 'retryable-precut',
      receiverOperationId: 'AgAAAAAAAAAAAAAAAAAAAA',
    }) satisfies V2RouteCommitResult

    expect(result).toEqual({
      kind: 'retryable-precut',
      receiverOperationId: 'AgAAAAAAAAAAAAAAAAAAAA',
    })
    expect(result).not.toHaveProperty('operation')
    expect(result).not.toHaveProperty('authority')
  })

  it('keeps the initiating trigger before consequences and closes after owned cleanup', () => {
    const issuer = createIncidentScopeIssuer()
    const order: string[] = []
    const decisions: PresentationDecision[] = []
    const incidents: V2ReceiverIncidentPort = {
      openScope: (kind: IncidentScopeKind) => issuer.open(kind, {
        factRecorded: ({ relation }: { relation: FailureFactRelation }) => {
          order.push(`fact:${relation}`)
        },
        scopeClosed: () => order.push('closed'),
      }),
      submitDecision: (_scope, decision) => {
        decisions.push(decision)
        order.push('decision')
      },
    }
    const attempt = new V2PresentationAttempt(incidents, 'authority_activation')
    const trigger = attempt.recordUnclassified('authority_activation', 'contributor')!

    attempt.incident('projection_authority', 'failed', trigger)
    attempt.recordUnclassified('cleanup', 'consequence', 'none')
    attempt.close()

    expect(order).toEqual(['fact:contributor', 'decision', 'fact:consequence', 'closed'])
    expect(decisions).toEqual([
      expect.objectContaining({ kind: 'incident', boundary: 'projection_authority', trigger }),
    ])
  })

  it('keeps reporter failure passive and constructs trace payloads only for a current observer', () => {
    const issuer = createIncidentScopeIssuer()
    const attempt = new V2PresentationAttempt({
      openScope: (kind) => issuer.open(kind),
      submitDecision: () => { throw new Error('reporter unavailable') },
    }, 'join')
    const trigger = attempt.recordUnclassified('join', 'contributor')!

    expect(() => attempt.incident('join', 'failed', trigger)).not.toThrow()
    expect(attempt.decisionSettled).toBe(true)
    expect(() => attempt.close()).not.toThrow()

    const traceState: {
      observer?: (event: { readonly name: string }) => void
    } = {}
    let payloadsConstructed = 0
    const observed: string[] = []
    const observability = new V2ControllerObservability({
      trace: { get current() { return traceState.observer } },
    })
    observability.trace(() => {
      payloadsConstructed += 1
      return Object.freeze({ name: 'join_transition', transition: 'started' })
    })
    traceState.observer = (event) => observed.push(event.name)
    observability.trace(() => {
      payloadsConstructed += 1
      return Object.freeze({ name: 'join_transition', transition: 'joined' })
    })

    expect(payloadsConstructed).toBe(1)
    expect(observed).toEqual(['join_transition'])
  })

  it.each([
    new ArtifactPlanningContractError('same-epoch-selection-digest-changed'),
    new V2ActivationStateContractError('impossible activation transition'),
  ])('classifies %s as a terminal output Contract fault', (error) => {
    const composition = diagnosticsComposition()
    const observability = new V2ControllerObservability({ incidents: composition.incidents })
    const attempt = observability.open('authority_activation')

    observability.fail(attempt, 'projection_authority', error, 'authority_activation')
    attempt.close()

    const incident = composition.runtime.inspectLastFailure()
    expect(incident?.payload.trigger).toEqual({
      kind: 'fault',
      stage: 'authority_activation',
      recovery_disposition: 'terminal',
      payload: {
        fault: {
          domain: 'output',
          scope: 'output_pause',
          code: 'contract',
        },
      },
    })
    expect(JSON.stringify(incident)).not.toContain('unclassified')
  })
})

function diagnosticsComposition() {
  let now = 1_000
  return createBrowserDiagnosticsComposition({
    build: browserBuildSnapshot(),
    secureContext: true,
    consoleSink: Object.freeze({ error: () => undefined }),
    randomBytes: (byteLength) =>
      Uint8Array.from({ length: byteLength }, (_, index) => index + 1),
    clock: Object.freeze({
      nowMilliseconds: () => now++,
      captureTime: () => new Date(now++).toISOString(),
    }),
    scheduler: Object.freeze({
      schedule: () => Object.freeze({ cancel: () => undefined }),
    }),
  })
}

function directory(): V2BrowseDirectory {
  return Object.freeze({
    id: identity(1),
    idText: identityText(1),
    name: 'Shared files',
    path: Object.freeze([]),
    ancestry: Object.freeze([identityText(1)]),
  })
}

function page(
  root: V2BrowseDirectory,
  entries: readonly V2CatalogEntry[],
): V2BrowsePage {
  return Object.freeze({
    directory: root,
    pageIndex: 0,
    pageCount: 2,
    entryCount: entries.length,
    omittedCount: 0n,
    entries,
  })
}

function file(
  first: number,
  name: string,
  expectedSize: bigint,
): Extract<V2CatalogEntry, { kind: 'file' }> {
  return Object.freeze({
    kind: 'file',
    id: identity(first),
    idText: identityText(first),
    name,
    expectedSize,
  })
}

function identity(first: number): Uint8Array<ArrayBuffer> {
  const value = new Uint8Array(16)
  value[0] = first
  return value
}

function identityText(first: number): string {
  return encodeBase64Url(identity(first))
}
