import { mkdtemp, rm } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

import {
  buildDirectProductBinaries,
  directBinaryPaths,
  DIRECT_PRODUCT_BINARY_DIRECTORY_ENV,
} from './direct-product-binaries'

export default async function prepareDirectProductBinaries(): Promise<() => Promise<void>> {
  const directory = await mkdtemp(join(tmpdir(), 'windshare-browser-binaries-'))
  const binaries = directBinaryPaths(directory)
  const previousDirectory = process.env[DIRECT_PRODUCT_BINARY_DIRECTORY_ENV]
  const removeBinaries = () => rm(directory, { recursive: true, force: true })
  const startedAt = Date.now()
  console.info('Preparing native browser-test binaries: wind and wsrelay')
  try {
    await buildDirectProductBinaries(binaries)
  } catch (error) {
    try {
      await removeBinaries()
    } catch (cleanupError) {
      throw new AggregateError([error, cleanupError], 'Direct product binary preparation cleanup failed', {
        cause: cleanupError,
      })
    }
    throw error
  }

  // Playwright propagates this environment to every worker, including workers
  // restarted after a failure. Publish only the completed pair, and retain it
  // until all scenarios and their native processes have finished teardown.
  process.env[DIRECT_PRODUCT_BINARY_DIRECTORY_ENV] = directory
  console.info(`Native browser-test binaries ready in ${Date.now() - startedAt} ms`)
  return async () => {
    if (previousDirectory === undefined) delete process.env[DIRECT_PRODUCT_BINARY_DIRECTORY_ENV]
    else process.env[DIRECT_PRODUCT_BINARY_DIRECTORY_ENV] = previousDirectory
    await removeBinaries()
  }
}
