import { createHash } from 'node:crypto'
import { mkdtemp, rename, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

import { describe, expect, it } from 'vitest'

import {
  deliverAndEraseRawChildInput,
  deliverAndEraseOwnerRequest,
  holdLinuxProcessOwnerArtifact,
  LINUX_PROCESS_OWNER_STATUS_SCHEMA_VERSION,
  parseLinuxProcessOwnerStatus,
  requestOwnerSettlement,
} from '../../scripts/browser-evidence/process/linux-process-owner-client'

describe('Linux process owner raw input delivery', () => {
  it('erases the raw input when Writable.end throws synchronously', async () => {
    const failure = new Error('injected synchronous write failure')
    const rawInput = Buffer.from('ephemeral credential', 'utf8')
    const pipe = {
      once() {
        return this
      },
      end() {
        throw failure
      },
    } as unknown as Parameters<typeof deliverAndEraseRawChildInput>[0]

    await expect(deliverAndEraseRawChildInput(pipe, rawInput)).resolves.toBe(failure)
    expect(rawInput.every((byte) => byte === 0)).toBe(true)
  })

  it('erases the owner request when its control stream throws synchronously', async () => {
    const failure = new Error('injected request write failure')
    const request = Buffer.from('canonical request', 'utf8')
    const pipe = {
      once() { return this },
      end() { throw failure },
    } as unknown as Parameters<typeof deliverAndEraseOwnerRequest>[0]

    await expect(deliverAndEraseOwnerRequest(pipe, request)).resolves.toBe(failure)
    expect(request.every((byte) => byte === 0)).toBe(true)
  })

  it('reports a synchronous owner-settlement control failure', async () => {
    const failure = new Error('injected control write failure')
    const pipe = {
      once() { return this },
      write() { throw failure },
    } as unknown as Parameters<typeof requestOwnerSettlement>[0]

    await expect(requestOwnerSettlement(pipe)).resolves.toBe(failure)
  })
})

describe('Linux process owner status state machine', () => {
  it('accepts the exact launched, delivered, tree-empty terminal state', () => {
    const parsed = parseStatus(validStatus())
    expect(parsed.treeEmpty).toBe(true)
    expect(parsed.inputEvidence.outcome).toBe('delivered')
  })

  it.each([
    ['launched with not-started control', (value: StatusFixture) => {
      value.ownershipEvidence.controlOutcome = 'not-started'
    }],
    ['tree-empty after authority loss', (value: StatusFixture) => {
      value.ownershipEvidence.controlOutcome = 'ownership-evidence-failure'
    }],
    ['unlaunched completed cleanup', (value: StatusFixture) => {
      value.launched = false
      value.processEvidence = {
        terminal: 'spawn-failed', errorCode: 'SPAWN_FAILED', errorMessage: 'failed',
      }
      value.ownershipEvidence.rootPid = null
      value.ownershipEvidence.rootStartTimeTicks = ''
    }],
    ['uint64-overflow root starttime', (value: StatusFixture) => {
      value.ownershipEvidence.rootStartTimeTicks = '18446744073709551616'
    }],
    ['timeout without deadline control', (value: StatusFixture) => {
      value.timedOut = true
    }],
    ['failed input without bounded failure', (value: StatusFixture) => {
      value.inputEvidence.outcome = 'failed'
    }],
    ['failed cleanup without bounded failure', (value: StatusFixture) => {
      value.treeEmpty = false
      value.ownershipEvidence.cleanupOutcome = 'failed'
      value.ownershipEvidence.quietInventoryCount = 0
    }],
  ])('rejects %s', (_label, mutate) => {
    const value = validStatus()
    mutate(value)
    expect(() => parseStatus(value)).toThrow()
  })
})

describe('Linux process owner held helper authority', () => {
  it('rejects an oversized helper before reading it', async () => {
    await expect(holdLinuxProcessOwnerArtifact({
      path: process.execPath,
      byteLength: (512 * 1024 * 1024) + 1,
      sha256: '0'.repeat(64),
    }, 'oversized helper')).rejects.toThrow(/byte length/u)
  })

  it.skipIf(process.platform !== 'linux')('detects a named-path replacement while its inode is held', async () => {
    const root = await mkdtemp(join(tmpdir(), 'windshare-held-owner-'))
    const helperPath = join(root, 'owner')
    const displacedPath = join(root, 'owner.displaced')
    const trusted = Buffer.from('trusted helper bytes', 'utf8')
    try {
      await writeFile(helperPath, trusted, { mode: 0o700 })
      const held = await holdLinuxProcessOwnerArtifact({
        path: helperPath,
        byteLength: trusted.byteLength,
        sha256: createHash('sha256').update(trusted).digest('hex'),
      }, 'held helper')
      try {
        await rename(helperPath, displacedPath)
        await writeFile(helperPath, 'replacement', { mode: 0o700 })
        await expect(held.assertLive()).rejects.toThrow(/changed|exact bounded/u)
      } finally {
        await held.close()
      }
    } finally {
      trusted.fill(0)
      await rm(root, { recursive: true, force: true })
    }
  })
})

interface StatusFixture {
  schemaVersion: typeof LINUX_PROCESS_OWNER_STATUS_SCHEMA_VERSION
  operationId: string
  processEvidence: Record<string, unknown>
  inputEvidence: { outcome: string; failureCode: string; failureMessage: string }
  timedOut: boolean
  launched: boolean
  treeEmpty: boolean
  ownershipEvidence: {
    ownerPid: number
    rootPid: number | null
    rootStartTimeTicks: string
    inventoryScans: number
    maximumObservedDescendants: number
    quietInventoryCount: number
    controlOutcome: string
    cleanupOutcome: string
    failureCode: string
    failureMessage: string
  }
}

function validStatus(): StatusFixture {
  return {
    schemaVersion: LINUX_PROCESS_OWNER_STATUS_SCHEMA_VERSION,
    operationId: 'status-contract',
    processEvidence: { terminal: 'exited', exitCode: 0 },
    inputEvidence: { outcome: 'delivered', failureCode: '', failureMessage: '' },
    timedOut: false,
    launched: true,
    treeEmpty: true,
    ownershipEvidence: {
      ownerPid: 10,
      rootPid: 11,
      rootStartTimeTicks: '12',
      inventoryScans: 2,
      maximumObservedDescendants: 0,
      quietInventoryCount: 2,
      controlOutcome: 'target-terminal',
      cleanupOutcome: 'completed',
      failureCode: '',
      failureMessage: '',
    },
  }
}

function parseStatus(value: StatusFixture) {
  return parseLinuxProcessOwnerStatus(JSON.stringify(value), value.operationId)
}
