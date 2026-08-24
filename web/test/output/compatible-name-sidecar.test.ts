import { describe, expect, it } from 'vitest'
import {
  COMPATIBLE_NAME_SIDECAR_FORMAT_VERSION,
  CompatibleNameSidecarError,
  compatibleNameSidecarPlacement,
  decodeCompatibleNameSidecar,
  encodeCompatibleNameSidecarFooter,
  encodeCompatibleNameSidecarHeader,
  encodeCompatibleNameSidecarMapping,
} from '../../src/output/file-system-access/compatible-name/sidecar-codec'

const TEXT_ENCODER = new TextEncoder()
const BASE_HEADER = encodeCompatibleNameSidecarHeader({
  operationId: 'receive-operation-17',
  placement: 'inside',
})

describe('compatible-name restoration sidecar codec', () => {
  it('writes and reads the exact canonical H/M/F grammar', () => {
    const first = encodeCompatibleNameSidecarMapping({
      ordinal: 1,
      entryKind: 'directory',
      logicalPath: ['资料', '原名'],
      physicalComponent: 'readable.windshare-abc234',
    })
    const second = encodeCompatibleNameSidecarMapping({
      ordinal: 2,
      entryKind: 'file',
      logicalPath: ['资料', '原名', 'résumé.txt'],
      physicalComponent: 'resume.windshare-abc234',
    })
    const footer = encodeCompatibleNameSidecarFooter({ committedCount: 2, state: 'completed' })
    const encoded = bytes(BASE_HEADER + first + second + footer)

    expect(BASE_HEADER).toBe(
      `H\t${COMPATIBLE_NAME_SIDECAR_FORMAT_VERSION}\t${base64('receive-operation-17')}\tinside\n`,
    )
    expect(first).toBe(
      `M\t1\tdirectory\t${base64('资料/原名')}\t${base64('readable.windshare-abc234')}\n`,
    )
    expect(footer).toBe('F\t2\tcompleted\n')

    const checkpoint = decodeCompatibleNameSidecar(encoded)
    expect(checkpoint).toMatchObject({
      header: {
        formatVersion: COMPATIBLE_NAME_SIDECAR_FORMAT_VERSION,
        operationId: 'receive-operation-17',
        placement: 'inside',
      },
      mappings: [
        {
          ordinal: 1,
          entryKind: 'directory',
          logicalPath: ['资料', '原名'],
          physicalComponent: 'readable.windshare-abc234',
        },
        {
          ordinal: 2,
          entryKind: 'file',
          logicalPath: ['资料', '原名', 'résumé.txt'],
          physicalComponent: 'resume.windshare-abc234',
        },
      ],
      footer: { committedCount: 2, state: 'completed' },
      checkpointByteLength: encoded.byteLength,
      trailingByteLength: 0,
    })
    expect(compatibleNameSidecarPlacement('inside-logical-root')).toBe('inside')
    expect(compatibleNameSidecarPlacement('beside-mapped-root')).toBe('beside')
  })

  it.each([
    {
      label: 'non-canonical Base64',
      sidecar: `H\t${COMPATIBLE_NAME_SIDECAR_FORMAT_VERSION}\tb3A\tinside\nF\t0\tactive\n`,
    },
    {
      label: 'non-UTF-8 Base64',
      sidecar: `H\t${COMPATIBLE_NAME_SIDECAR_FORMAT_VERSION}\t/w==\tinside\nF\t0\tactive\n`,
    },
    {
      label: 'unsupported version',
      sidecar: BASE_HEADER.replace(
        COMPATIBLE_NAME_SIDECAR_FORMAT_VERSION,
        'windshare-name-restoration/v1',
      ) +
        'F\t0\tactive\n',
    },
    {
      label: 'unsupported placement',
      sidecar: BASE_HEADER.replace('\tinside\n', '\tinside-logical-root\n') + 'F\t0\tactive\n',
    },
    {
      label: 'non-contiguous ordinal',
      sidecar: BASE_HEADER + rawMapping(2, 'file', 'alpha.txt', 'alpha.windshare-abc234') +
        'F\t1\tactive\n',
    },
    {
      label: 'non-canonical integer',
      sidecar: BASE_HEADER + rawMapping('01', 'file', 'alpha.txt', 'alpha.windshare-abc234') +
        'F\t1\tactive\n',
    },
    {
      label: 'duplicate logical path',
      sidecar: BASE_HEADER + rawMapping(1, 'file', 'alpha.txt', 'alpha.windshare-abc234') +
        rawMapping(2, 'file', 'ALPHA.TXT', 'other.windshare-abc234') + 'F\t2\tactive\n',
    },
    {
      label: 'escaping path',
      sidecar: BASE_HEADER + rawMapping(1, 'file', '../alpha.txt', 'alpha.windshare-abc234') +
        'F\t1\tactive\n',
    },
    {
      label: 'absolute path',
      sidecar: BASE_HEADER + rawMapping(1, 'file', '/alpha.txt', 'alpha.windshare-abc234') +
        'F\t1\tactive\n',
    },
    {
      label: 'backslash path',
      sidecar: BASE_HEADER + rawMapping(1, 'file', 'folder\\alpha.txt', 'alpha.windshare-abc234') +
        'F\t1\tactive\n',
    },
    {
      label: 'unsupported kind',
      sidecar: BASE_HEADER + rawMapping(1, 'link', 'alpha.txt', 'alpha.windshare-abc234') +
        'F\t1\tactive\n',
    },
    {
      label: 'same logical and physical leaf',
      sidecar: BASE_HEADER + rawMapping(1, 'file', 'Alpha.txt', 'alpha.TXT') +
        'F\t1\tactive\n',
    },
    {
      label: 'mapped file ancestor',
      sidecar: BASE_HEADER + rawMapping(1, 'file', 'folder', 'folder.windshare-abc234') +
        rawMapping(2, 'file', 'folder/alpha.txt', 'alpha.windshare-abc234') +
        'F\t2\tactive\n',
    },
    {
      label: 'footer count mismatch',
      sidecar: BASE_HEADER + rawMapping(1, 'file', 'alpha.txt', 'alpha.windshare-abc234') +
        'F\t0\tactive\n',
    },
  ])('rejects $label inside the selected checkpoint', ({ sidecar }) => {
    expect(() => decodeCompatibleNameSidecar(bytes(sidecar)))
      .toThrow(CompatibleNameSidecarError)
  })

  it('selects the last structurally valid footer and ignores an uncheckpointed tail', () => {
    const firstBatch = rawMapping(1, 'directory', 'root', 'root.windshare-abc234') +
      'F\t1\tactive\n'
    const secondBatch = rawMapping(2, 'file', 'root/file.txt', 'file.windshare-abc234') +
      'F\t2\tstopped\n'
    const checkpointText = BASE_HEADER + firstBatch + secondBatch
    const tornTail = rawMapping(3, 'file', 'root/later.txt', 'later.windshare-abc234')
      .slice(0, -4)
    const checkpoint = decodeCompatibleNameSidecar(bytes(checkpointText + tornTail))

    expect(checkpoint.footer).toEqual({ committedCount: 2, state: 'stopped' })
    expect(checkpoint.mappings.map(mapping => mapping.ordinal)).toEqual([1, 2])
    expect(checkpoint.checkpointByteLength).toBe(bytes(checkpointText).byteLength)
    expect(checkpoint.trailingByteLength).toBe(bytes(tornTail).byteLength)
  })

  it('preserves an earlier checkpoint when a later complete batch is malformed', () => {
    const closedPrefix = BASE_HEADER +
      rawMapping(1, 'file', 'first.txt', 'first.windshare-abc234') + 'F\t1\tactive\n'
    const malformedTail = rawMapping(2, 'file', 'second.txt', 'second.windshare-abc234') +
      'F\t1\tcompleted\n'
    const checkpoint = decodeCompatibleNameSidecar(bytes(closedPrefix + malformedTail))

    expect(checkpoint.footer).toEqual({ committedCount: 1, state: 'active' })
    expect(checkpoint.checkpointByteLength).toBe(bytes(closedPrefix).byteLength)
    expect(checkpoint.trailingByteLength).toBe(bytes(malformedTail).byteLength)
  })

  it('requires at least one newline-terminated valid footer', () => {
    expect(() => decodeCompatibleNameSidecar(bytes(BASE_HEADER + 'F\t0\tactive')))
      .toThrow('no structurally valid complete checkpoint')
    expect(() => decodeCompatibleNameSidecar(Uint8Array.of(0xff)))
      .toThrow('not strict UTF-8')
  })
})

function rawMapping(
  ordinal: number | string,
  kind: string,
  logicalPath: string,
  physicalComponent: string,
): string {
  return `M\t${ordinal}\t${kind}\t${base64(logicalPath)}\t${base64(physicalComponent)}\n`
}

function base64(value: string): string {
  return Buffer.from(value, 'utf8').toString('base64')
}

function bytes(value: string): Uint8Array<ArrayBuffer> {
  return TEXT_ENCODER.encode(value)
}
