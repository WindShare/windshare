import { describe, expect, it } from 'vitest'

import { encodeBase64Url } from '../../src/crypto/bytes'
import { FaultScope, OutputFaultCode, outputFault } from '../../src/transfer/fault'
import {
  DirectoryAdmissionBindingError,
  DirectorySettlementKind,
  createDirectoryAdmission,
  finalizedDirectorySettlement,
  isolatedDirectorySettlement,
  sameDirectoryAdmission,
  sameDirectoryAdmissionToken,
  validateDirectoryAdmissionBinding,
  validateDirectorySettlement,
  verifyDirectoryAdmissionToken,
  type DirectoryAdmission,
  type DirectoryAdmissionScope,
  type OutputDirectoryAdmission,
} from '../../src/transfer/output-session'

const secret = Uint8Array.from({ length: 32 }, (_, index) => index + 1)
const scope = Object.freeze({
  transferIntentDigest: identity(32, 40),
  syntheticRoot: identity(16, 10),
})

describe('DirectoryAdmission V1 values', () => {
  it('binds the synthetic root and every child to one immutable intent scope', async () => {
    const rootRequest: OutputDirectoryAdmission = {
      directoryId: scope.syntheticRoot,
      generation: identity(16, 20),
      path: [],
    }
    const root = await createDirectoryAdmission(secret, scope, rootRequest)
    const childPath = ['child']
    const childRequest: OutputDirectoryAdmission = {
      directoryId: identity(16, 11),
      generation: identity(16, 21),
      path: childPath,
      parentAdmission: root,
    }
    const child = await createDirectoryAdmission(secret, scope, childRequest)
    expect(validateDirectoryAdmissionBinding(scope, childRequest, child)).toEqual(child)
    expect(await verifyDirectoryAdmissionToken(secret, scope, childRequest, child.token)).toBe(true)
    childPath[0] = 'mutated'

    expect(child.path).toEqual(['child'])
    expect(Object.isFrozen(child)).toBe(true)
    expect(Object.isFrozen(child.path)).toBe(true)

    const foreignScope: DirectoryAdmissionScope = {
      transferIntentDigest: identity(32, 41),
      syntheticRoot: scope.syntheticRoot,
    }
    await expect(createDirectoryAdmission(secret, foreignScope, childRequest))
      .rejects.toThrow(/another transfer intent/u)
    await expect(createDirectoryAdmission(secret, scope, {
      ...rootRequest,
      directoryId: identity(16, 12),
    })).rejects.toThrow(/frozen intent root/u)
  })

  it('compares fixed-size receipt tokens without trusting mutable claim echoes', async () => {
    const request: OutputDirectoryAdmission = {
      directoryId: scope.syntheticRoot,
      generation: identity(16, 22),
      path: [],
    }
    const first = await createDirectoryAdmission(secret, scope, request)
    const retry = await createDirectoryAdmission(Uint8Array.from(secret), scope, request)
    const rebound = Object.freeze({ ...retry, generation: identity(16, 23) })

    expect(sameDirectoryAdmissionToken(first.token, retry.token)).toBe(true)
    expect(sameDirectoryAdmission(first, retry)).toBe(true)
    expect(sameDirectoryAdmission(first, rebound)).toBe(false)
    expect(() => validateDirectoryAdmissionBinding(scope, request, rebound))
      .toThrow(DirectoryAdmissionBindingError)
  })
})

describe('DirectorySettlement values', () => {
  it('retains the exact receipt and only the closed isolated metadata fault', async () => {
    const admission = await rootAdmission()
    const finalized = finalizedDirectorySettlement(admission)
    expect(finalized.kind).toBe(DirectorySettlementKind.Finalized)
    expect(validateDirectorySettlement(admission, finalized)).toEqual(finalized)

    const metadata = outputFault(FaultScope.DirectoryLocal, OutputFaultCode.DirectoryMetadata)
    const isolated = isolatedDirectorySettlement(admission, metadata)
    expect(isolated.kind).toBe(DirectorySettlementKind.IsolatedFailure)
    expect(validateDirectorySettlement(admission, isolated)).toEqual(isolated)
    expect(Object.isFrozen(isolated)).toBe(true)
    if (isolated.kind === DirectorySettlementKind.IsolatedFailure) {
      expect(Object.isFrozen(isolated.fault)).toBe(true)
      expect(isolated.fault).toEqual(metadata)
    }
  })

  it('rejects wider failures and settlements for a foreign receipt', async () => {
    const admission = await rootAdmission()
    expect(() => isolatedDirectorySettlement(
      admission,
      outputFault(FaultScope.OutputPause, OutputFaultCode.DirectoryMetadata),
    )).toThrow(/directory-local metadata/u)

    const foreign = await createDirectoryAdmission(secret, scope, {
      directoryId: scope.syntheticRoot,
      generation: identity(16, 24),
      path: [],
    })
    expect(() => validateDirectorySettlement(
      admission,
      finalizedDirectorySettlement(foreign),
    )).toThrow(/another admission/u)
  })
})

async function rootAdmission(): Promise<DirectoryAdmission> {
  return createDirectoryAdmission(secret, scope, {
    directoryId: scope.syntheticRoot,
    generation: identity(16, 20),
    path: [],
  })
}

function identity(length: number, first: number): string {
  const value = new Uint8Array(length)
  value[0] = first
  return encodeBase64Url(value)
}
