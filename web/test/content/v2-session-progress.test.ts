import { describe, expect, it } from 'vitest'

import type { V2CatalogPageRequest } from '../../src/catalog/v2-records'
import { V2CatalogSessionOperations } from '../../src/content/v2-session-services'
import {
  encodeV2Body,
  type V2SessionMessage,
  V2_MESSAGE_KIND,
} from '../../src/session/v2-message'
import type { V2ReceiverSessionRuntime } from '../../src/session/v2-runtime'
import type { V2SessionOperation } from '../../src/session/v2-runtime-types'

const request: V2CatalogPageRequest = Object.freeze({
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
        V2_MESSAGE_KIND.catalogResult,
        encodeV2Body(new Map<number, unknown>([[0, 1], [1, finalObject]])),
      ),
    ])
    const observed: bigint[] = []
    const result = await new V2CatalogSessionOperations(
      session as unknown as V2ReceiverSessionRuntime,
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
      await expect(new V2CatalogSessionOperations(
        session as unknown as V2ReceiverSessionRuntime,
      ).fetchPage(request, new AbortController().signal)).rejects.toMatchObject({ scope: 'session' })
      expect(session.closeCalls).toBe(1)
    })
  }

  it('terminates the ProtocolSession when catalog receives another error scope', async () => {
    const session = new FakeCatalogSession([
      controlMessage(
        V2_MESSAGE_KIND.operationError,
        encodeV2Body(new Map<number, unknown>([
          [0, 1], [1, 5], [2, 0x5001], [3, false], [4, null],
          [5, 'Peer negotiation failed'],
        ])),
      ),
    ])
    await expect(new V2CatalogSessionOperations(
      session as unknown as V2ReceiverSessionRuntime,
    ).fetchPage(request, new AbortController().signal)).rejects.toMatchObject({ scope: 'session' })
    expect(session.closeCalls).toBe(1)
  })
})

class FakeCatalogSession {
  readonly #messages: V2SessionMessage[]
  closeCalls = 0

  constructor(messages: readonly V2SessionMessage[]) {
    this.#messages = [...messages]
  }

  async beginOperation(): Promise<V2SessionOperation> {
    return {
      id: identity(9),
      requestKind: V2_MESSAGE_KIND.listChildren,
      next: async () => {
        const message = this.#messages.shift()
        if (message === undefined) throw new Error('test operation exhausted')
        return message
      },
      close: () => undefined,
    }
  }

  async close(): Promise<void> {
    this.closeCalls += 1
  }
}

function progressMessage(attemptId: Uint8Array, discoveredEntries: bigint): V2SessionMessage {
  return controlMessage(
    V2_MESSAGE_KIND.scanProgress,
    encodeV2Body(new Map<number, unknown>([
      [0, 1], [1, attemptId], [2, discoveredEntries],
    ])),
  )
}

function controlMessage(
  kind: V2SessionMessage['kind'],
  body: Uint8Array,
): V2SessionMessage {
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
