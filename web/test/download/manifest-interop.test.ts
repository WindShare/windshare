import { describe, expect, it } from 'vitest'

import {
  PackedLayout,
  createTransferPlan,
  decodeCanonicalManifest,
  validateCanonicalPath,
} from '../../src/manifest'
import { createSingleFileDownloadSink } from '../../src/download'
import { createChunkIndex } from '../../src/contracts'
import { encodedManifest, wireEntry } from '../manifest/fixtures'
import { concat, recordingOutput } from './fixtures'

describe('authenticated manifest and download sink interoperability', () => {
  it('uses the real C1 plan and layout while omitting a boundary sibling', async () => {
    const manifest = decodeCanonicalManifest(
      encodedManifest([
        wireEntry('skipped.bin', 1_023),
        wireEntry('selected.bin', 2),
      ]),
    )
    const layout = new PackedLayout(manifest)
    const plan = await createTransferPlan(manifest, ['selected.bin'])
    const output = recordingOutput()
    const sink = createSingleFileDownloadSink(
      { plan, layout, validatePath: validateCanonicalPath },
      output.stream,
    )
    const first = new Uint8Array(1_024)
    first.fill(90)
    first[1_023] = 7

    await sink.writeBlock(createChunkIndex(0), first)
    await sink.writeBlock(createChunkIndex(1), Uint8Array.of(8))
    await sink.finalize()

    expect(concat(output.chunks)).toEqual(Uint8Array.of(7, 8))
  })
})
