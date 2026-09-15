/// <reference types="vite/client" />

import { readFileSync } from 'node:fs'
import { expect, it } from 'vitest'
import rawTemplate from '../../src/output/file-system-access/compatible-name/restoration/windows-v1.ps1?raw'

it('bundles the production restoration script without changing its contents', () => {
  // Windows host contracts execute this asset; the Web boundary must preserve it when bundling.
  const template = readFileSync(new URL(
    '../../src/output/file-system-access/compatible-name/restoration/windows-v1.ps1',
    import.meta.url,
  ), 'utf8')
  expect(rawTemplate).toBe(template)
})
