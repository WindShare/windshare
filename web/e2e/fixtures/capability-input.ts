import type { Page } from '@playwright/test'

const CAPABILITY_FIELD_ID = 'capability-key'
const CAPABILITY_DISCARD_EVENT = 'windshare-e2e-discard-capability'
const CAPABILITY_OWNER_LIFETIME_MILLISECONDS = 30_000

async function installCapabilityFieldOwner(page: Page): Promise<void> {
  await page.evaluate(
    ({ discardEvent, fieldId, lifetimeMilliseconds }) => {
      const field = document.getElementById(fieldId)
      const form = field instanceof HTMLInputElement ? field.form : null
      const submit = form?.querySelector('button[type="submit"]')
      if (
        !(field instanceof HTMLInputElement) ||
        !(form instanceof HTMLFormElement) ||
        !(submit instanceof HTMLButtonElement)
      ) throw new Error('Capability form is unavailable')

      let ownedKey: string | undefined
      const expiration: { frame?: number; timer?: number } = {}
      let disposed = false
      const capture = () => {
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
      const releaseAfterSubmit = () => dispose()
      const releaseAfterPreventedClick = (event: MouseEvent) => {
        if (event.defaultPrevented) dispose()
      }
      const restore = () => {
        if (ownedKey === undefined) return
        // React snapshots the input during this activation. The next frame is
        // the fail-closed boundary when validation prevents submission.
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
      expiration.timer = window.setTimeout(dispose, lifetimeMilliseconds)
    },
    {
      discardEvent: CAPABILITY_DISCARD_EVENT,
      fieldId: CAPABILITY_FIELD_ID,
      lifetimeMilliseconds: CAPABILITY_OWNER_LIFETIME_MILLISECONDS,
    },
  )
}

async function discardCapabilityField(page: Page): Promise<void> {
  let fieldIsSafe: boolean
  try {
    fieldIsSafe = await page.evaluate(({ discardEvent, fieldId }) => {
      window.dispatchEvent(new Event(discardEvent))
      const field = document.getElementById(fieldId)
      if (field === null) return true
      if (!(field instanceof HTMLInputElement)) return false
      field.value = ''
      return true
    }, { discardEvent: CAPABILITY_DISCARD_EVENT, fieldId: CAPABILITY_FIELD_ID })
  } catch {
    fieldIsSafe = page.isClosed()
  }
  if (fieldIsSafe) return
  await page.close({ runBeforeUnload: false }).catch(() => undefined)
  if (!page.isClosed()) await page.context().close().catch(() => undefined)
}

export async function submitCapabilityKey(page: Page, temporaryKey: string): Promise<void> {
  try {
    await installCapabilityFieldOwner(page)
    await page.getByLabel('Separate key').fill(temporaryKey)
    await page.getByRole('button', { name: 'Open share' }).click()
  } catch {
    await discardCapabilityField(page)
    throw new Error('The browser could not submit the temporary capability key')
  }
}
