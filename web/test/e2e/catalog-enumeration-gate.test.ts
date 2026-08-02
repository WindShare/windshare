import { once } from 'node:events'
import { createServer, type Server } from 'node:net'

import { afterEach, describe, expect, it } from 'vitest'

import {
  CatalogEnumerationGateController,
} from '../../e2e/fixtures/catalog-enumeration-gate'
import type { TestEvent } from '../../scripts/browser-evidence/process/test-event-channel.mjs'

const RESPONSE_SCHEMA_VERSION = 'windshare.catalog-enumeration-gate-response/v1'
const CONTROL_SCHEMA_VERSION = 'windshare.catalog-enumeration-gate-control/v1'
const TEST_IDENTITY = Object.freeze({
  runId: 'catalog-run',
  operationId: 'catalog-operation',
  scenario: 'catalog-scenario',
})
const TEST_TIMEOUT_MILLISECONDS = 2_000

describe('catalog enumeration gate owner controller', () => {
  const servers: Server[] = []

  afterEach(async () => {
    await Promise.all(servers.splice(0).map(closeServer))
  })

  it('commits the exact identity-bound release with EOF and retires local replay authority', async () => {
    let connectionCount = 0
    let requestEnded = false
    let resolveRequest: (request: string) => void = () => undefined
    const request = new Promise<string>((resolve) => {
      resolveRequest = resolve
    })
    const server = createServer((socket) => {
      connectionCount += 1
      const chunks: Buffer[] = []
      socket.on('data', (chunk: Buffer) => chunks.push(chunk))
      socket.once('end', () => {
        requestEnded = true
        resolveRequest(Buffer.concat(chunks).toString('utf8'))
        socket.end(JSON.stringify({
          schema_version: RESPONSE_SCHEMA_VERSION,
          action: 'release',
          outcome: 'released',
          blocked_requests: 1,
          released: true,
        }) + '\n')
      })
    })
    servers.push(server)
    const address = await listenLoopback(server)
    const controller = CatalogEnumerationGateController.fromPrivateReadyEvent(
      privateReadyEvent(address),
    )

    const released = await controller.release({ timeoutMilliseconds: TEST_TIMEOUT_MILLISECONDS })

    expect(requestEnded).toBe(true)
    expect(released).toEqual({ blockedRequests: 1, released: true })
    await expect(request).resolves.toBe(JSON.stringify({
      schema_version: CONTROL_SCHEMA_VERSION,
      run_id: TEST_IDENTITY.runId,
      operation_id: TEST_IDENTITY.operationId,
      scenario: TEST_IDENTITY.scenario,
      action: 'release',
    }) + '\n')
    await expect(controller.release()).rejects.toThrow('single-use')
    expect(connectionCount).toBe(1)
    expect(JSON.stringify(controller)).toBe('{}')
    let disclosureFailure: unknown
    try {
      controller.assertNotDisclosed('test material', 'prefix-' + address)
    } catch (error) {
      disclosureFailure = error
    }
    expect(disclosureFailure).toBeInstanceOf(Error)
    expect((disclosureFailure as Error).message).not.toContain(address)
  })

  it('rejects a noncanonical response without disclosing the listener capability', async () => {
    const server = createServer((socket) => {
      socket.resume()
      socket.once('end', () => {
        socket.end(JSON.stringify({
          action: 'snapshot',
          schema_version: RESPONSE_SCHEMA_VERSION,
          outcome: 'observed',
          blocked_requests: 1,
          released: false,
        }) + '\n')
      })
    })
    servers.push(server)
    const address = await listenLoopback(server)
    const controller = CatalogEnumerationGateController.fromPrivateReadyEvent(
      privateReadyEvent(address),
    )

    const failure = await controller.snapshot({
      timeoutMilliseconds: TEST_TIMEOUT_MILLISECONDS,
    }).then(
      () => undefined,
      (error: unknown) => error,
    )

    expect(failure).toBeInstanceOf(Error)
    expect((failure as Error).message).toContain('fields are invalid')
    expect((failure as Error).message).not.toContain(address)
  })

  it('fails closed when release is attempted before any scan is blocked', async () => {
    let connectionCount = 0
    const server = createServer((socket) => {
      connectionCount += 1
      socket.resume()
      socket.once('end', () => {
        socket.end(JSON.stringify({
          schema_version: RESPONSE_SCHEMA_VERSION,
          action: 'release',
          outcome: 'observed',
          blocked_requests: 0,
          released: false,
        }) + '\n')
      })
    })
    servers.push(server)
    const controller = CatalogEnumerationGateController.fromPrivateReadyEvent(
      privateReadyEvent(await listenLoopback(server)),
    )

    await expect(controller.release({
      timeoutMilliseconds: TEST_TIMEOUT_MILLISECONDS,
    })).rejects.toThrow('semantically invalid')
    await expect(controller.release()).rejects.toThrow('single-use')
    expect(connectionCount).toBe(1)
  })
})

function privateReadyEvent(address: string): TestEvent {
  return Object.freeze({
    schemaVersion: 'windshare.test-event/v1',
    ...TEST_IDENTITY,
    component: 'windshare_share',
    milestone: 'catalog_gate_ready',
    outcome: 'succeeded',
    payload: Object.freeze({ address }),
  })
}

async function listenLoopback(server: Server): Promise<string> {
  server.listen(0, '127.0.0.1')
  await once(server, 'listening')
  const address = server.address()
  if (address === null || typeof address === 'string') {
    throw new Error('Test catalog gate server did not publish an address')
  }
  return '127.0.0.1:' + String(address.port)
}

async function closeServer(server: Server): Promise<void> {
  if (!server.listening) return
  await new Promise<void>((resolveClose, rejectClose) => {
    server.close((error) => {
      if (error === undefined) resolveClose()
      else rejectClose(error)
    })
  })
}
