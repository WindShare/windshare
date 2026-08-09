import { describe, expect, it } from 'vitest'

import {
  createCompleteDirectoryResultRoot,
  type ReceiveIntent,
} from '../../src/transfer/intent'
import {
  artifactDirectoryPath,
  artifactFilePath,
  directoryIsResultRoot,
} from '../../src/transfer/job/artifact-path'
import { identityText } from './v2-job-fixture'

describe('artifact path projection', () => {
  it('maps an anchored directory itself to the named result root while retaining ancestors as references', () => {
    const layout = createCompleteDirectoryResultRoot(identityText(8), 'parent/photos')
    const intent = {
      artifact: { kind: 'zip-archive', layout },
    } as unknown as ReceiveIntent

    expect(artifactDirectoryPath(intent, ['parent'])).toEqual([])
    expect(artifactDirectoryPath(intent, ['parent', 'photos'])).toEqual([layout.name])
    expect(artifactDirectoryPath(intent, ['parent', 'photos', 'nested']))
      .toEqual([layout.name, 'nested'])
    expect(artifactFilePath(intent, ['parent', 'photos', 'image.jpg']))
      .toEqual([layout.name, 'image.jpg'])
    expect(directoryIsResultRoot(intent, ['parent', 'photos'])).toBe(true)
  })
})
