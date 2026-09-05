import { describe, expect, it } from 'vitest'

import { encodeBase64Url } from '../../src/crypto/bytes'
import {
  COMPATIBLE_NAME_LEDGER_FORMAT_VERSION,
  COMPATIBLE_NAME_PENDING_OUTCOME_FORMAT_VERSION,
  MAX_COMPATIBLE_NAME_REPAIR_SUMMARY_PATHS,
  compatibleNameMappingId,
  compatibleNameMappingV1,
  compatibleNameOperationBootstrapV1,
  compatibleNameOperationHeaderV1,
  compatibleNamePendingTerminalOutcomeV1,
  compatibleNameRepairSummary,
  type CompatibleNameOperationHeaderV1,
  type CompatibleNamePendingTerminalOutcomeV1,
} from '../../src/output/file-system-access/compatible-name/model'
import { PhysicalPathResolver } from '../../src/output/file-system-access/compatible-name/resolver'
import {
  readMapping,
  readOperationRow,
  storedOperationRow,
} from '../../src/output/browser/indexeddb-compatible-name-records'
import { initialReceiveLifecycleState } from '../../src/output/workspace/state'

describe('compatible-name ledger model', () => {
  it('keys exact canonical path boundaries and entry kind without joined-path ambiguity', () => {
    const operationId = identity(16, 1)
    expect(compatibleNameMappingId(operationId, ['ab', 'c'], 'file')).not.toBe(
      compatibleNameMappingId(operationId, ['a', 'bc'], 'file'),
    )
    expect(compatibleNameMappingId(operationId, ['ab', 'c'], 'file')).not.toBe(
      compatibleNameMappingId(operationId, ['ab', 'c'], 'directory'),
    )

    const mapping = selectedMapping(operationId)
    expect(mapping).toMatchObject({
      formatVersion: COMPATIBLE_NAME_LEDGER_FORMAT_VERSION,
      logicalPath: ['root', 'rejected.cfg'],
      entryKind: 'file',
      ownershipState: 'selected',
      commitState: 'uncommitted',
    })
    expect(Object.isFrozen(mapping.logicalPath)).toBe(true)
  })

  it('rejects legacy durable ledger, mapping, and pending-outcome versions', () => {
    const operationId = identity(16, 1)
    expect(() => readOperationRow({
      ...storedOperationRow(operationHeader(operationId), 1),
      formatVersion: 'compatible-name-ledger/v1',
    })).toThrow('version')
    expect(() => readMapping({
      ...selectedMapping(operationId),
      formatVersion: 'compatible-name-ledger/v1',
    })).toThrow('version')
    expect(() => compatibleNamePendingTerminalOutcomeV1({
      formatVersion: 'compatible-name-pending-outcome/v1',
    } as unknown as CompatibleNamePendingTerminalOutcomeV1)).toThrow('version')
  })

  it('admits only a pristine pair-first bootstrap using the operation token', () => {
    const operationId = identity(16, 1)
    const header = operationHeader(operationId)
    expect(compatibleNameOperationBootstrapV1({
      header,
      initialMapping: selectedMapping(operationId),
    })).toEqual({ header, initialMapping: selectedMapping(operationId) })

    expect(() => compatibleNameOperationBootstrapV1({
      header,
      initialMapping: selectedMapping(operationId, {
        logicalPath: ['rejected.cfg'],
        physicalComponent: header.pair.script.physicalName,
      }),
    })).toThrow('restoration pair claim')
    expect(() => compatibleNameOperationBootstrapV1({
      header,
      initialMapping: selectedMapping(operationId, { token: 'bbbbbb' }),
    })).toThrow('operation token')
  })

  it('bounds repair samples and never accepts a footer beyond durable commits', () => {
    const paths = Array.from(
      { length: MAX_COMPATIBLE_NAME_REPAIR_SUMMARY_PATHS + 1 },
      (_, index) => [`file-${index}.bin`],
    )
    expect(() => repairSummary(paths, paths.length)).toThrow('sample exceeds')
    expect(() => repairSummary([['file.bin']], 1, 2)).toThrow('footer exceeds')
  })

  it('rejects independently named pair files and missing pair tokens', () => {
    const header = operationHeader(identity(16, 1))
    expect(() => compatibleNameOperationHeaderV1({
      ...header, pair: { ...header.pair, token: 'bbbbbb' },
    })).toThrow('canonical token')
    expect(() => compatibleNameOperationHeaderV1({
      ...header, pair: {
        ...header.pair,
        sidecar: { ...header.pair.sidecar, physicalName: 'restore.windshare-bbbbbb.data' },
      },
    })).toThrow('canonical token')
    expect(() => compatibleNameOperationHeaderV1({
      ...header, pair: { ...header.pair, token: undefined as unknown as string },
    })).toThrow('pair token')
  })

  it('keeps sidecar synchronization separate from unfinished terminal settlement', () => {
    const current = repairSummary([['file.bin']], 1)
    expect(compatibleNameRepairSummary({
      ...current, terminalSettlement: 'pending',
    })).toMatchObject({ sidecarSync: 'current', terminalSettlement: 'pending' })
    expect(() => compatibleNameRepairSummary({
      ...current, committedCount: 2,
    })).toThrow('verified complete ledger prefix')
    expect(() => compatibleNameRepairSummary({
      ...current, terminalSettlement: 'complete',
    })).toThrow('terminal footer')
  })

  it('requires pair ownership before a header can leave the prepared state', () => {
    const operationId = identity(16, 1)
    expect(() => compatibleNameOperationHeaderV1({
      ...operationHeader(operationId),
      activationState: 'pair-ready',
    })).toThrow('pair ownership')
  })

  it('keys mapping and pair claims by their physical parent namespace', () => {
    const operationId = identity(16, 1)
    const prepared = operationHeader(operationId)
    const header = compatibleNameOperationHeaderV1({
      ...prepared,
      pair: {
      token: 'aaaaaa',
        script: { ...prepared.pair.script, ownershipState: 'owned' },
        sidecar: { ...prepared.pair.sidecar, ownershipState: 'owned' },
      },
      activationState: 'active',
      repairSummary: repairSummary([], 0),
    })
    const left = selectedMapping(operationId, {
      logicalPath: ['left', 'blocked.txt'],
      physicalComponent: 'blocked.windshare-aaaaaa',
    })
    const right = selectedMapping(operationId, {
      logicalPath: ['right', 'blocked.txt'],
      physicalComponent: 'blocked.windshare-aaaaaa',
    })
    const resolver = new PhysicalPathResolver({ header, mappings: [left, right] })

    expect(resolver.hasClaimedPhysicalComponent(['left'], left.physicalComponent)).toBe(true)
    expect(resolver.hasClaimedPhysicalComponent(['unrelated'], left.physicalComponent)).toBe(false)
    expect(resolver.physicalChild(['unrelated'], left.physicalComponent, 'file')).toEqual({
      kind: 'logical',
      logicalComponent: left.physicalComponent,
    })
    expect(resolver.physicalChild([], header.pair.script.physicalName, 'file')).toEqual({
      kind: 'restoration-pair',
    })
    expect(resolver.physicalChild(
      ['unrelated'],
      header.pair.script.physicalName,
      'file',
    )).toEqual({ kind: 'logical', logicalComponent: header.pair.script.physicalName })

    expect(() => resolver.adoptMapping(selectedMapping(operationId, {
      logicalPath: ['left', 'other.txt'],
      physicalComponent: left.physicalComponent,
    }))).toThrow('conflicting sibling claims')
  })

  it('does not admit an active lifecycle as a pending terminal outcome', () => {
    const operationId = identity(16, 1)
    expect(() => compatibleNamePendingTerminalOutcomeV1({
      formatVersion: COMPATIBLE_NAME_PENDING_OUTCOME_FORMAT_VERSION,
      footerState: 'failed',
      ordinaryLifecycle: initialReceiveLifecycleState({
        operationId,
        receiveIntentDigest: identity(32, 2),
      }) as unknown as CompatibleNamePendingTerminalOutcomeV1['ordinaryLifecycle'],
      terminalReceipt: undefined as unknown as CompatibleNamePendingTerminalOutcomeV1['terminalReceipt'],
    })).toThrow('not an ordinary terminal lifecycle')
  })
})

function operationHeader(operationId: string): CompatibleNameOperationHeaderV1 {
  return compatibleNameOperationHeaderV1({
    operationId,
    primaryToken: 'aaaaaa',
    authorityRef: identity(32, 2),
    root: { logicalName: 'root', physicalName: 'root' },
    templateId: 'windows-powershell-v1',
    pairPlacement: 'inside-logical-root',
    pair: {
      token: 'aaaaaa',
      script: {
        physicalName: 'restore.windshare-aaaaaa.ps1',
        handleId: 'repair-script-handle',
        ownedObjectId: identity(32, 3),
        ownershipState: 'claimed',
      },
      sidecar: {
        physicalName: 'restore.windshare-aaaaaa.data',
        handleId: 'repair-sidecar-handle',
        ownedObjectId: identity(32, 4),
        ownershipState: 'claimed',
      },
    },
    activationState: 'prepared',
  })
}

function selectedMapping(
  operationId: string,
  overrides: Partial<Parameters<typeof compatibleNameMappingV1>[0]> = {},
) {
  return compatibleNameMappingV1({
    operationId,
    logicalPath: ['root', 'rejected.cfg'],
    entryKind: 'file',
    physicalComponent: 'rejected.windshare-aaaaaa',
    attempt: 0,
    token: 'aaaaaa',
    ownershipState: 'selected',
    commitState: 'uncommitted',
    ...overrides,
  })
}

function repairSummary(
  logicalPathSample: readonly (readonly string[])[],
  committedCount: number,
  footerCount = committedCount,
) {
  return compatibleNameRepairSummary({
    committedCount,
    logicalPathSample,
    pairDisplayNames: {
      script: 'restore.windshare-aaaaaa.ps1',
      sidecar: 'restore.windshare-aaaaaa.data',
    },
    placement: 'inside-logical-root',
    latestObservedFooter: { committedCount: footerCount, state: 'active' },
    sidecarSync: footerCount !== committedCount ? 'pending' : 'current',
    terminalSettlement: 'none',
  })
}

function identity(width: number, fill: number): string {
  return encodeBase64Url(new Uint8Array(width).fill(fill))
}
