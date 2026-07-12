import { expect as playwrightExpect, test as playwright, type Page } from '@playwright/test'

import {
  RealStack,
  buildE2EBinaries,
  removeE2EBinaries,
  type BinaryPaths,
  type ManagedProcess,
} from './process'

interface TestFixtures {
  readonly stack: RealStack
  readonly baseUrl: string
}

interface WorkerFixtures {
  readonly binaries: BinaryPaths
}

const SEPARATE_KEY_FIELD_ID = 'capability-key'
const SEPARATE_KEY_DISCARD_EVENT = 'windshare-e2e-discard-separate-key'
const SEPARATE_KEY_OWNER_LIFETIME_MS = 30_000

async function runWithCleanup(
  operation: () => Promise<void>,
  cleanup: () => Promise<void>,
  boundary: string,
): Promise<void> {
  let failed = false
  let primaryFailure: unknown
  try {
    await operation()
  } catch (error) {
    failed = true
    primaryFailure = error
  }
  try {
    await cleanup()
  } catch (cleanupFailure) {
    if (failed) {
      throw new AggregateError(
        [primaryFailure, cleanupFailure],
        `${boundary} and cleanup both failed`,
        { cause: cleanupFailure },
      )
    }
    throw cleanupFailure
  }
  if (failed) throw primaryFailure
}

export const test = playwright.extend<TestFixtures, WorkerFixtures>({
  binaries: [
    async ({ browserName }, use) => {
      if (browserName !== 'chromium') {
        throw new Error('The M1b real-browser gate requires Chromium')
      }
      const binaries = await buildE2EBinaries()
      await runWithCleanup(
        () => use(binaries),
        () => removeE2EBinaries(binaries),
        'E2E worker',
      )
    },
    { scope: 'worker', timeout: 180_000 },
  ],

  stack: async ({ binaries }, use) => {
    const stack = new RealStack(binaries)
    await runWithCleanup(
      async () => {
        await stack.start()
        await use(stack)
      },
      () => stack.dispose(),
      'Real-stack test',
    )
  },

  baseUrl: async ({ baseURL }, use) => {
    if (baseURL === undefined) {
      throw new Error('Playwright baseURL is required for real-stack E2E')
    }
    await use(baseURL)
  },
})

export function disableCapabilityArtifacts(): void {
  // C5 intentionally drives capability URLs and separate keys. Failure artifacts
  // must not persist those ephemeral secrets after the fixture tears the share down.
  test.use({ trace: 'off', screenshot: 'off', video: 'off' })
}

export { playwrightExpect as expect }

export async function waitUntilReady(page: Page, sender?: ManagedProcess): Promise<void> {
  const ready = page
    .getByRole('status')
    .filter({ hasText: 'Choose what to save, then start the download.' })
    .waitFor()
  const failed = page.getByRole('alert').waitFor().then(async () => {
    const publicMessage = await page.getByRole('alert').textContent()
    const harness = await page.evaluate(() => {
      const candidate = (window as unknown as { readonly __windshareE2E?: unknown }).__windshareE2E
      return candidate ?? null
    })
    throw new Error(
      `Receiver failed before becoming ready: ${publicMessage ?? '<empty>'}. ` +
      `sender stderr=${JSON.stringify(sender?.stderr ?? '')} ` +
      `browser harness=${JSON.stringify(harness)}`,
    )
  })
  await Promise.race([ready, failed])
}

export async function navigateToShare(page: Page, capabilityUrl: string): Promise<void> {
  try {
    await page.goto(capabilityUrl)
  } catch {
    // Playwright includes navigation arguments in its native error. The random
    // capability is already unusable after teardown, but it still must not enter logs.
    throw new Error('The browser could not navigate to the temporary share')
  }
}

export async function expectFragmentErased(page: Page): Promise<void> {
  // URL assertions print the received URL on failure, which is precisely when a
  // fragment-erasure regression would otherwise copy the capability into logs.
  await playwrightExpect
    .poll(async () => {
      try {
        return await page.evaluate(() => window.location.hash !== '')
      } catch {
        throw new Error('The browser could not inspect fragment erasure safely')
      }
    })
    .toBe(false)
}

async function installSeparateKeyFieldOwner(page: Page): Promise<void> {
  await page.evaluate(
    ({ discardEvent, fieldId, lifetimeMs }) => {
      const field = document.getElementById(fieldId)
      const form = field instanceof HTMLInputElement ? field.form : null
      const submit = form?.querySelector('button[type="submit"]')
      if (
        !(field instanceof HTMLInputElement) ||
        !(form instanceof HTMLFormElement) ||
        !(submit instanceof HTMLButtonElement)
      ) {
        throw new Error('Separate-key form is unavailable')
      }

      let ownedKey: string | undefined
      const expiration: { frame?: number; timer?: number } = {}
      let disposed = false
      const capture = () => {
        // Move the capability out of rendered state in the same browser task that
        // receives it. Playwright may cancel a later action without running test code.
        ownedKey = field.value
        field.value = ''
      }
      const dispose = () => {
        if (disposed) return
        disposed = true
        field.value = ''
        ownedKey = undefined
        if (expiration.frame !== undefined) window.cancelAnimationFrame(expiration.frame)
        if (expiration.timer !== undefined) window.clearTimeout(expiration.timer)
        field.removeEventListener('input', capture, true)
        submit.removeEventListener('click', restore, true)
        window.removeEventListener('click', releaseAfterPreventedClick)
        window.removeEventListener('submit', releaseAfterSubmit)
        window.removeEventListener('pagehide', dispose)
        window.removeEventListener(discardEvent, dispose)
      }
      const releaseAfterSubmit = () => {
        // React's delegated submit handler runs at the application root before the
        // event reaches window, so its synchronous capability snapshot is complete.
        dispose()
      }
      const releaseAfterPreventedClick = (event: MouseEvent) => {
        if (event.defaultPrevented) dispose()
      }
      const restore = () => {
        if (ownedKey === undefined) return
        // Activation and validation complete before the next rendering opportunity.
        // Submit clears after the application snapshot; the frame is a fail-closed
        // fallback for prevented or otherwise non-submitting activation.
        window.addEventListener('submit', releaseAfterSubmit, { once: true })
        window.addEventListener('click', releaseAfterPreventedClick, { once: true })
        field.value = ownedKey
        ownedKey = undefined
        expiration.frame = window.requestAnimationFrame(dispose)
      }

      field.addEventListener('input', capture, { capture: true })
      submit.addEventListener('click', restore, { capture: true })
      window.addEventListener('pagehide', dispose, { once: true })
      window.addEventListener(discardEvent, dispose, { once: true })
      expiration.timer = window.setTimeout(dispose, lifetimeMs)
    },
    {
      discardEvent: SEPARATE_KEY_DISCARD_EVENT,
      fieldId: SEPARATE_KEY_FIELD_ID,
      lifetimeMs: SEPARATE_KEY_OWNER_LIFETIME_MS,
    },
  )
}

async function discardSeparateKeyField(page: Page): Promise<void> {
  let fieldIsSafe: boolean
  try {
    fieldIsSafe = await page.evaluate(({ discardEvent, fieldId }) => {
      window.dispatchEvent(new Event(discardEvent))
      const field = document.getElementById(fieldId)
      if (field === null) return true
      if (!(field instanceof HTMLInputElement)) return false
      field.value = ''
      return true
    }, { discardEvent: SEPARATE_KEY_DISCARD_EVENT, fieldId: SEPARATE_KEY_FIELD_ID })
  } catch {
    // A closed or replaced document cannot contribute its former field to the
    // failure context. Other evaluation failures fall through to page teardown.
    fieldIsSafe = page.isClosed()
  }
  if (!fieldIsSafe) {
    // If the live document cannot prove destruction, remove the artifact source
    // itself instead of allowing Playwright's automatic ARIA snapshot to retain it.
    await page.close({ runBeforeUnload: false }).catch(() => undefined)
    if (!page.isClosed()) {
      await page.context().close().catch(() => undefined)
    }
  }
}

export async function submitSeparateKey(page: Page, temporaryKey: string): Promise<void> {
  try {
    await installSeparateKeyFieldOwner(page)
    await page.getByLabel('Separate key').fill(temporaryKey)
    await page.getByRole('button', { name: 'Open share' }).click()
  } catch {
    await discardSeparateKeyField(page)
    throw new Error('The browser could not submit the temporary separate key')
  }
}

export async function waitUntilComplete(page: Page): Promise<void> {
  await page.getByRole('status').filter({ hasText: 'Download complete.' }).waitFor()
}
