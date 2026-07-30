import { randomBytes } from 'node:crypto'

import type { ContainedBrowserSampleDependencies } from './contracts.ts'
import { launchPlaywrightBrowser } from './playwright-session.ts'
import { createContainedBrowserPionControl } from './remote-control.ts'

export const DEFAULT_CONTAINED_BROWSER_DEPENDENCIES: ContainedBrowserSampleDependencies =
  Object.freeze({
    launch: launchPlaywrightBrowser,
    control: createContainedBrowserPionControl,
    requestId: () => randomBytes(24).toString('base64url'),
    delay: abortableDelay,
    now: Date.now,
  })

async function abortableDelay(milliseconds: number, signal: AbortSignal): Promise<void> {
  await new Promise<void>((resolve, reject) => {
    if (signal.aborted) {
      reject(new Error('contained browser sample was terminated'))
      return
    }
    const timeout = setTimeout(done, milliseconds)
    signal.addEventListener('abort', aborted, { once: true })
    function done(): void {
      signal.removeEventListener('abort', aborted)
      resolve()
    }
    function aborted(): void {
      clearTimeout(timeout)
      reject(new Error('contained browser sample was terminated'))
    }
  })
}
