import { LaneRequests } from '../../src/content/scheduling/requests'
import { describe, expect, it } from 'vitest'

import type { CatalogPageRequest } from '../../src/catalog/records'
import { CatalogSessionOperations } from '../../src/content/session-services'
import {
  encodeBody,
  type SessionMessage,
  MESSAGE_KIND,
} from '../../src/session/message'
import type { ReceiverSessionRuntime } from '../../src/session/runtime'
import type { SessionOperation } from '../../src/session/runtime-types'

const request: CatalogPageRequest = Object.freeze({
  directoryId: identity(1),
  pageIndex: 0,
})

describe('v2 catalog session scan progress', () => {
  it('reports increasing milestones before final and suppresses equal replay', async () => {
    const attempt = identity(2)
    const finalObject = Uint8Array.of(7, 8, 9)
    const session = new FakeCatalogSession([
      progressMessage(attempt, 1n),
      progressMessage(attempt, 1n),
      progressMessage(attempt, 256n),
      controlMessage(
        MESSAGE_KIND.catalogResult,
        encodeBody(new Map<number, unknown>([[0, 1], [1, finalObject]])),
      ),
    ])
    const observed: bigint[] = []
    const result = await new CatalogSessionOperations(
      session as unknown as ReceiverSessionRuntime, requestLanes(),
    ).fetchPage(request, new AbortController().signal, (progress) => {
      observed.push(progress.discoveredEntries)
    })
    expect(result).toEqual(finalObject)
    expect(observed).toEqual([1n, 256n])
    expect(session.closeCalls).toBe(0)
  })

  for (const [name, hostile] of [
    ['regression', progressMessage(identity(3), 5n)],
    ['attempt substitution', progressMessage(identity(4), 11n)],
  ] as const) {
    it(`terminates the ProtocolSession on authenticated ${name}`, async () => {
      const initialAttempt = name === 'regression' ? identity(3) : identity(5)
      const session = new FakeCatalogSession([
        progressMessage(initialAttempt, 10n),
        hostile,
      ])
      await expect(new CatalogSessionOperations(
        session as unknown as ReceiverSessionRuntime, requestLanes(),
      ).fetchPage(request, new AbortController().signal)).rejects.toMatchObject({ scope: 'session' })
      expect(session.closeCalls).toBe(1)
    })
  }
})

class FakeCatalogSession {
  readonly #messages: SessionMessage[]
  closeCalls = 0

  constructor(messages: readonly SessionMessage[]) {
    this.#messages = [...messages]
  }

  async beginOperation(): Promise<SessionOperation> {
    return {
      id: identity(9),
      requestKind: MESSAGE_KIND.listChildren,
      next: async () => {
        const message = this.#messages.shift()
        if (message === undefined) throw new Error('test operation exhausted')
        return message
      },
      cancel: () => undefined,
    }
  }

  async close(): Promise<void> {
    this.closeCalls += 1
  }
}

function progressMessage(attemptId: Uint8Array, discoveredEntries: bigint): SessionMessage {
  return controlMessage(
    MESSAGE_KIND.scanProgress,
    encodeBody(new Map<number, unknown>([
      [0, 1], [1, attemptId], [2, discoveredEntries],
    ])),
  )
}

function controlMessage(
  kind: SessionMessage['kind'],
  body: Uint8Array,
): SessionMessage {
  return {
    kind,
    operationId: identity(9),
    body: body.slice(),
    plaintext: Uint8Array.of(1),
    data: false,
  }
}

function identity(first: number): Uint8Array<ArrayBuffer> {
  const value = new Uint8Array(16)
  value[0] = first
  return value
}

function requestLanes(): LaneRequests {
  const lanes = new LaneRequests()
  lanes.add({ id: 1, epoch: 0, route: 'application-relay' })
  return lanes
}
