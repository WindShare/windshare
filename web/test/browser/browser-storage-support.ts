import { rm } from 'node:fs/promises'

import { test, type Page } from '@playwright/test'

interface OriginPrivateStorageManager extends StorageManager {
  getDirectory(): Promise<FileSystemDirectoryHandle>
}

const PROFILE_REMOVAL_MAX_RETRIES = 5
const PROFILE_REMOVAL_RETRY_DELAY_MS = 100

export async function requireOriginPrivateStorage(
  page: Page,
  browserName: string,
): Promise<void> {
  const available = await page.evaluate(
    () => typeof (
      navigator.storage as Partial<OriginPrivateStorageManager> | undefined
    )?.getDirectory === 'function',
  )
  // OPFS recovery has no truthful substitute on unsupported engines; portable output is matrix-tested.
  test.skip(!available, `${browserName} does not expose the Origin Private File System API`)
}

export async function removePersistentBrowserProfile(profile: string): Promise<void> {
  // Browser processes can release profile handles shortly after context.close resolves on Windows.
  await rm(profile, {
    recursive: true,
    force: true,
    maxRetries: PROFILE_REMOVAL_MAX_RETRIES,
    retryDelay: PROFILE_REMOVAL_RETRY_DELAY_MS,
  })
}
