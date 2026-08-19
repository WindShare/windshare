import { describe, expect, it } from 'vitest'

import type { V2CatalogEntry } from '../../src/catalog/v2-records'
import { V2SelectionPolicy } from '../../src/catalog/v2-selection'
import { encodeBase64Url } from '../../src/crypto/bytes'
import {
  createIncidentScopeIssuer,
  type FailureFactRelation,
  type IncidentScopeKind,
  type PresentationDecision,
} from '../../src/diagnostics/incident'
import { V2ControllerObservability } from '../../src/ui/controller/controller-observability'
import type { V2ReceiverIncidentPort } from '../../src/ui/controller/contracts'
import { V2PresentationAttempt } from '../../src/ui/controller/presentation-attempt'
import { projectBrowsePage } from '../../src/ui/v2-controller-state'
import type { V2BrowseDirectory, V2BrowsePage } from '../../src/ui/v2-gateway'

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
})

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
