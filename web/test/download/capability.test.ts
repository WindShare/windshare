import { describe, expect, expectTypeOf, it } from 'vitest'

import {
  DownloadError,
  createDownloadSink,
  type FileSystemDownloadSink,
  type SingleFileDownloadSink,
  type ZipDownloadSink,
} from '../../src/download'
import {
  file,
  fixtureContext,
  recordingOutput,
} from './fixtures'

describe('download capability selection', () => {
  it('derives ordering from the target instead of a composition flag', async () => {
    const singleOutput = recordingOutput()
    const zipOutput = recordingOutput()
    const single = createDownloadSink(
      fixtureContext([file('empty.bin', 0)], new Map()),
      { kind: 'single-file-stream', output: singleOutput.stream },
    )
    const zip = createDownloadSink(
      fixtureContext([], new Map()),
      { kind: 'zip-stream', output: zipOutput.stream },
    )
    const filesystem = createDownloadSink(
      fixtureContext([], new Map()),
      {
        kind: 'file-system',
        root: {} as FileSystemDirectoryHandle,
      },
    )

    expectTypeOf(single).toEqualTypeOf<SingleFileDownloadSink>()
    expectTypeOf(zip).toEqualTypeOf<ZipDownloadSink>()
    expectTypeOf(filesystem).toEqualTypeOf<FileSystemDownloadSink>()
    expect(single.deliveryOrder).toBe('ascending')
    expect(zip.deliveryOrder).toBe('ascending')
    expect(filesystem.deliveryOrder).toBe('any')

    await single.abort(new Error('test cleanup'))
    await zip.abort(new Error('test cleanup'))
    await filesystem.abort(new Error('test cleanup'))
  })

  it('rejects an unknown runtime capability instead of returning no sink', () => {
    const context = fixtureContext([], new Map())

    expect(() => createDownloadSink(context, { kind: 'memory' } as never)).toThrowError(
      expect.objectContaining<Partial<DownloadError>>({ code: 'unsupported-target' }),
    )
  })
})
