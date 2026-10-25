import { describe, expect, it, vi } from 'vitest'
import { encodeBase64Url } from '../../src/crypto/bytes'
import { isIncompatibleNameRecord } from '../../src/output/browser/indexeddb/incompatible-name-records'
import { COMPATIBLE_NAME_LEDGER_FORMAT_VERSION } from '../../src/output/file-system-access/compatible-name/model'
import { receiveOperationResumeDescriptor } from '../../src/output/resume/descriptor'
import { AuthorityOwnedReceiveOperationMutationPort } from '../../src/output/resume/reopen/cleanup'

describe('incompatible local name records', () => {
  it('rejects every unknown format without interpreting its filesystem claims', () => {
    const row = { operationId: 'operation', formatVersion: 'compatible-name-ledger/v2', pair: 'opaque' }
    expect(isIncompatibleNameRecord(row, 'operation')).toBe(true)
    expect(isIncompatibleNameRecord(row, 'other')).toBe(false)
    expect(isIncompatibleNameRecord({ ...row, formatVersion: COMPATIBLE_NAME_LEDGER_FORMAT_VERSION }, 'operation')).toBe(false)
    expect(isIncompatibleNameRecord({ ...row, formatVersion: 'unknown' }, 'operation')).toBe(true)
    expect(isIncompatibleNameRecord(null, 'operation')).toBe(false)
  })

  it('routes forgetting directly to metadata authority without reopening physical output', async () => {
    const reopen = vi.fn(async () => { throw new Error('physical reopen forbidden') })
    const cleanup = vi.fn(async () => { throw new Error('physical cleanup forbidden') })
    const forgetIncompatible = vi.fn(async () => ({ kind: 'record-forgotten' as const }))
    const mutation = new AuthorityOwnedReceiveOperationMutationPort({
      reopen: { reopen }, cleanup: { cleanup }, forgetIncompatible,
    })
    const descriptor = {
      ...receiveOperationResumeDescriptor({
        operationId: identity(16, 1),
        receiveIntentDigest: identity(32, 2),
        generation: 1n,
        kind: 'receiving',
        activeLeaseId: identity(16, 3),
      })!,
      continuation: 'cleanup-incompatible' as const,
    }
    await expect(mutation.resume(descriptor)).rejects.toThrow('no physical output authority')
    await expect(mutation.cleanup(descriptor)).rejects.toThrow('no physical output authority')
    await expect(mutation.catchUp(descriptor)).rejects.toThrow('no physical output authority')
    await expect(mutation.discard(descriptor)).resolves.toEqual({ kind: 'record-forgotten' })
    expect(forgetIncompatible).toHaveBeenCalledWith(descriptor)
    expect(reopen).not.toHaveBeenCalled()
    expect(cleanup).not.toHaveBeenCalled()
  })
})

function identity(length: number, byte: number): string {
  return encodeBase64Url(new Uint8Array(length).fill(byte))
}
