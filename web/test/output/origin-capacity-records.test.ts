import { describe, expect, it } from 'vitest'
import { workspaceCapacityAccount, workspaceObjectCapacity, stagingFileCapacity } from '../../src/output/origin-private/capacity/records'
import { OriginCapacityDataError } from '../../src/output/origin-private/capacity/errors'

const account = {
  id: 'operation', operationId: 'operation', token: 'lease', budgetDigest: 'digest',
  peakOwnedBytes: 339260n, expiresAtMilliseconds: 1000, occupiedBytes: 0n,
  outstandingGrowthBytes: 0n, metadataHeadroomBytes: 32n,
}
const object = {
  id: 'operation:object', operationId: 'operation', objectId: 'object', token: 'lease',
  occupiedBytes: 12n, reservations: [{ reservationId: 'write', targetLength: 20n, metadataHeadroom: 2n }],
}
const staging = {
  id: 'staged', operationId: 'operation', fileId: 'file', token: 'lease', phase: 'receiving',
  exactSize: 100n, verifiedStagedBytes: 20n, headroomBytes: 2n,
}

describe('origin capacity persistence boundary', () => {
  it('accepts current workspace, object, and staging representations without coercion', () => {
    expect(workspaceCapacityAccount(account)).toBe(account)
    expect(workspaceObjectCapacity(object)).toBe(object)
    expect(stagingFileCapacity(staging)).toBe(staging)
    expect(workspaceCapacityAccount({ ...account, token: '', expiresAtMilliseconds: 0 })).toBeDefined()
  })

  it.each([undefined, null, NaN, 0, '0', -1n, 0x1_0000_0000_0000_0000n])(
    'rejects invalid capacity %s before accounting and identifies the persisted field',
    value => {
      for (const field of ['peakOwnedBytes', 'occupiedBytes', 'outstandingGrowthBytes', 'metadataHeadroomBytes']) {
        expect(() => workspaceCapacityAccount({ ...account, [field]: value }))
          .toThrow(`store=workspace-budget-claims id=operation field=${field}`)
      }
      expect(() => workspaceObjectCapacity({ ...object, occupiedBytes: value })).toThrow(OriginCapacityDataError)
      expect(() => stagingFileCapacity({ ...staging, exactSize: value })).toThrow(OriginCapacityDataError)
    },
  )

  it('rejects legacy rows without manufacturing NaN or treating them as zero usage', () => {
    const legacy = { id: 'old', operationId: 'old', token: '', budgetDigest: 'digest',
      peakOwnedBytes: 339260n, expiresAtMilliseconds: 0 }
    expect(() => workspaceCapacityAccount(legacy)).toThrow('field=occupiedBytes')
    expect(legacy).not.toHaveProperty('occupiedBytes')
  })

  it.each([null, [], 42, { id: '' }])('rejects a malformed record %s', value => {
    expect(() => workspaceCapacityAccount(value)).toThrow(OriginCapacityDataError)
  })

  it('rejects broken ownership, expiry, reservation, and staging constraints', () => {
    expect(() => workspaceCapacityAccount({ ...account, operationId: 'other' })).toThrow('operationId')
    expect(() => workspaceCapacityAccount({ ...account, expiresAtMilliseconds: NaN })).toThrow('expiresAtMilliseconds')
    expect(() => workspaceObjectCapacity({ ...object, id: 'other' })).toThrow('field=id')
    expect(() => workspaceObjectCapacity({ ...object, reservations: [null] })).toThrow('reservations[0]')
    expect(() => workspaceObjectCapacity({ ...object, reservations: [...object.reservations, ...object.reservations] }))
      .toThrow('duplicate reservationId')
    expect(() => workspaceObjectCapacity({ ...object, reservations: [{ reservationId: 'write', targetLength: NaN }] }))
      .toThrow('id=operation:object field=targetLength')
    expect(() => stagingFileCapacity({ ...staging, verifiedStagedBytes: 101n })).toThrow('exceeds exactSize')
    expect(() => stagingFileCapacity({ ...staging, phase: 'unknown' })).toThrow('field=phase')
  })
})
