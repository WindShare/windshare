import { describe, expect, it } from 'vitest'
import {
  COMPATIBLE_NAME_COLLISION_RETRY_LIMIT,
  COMPATIBLE_NAME_READABLE_PREFIX_FALLBACK,
  COMPATIBLE_NAME_READABLE_PREFIX_MAX_UTF8_BYTES,
  compatibleNameCandidate,
  compatibleNameReadablePrefix,
  deriveCompatibleNameFallbackToken,
  generateCompatibleNamePrimaryToken,
} from '../../src/output/file-system-access/compatible-name/naming'

const OPERATION_ID = 'AQIDBAUGBwgJCgsMDQ4PEA'
const OTHER_OPERATION_ID = 'EBESExQVFhcYGRobHB0eHw'
const PRIMARY_TOKEN = 'abcdef'
const TEXT_ENCODER = new TextEncoder()

describe('compatible physical-name policy', () => {
  it('encodes one complete 30-bit sample as six lowercase RFC 4648 Base32 characters', () => {
    let calls = 0
    expect(generateCompatibleNamePrimaryToken(() => {
      calls += 1
      return 0
    })).toBe('aaaaaa')
    expect(calls).toBe(1)
    expect(generateCompatibleNamePrimaryToken(() => 0x3fff_ffff)).toBe('777777')
    expect(generateCompatibleNamePrimaryToken(() => 1)).toBe('aaaaab')
    expect(() => generateCompatibleNamePrimaryToken(() => 0x4000_0000)).toThrow(TypeError)
    expect(() => generateCompatibleNamePrimaryToken(() => -1)).toThrow(TypeError)
    expect(() => generateCompatibleNamePrimaryToken(() => 1.5)).toThrow(TypeError)
  })

  it('sanitizes portable punctuation and symbols without erasing readable Unicode text', async () => {
    expect(compatibleNameReadablePrefix('café_日本語—résumé №２.txt')).toBe(
      'café-日本語-résumé-２-txt',
    )
    expect(compatibleNameReadablePrefix('!CON')).toBe('-CON')
    expect(compatibleNameReadablePrefix('COM1!')).toBe('entry-COM1')
    expect(compatibleNameReadablePrefix('💥—…')).toBe(
      COMPATIBLE_NAME_READABLE_PREFIX_FALLBACK,
    )
    await expect(compatibleNameCandidate({
      operationId: OPERATION_ID,
      logicalPath: ['COM1!'],
      entryKind: 'file',
      primaryToken: PRIMARY_TOKEN,
      attempt: 0,
    })).resolves.toMatchObject({
      physicalComponent: `entry-COM1.windshare-${PRIMARY_TOKEN}`,
    })
  })

  it('sanitizes before bounding and never splits a retained Unicode scalar', async () => {
    expect(compatibleNameReadablePrefix(`${'!'.repeat(251)}tail`)).toBe('-tail')
    const logicalComponent = `${'é'.repeat(119)}界`
    const prefix = compatibleNameReadablePrefix(logicalComponent)
    const candidate = await compatibleNameCandidate({
      operationId: OPERATION_ID,
      logicalPath: [logicalComponent],
      entryKind: 'file',
      primaryToken: PRIMARY_TOKEN,
      attempt: 0,
    })

    expect(prefix).toBe('é'.repeat(119))
    expect(TEXT_ENCODER.encode(prefix)).toHaveLength(
      COMPATIBLE_NAME_READABLE_PREFIX_MAX_UTF8_BYTES,
    )
    expect(TEXT_ENCODER.encode(candidate.physicalComponent)).toHaveLength(255)
    expect(candidate.physicalComponent).toBe(`${prefix}.windshare-${PRIMARY_TOKEN}`)
  })

  it('keeps non-portable input outside compatible-name virtualization', () => {
    expect(() => compatibleNameReadablePrefix('nested/name')).toThrow(TypeError)
    expect(() => compatibleNameReadablePrefix('entry.')).toThrow(TypeError)
  })

  it('uses the persisted primary token only for attempt zero', async () => {
    const candidate = await compatibleNameCandidate({
      operationId: OPERATION_ID,
      logicalPath: ['root', 'pyvenv.cfg'],
      entryKind: 'file',
      primaryToken: PRIMARY_TOKEN,
      attempt: 0,
    })

    expect(candidate).toEqual({
      operationId: OPERATION_ID,
      logicalPath: ['root', 'pyvenv.cfg'],
      entryKind: 'file',
      physicalComponent: `pyvenv-cfg.windshare-${PRIMARY_TOKEN}`,
      attempt: 0,
      token: PRIMARY_TOKEN,
    })
  })

  it('pins the versioned fallback derivation and all of its identity fields', async () => {
    const input = {
      operationId: OPERATION_ID,
      logicalPath: ['root', 'café', 'pyvenv.cfg'],
      entryKind: 'file' as const,
      attempt: 1,
    }
    const token = await deriveCompatibleNameFallbackToken(input)

    expect(token).toBe('syqwye')
    await expect(deriveCompatibleNameFallbackToken({
      ...input,
      operationId: OTHER_OPERATION_ID,
    })).resolves.not.toBe(token)
    await expect(deriveCompatibleNameFallbackToken({
      ...input,
      logicalPath: ['other-root', 'café', 'pyvenv.cfg'],
    })).resolves.not.toBe(token)
    await expect(deriveCompatibleNameFallbackToken({
      ...input,
      entryKind: 'directory',
    })).resolves.not.toBe(token)
    await expect(deriveCompatibleNameFallbackToken({
      ...input,
      attempt: 2,
    })).resolves.not.toBe(token)
  })

  it('returns a deeply immutable, repeatable ledger-ready selection', async () => {
    const logicalPath = ['root', 'café', 'pyvenv.cfg']
    const first = await compatibleNameCandidate({
      operationId: OPERATION_ID,
      logicalPath,
      entryKind: 'file',
      primaryToken: PRIMARY_TOKEN,
      attempt: 2,
    })
    logicalPath[0] = 'mutated'
    const repeated = await compatibleNameCandidate({
      operationId: OPERATION_ID,
      logicalPath: ['root', 'café', 'pyvenv.cfg'],
      entryKind: 'file',
      primaryToken: 'zzzzzz',
      attempt: 2,
    })

    expect(first).toEqual(repeated)
    expect(first.logicalPath).toEqual(['root', 'café', 'pyvenv.cfg'])
    expect(Object.isFrozen(first)).toBe(true)
    expect(Object.isFrozen(first.logicalPath)).toBe(true)
    expect(first.token).toMatch(/^[a-z2-7]{6}$/u)
    expect(first.physicalComponent).toBe('pyvenv-cfg.windshare-jvgasy')
  })

  it('admits only the named bounded occupancy-retry range', async () => {
    await expect(compatibleNameCandidate({
      operationId: OPERATION_ID,
      logicalPath: ['root', 'entry'],
      entryKind: 'directory',
      primaryToken: PRIMARY_TOKEN,
      attempt: COMPATIBLE_NAME_COLLISION_RETRY_LIMIT,
    })).resolves.toMatchObject({ attempt: COMPATIBLE_NAME_COLLISION_RETRY_LIMIT })

    await expect(compatibleNameCandidate({
      operationId: OPERATION_ID,
      logicalPath: ['root', 'entry'],
      entryKind: 'directory',
      primaryToken: PRIMARY_TOKEN,
      attempt: COMPATIBLE_NAME_COLLISION_RETRY_LIMIT + 1,
    })).rejects.toThrow(TypeError)
    await expect(deriveCompatibleNameFallbackToken({
      operationId: OPERATION_ID,
      logicalPath: ['root', 'entry'],
      entryKind: 'directory',
      attempt: 0,
    })).rejects.toThrow(TypeError)
  })
})
