import { createHash } from 'node:crypto'
import { mkdtemp, readFile, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { describe, expect, it } from 'vitest'

import {
  assertStableWindowsE2ELeaseAt,
  assertStableWindowsE2ELeaseProof,
  stableWindowsE2EDirectory,
} from '../../e2e/fixtures/windows-stable-runner'

const LEASE_TOKEN = '0123456789abcdef0123456789abcdef'
const WRONG_LEASE_TOKEN = 'fedcba9876543210fedcba9876543210'
const WINDOWS_CONTRACT = 'stable-harness-v3'

function sha256(data: Uint8Array): string {
  return createHash('sha256').update(data).digest('hex')
}

describe('Windows real-stack executable identity', () => {
  it('maps every authorized run to the same fixed E2E directory', () => {
    const first = stableWindowsE2EDirectory('win32', WINDOWS_CONTRACT, LEASE_TOKEN)
    const second = stableWindowsE2EDirectory('win32', WINDOWS_CONTRACT, LEASE_TOKEN)
    expect(first).toBe(second)
    expect(first).toMatch(
      new RegExp(
        `${join('tmp', 'd5-harness', 'e2e-bin').replaceAll('\\', '\\\\')}$`,
        'u',
      ),
    )
  })

  it('fails before a Windows random-path build can start', () => {
    expect(() => stableWindowsE2EDirectory('win32', undefined, undefined)).toThrow(
      'scripts/d5-windows-performance.ps1 -Mode BrowserTests',
    )
    expect(() => stableWindowsE2EDirectory('win32', WINDOWS_CONTRACT, undefined)).toThrow()
    expect(() => stableWindowsE2EDirectory('win32', 'stable-harness-v2', LEASE_TOKEN)).toThrow()
  })

  it('preserves non-Windows temporary-worker behavior', () => {
    expect(stableWindowsE2EDirectory('linux', undefined, undefined)).toBeUndefined()
  })

  it('rejects a retained token after its wrapper lease has died', async () => {
    const directory = await mkdtemp(join(tmpdir(), 'windshare-d5-stale-lease-'))
    const lockPath = join(directory, '.owner.lock')
    try {
      await writeFile(lockPath, JSON.stringify({
        contract: WINDOWS_CONTRACT,
        ownerPid: process.pid,
        acquiredAt: new Date().toISOString(),
        tokenSha256: sha256(Buffer.from(LEASE_TOKEN, 'utf8')),
      }))
      await expect(
        assertStableWindowsE2ELeaseAt(lockPath, WINDOWS_CONTRACT, LEASE_TOKEN),
      ).rejects.toThrow('not held by the auditing runner')
      expect(await readFile(lockPath, 'utf8')).not.toBe('')
    } finally {
      await rm(directory, { recursive: true, force: true })
    }
  })

  it('cannot compose owner A identity with successor B liveness', async () => {
    const successorRecord = JSON.stringify({
      contract: WINDOWS_CONTRACT,
      ownerPid: process.pid + 1,
      acquiredAt: new Date().toISOString(),
      tokenSha256: sha256(Buffer.from(WRONG_LEASE_TOKEN, 'utf8')),
    })
    const order: string[] = []
    await expect(assertStableWindowsE2ELeaseProof(
      WINDOWS_CONTRACT,
      LEASE_TOKEN,
      async () => {
        order.push('write-denied-by-owner-a')
      },
      async () => {
        order.push('read-successor-b')
        return successorRecord
      },
    )).rejects.toThrow('does not own this lease record')
    expect(order).toEqual(['write-denied-by-owner-a', 'read-successor-b'])
  })
})
