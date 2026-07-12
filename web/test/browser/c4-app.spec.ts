import { expect, test, type Page } from '@playwright/test'

interface C4Harness {
  mountC4Harness(mode?: 'ready' | 'key' | 'key-pending' | 'wide'): Promise<void>
  harnessState(): {
    readonly startCalls: number
    readonly aborted: boolean
    readonly joinedCapabilityCleared: boolean
  }
  cancelNextOutput(): void
  failAbortCleanup(): void
  failPendingJoin(): void
  emitProgress(): void
  emitReconnect(): void
  emitReconnected(): void
  completeTransfer(): void
  failTerminal(): void
}

const HARNESS_PATH = '/test/browser/c4-harness.ts'
const SHARE_ID = 'AAECAwQFBgcI'
const OTHER_SHARE_ID = 'AAECAwQFBgcJ'
const KEY = 'AQECAwQFBgcICQoLDA0ODxA'
const FULL_LINK = `https://windshare.test/v1/ws/${SHARE_ID}?r=https%3A%2F%2Frelay.test#${KEY}`

async function mount(
  page: Page,
  mode: 'ready' | 'key' | 'key-pending' | 'wide' = 'ready',
) {
  await page.goto('/')
  await page.evaluate(async ({ path, selectedMode }) => {
    const harness = (await import(path)) as C4Harness
    await harness.mountC4Harness(selectedMode)
  }, { path: HARNESS_PATH, selectedMode: mode })
}

test('supports accessible key entry, keyboard selection, gesture gating, and progress', async ({
  page,
}) => {
  await mount(page, 'key')
  const key = page.getByLabel('Separate key')
  await expect(key).toHaveAttribute('type', 'password')
  await key.fill(
    `https://windshare.test/v1/ws/${OTHER_SHARE_ID}?r=https%3A%2F%2Frelay.test#${KEY}`,
  )
  await key.press('Enter')
  await expect(page.getByRole('alert')).toHaveText('The separate key is invalid.')
  await expect(key).toBeFocused()
  await expect(key).toHaveValue('')
  await key.fill(FULL_LINK)
  await key.press('Enter')

  await expect(page.getByRole('group', { name: 'Files to download' })).toBeVisible()
  await expect(page.getByText('3 selected · 8 B')).toBeVisible()
  await page.getByRole('checkbox', { name: 'File "docs/a.txt"' }).press('Space')
  await expect(page.getByText('1 selected · 5 B')).toBeVisible()
  await page.getByRole('checkbox', { name: 'File "docs/a.txt"' }).press('Space')
  await expect(page.getByText('3 selected · 8 B')).toBeVisible()
  await page.getByRole('radio', { name: /Browser download/u }).check()

  expect(await page.evaluate(async (path) => {
    const harness = (await import(path)) as C4Harness
    return harness.harnessState().startCalls
  }, HARNESS_PATH)).toBe(0)
  await page.getByRole('button', { name: 'Download selected' }).click()
  expect(await page.evaluate(async (path) => {
    const harness = (await import(path)) as C4Harness
    return harness.harnessState().startCalls
  }, HARNESS_PATH)).toBe(1)
  await expect(page.getByRole('button', { name: 'Stop download' })).toBeFocused()

  await page.evaluate(async (path) => {
    const harness = (await import(path)) as C4Harness
    harness.emitProgress()
    harness.emitReconnect()
  }, HARNESS_PATH)
  await expect(page.getByText('Reconnect attempt 2')).toBeVisible()
  await expect(page.getByText('Blocks').locator('..').getByText('1 / 2')).toBeVisible()
  await expect(page.getByText('3 B / 8 B')).toBeVisible()

  await page.evaluate(async (path) => {
    const harness = (await import(path)) as C4Harness
    harness.emitReconnected()
    harness.completeTransfer()
  }, HARNESS_PATH)
  await expect(page.getByRole('status')).toContainText('Download complete')
  await expect(page.getByRole('status')).toBeFocused()
  await expect(page.getByRole('progressbar', { name: 'Download progress' })).toHaveAttribute(
    'value',
    '8',
  )
})

test('keeps a wide manifest keyboard-reachable through bounded selection pages', async ({ page }) => {
  await mount(page, 'wide')

  await expect(page.getByText('Showing 1–200 of 205')).toBeVisible()
  await expect(page.getByRole('checkbox')).toHaveCount(200)
  const next = page.getByRole('button', { name: 'Next' })
  await next.focus()
  await page.keyboard.press('Enter')

  await expect(page.getByText('Showing 201–205 of 205')).toBeVisible()
  await expect(page.getByText('Page 2 of 2')).toBeVisible()
  await expect(page.getByRole('checkbox')).toHaveCount(5)
  await expect(page.getByRole('checkbox', { name: 'File "wide-0200.bin"' })).toBeVisible()
  await page.getByRole('button', { name: 'Previous' }).click()
  await expect(page.getByText('Page 1 of 2')).toBeVisible()
})

test('destroys separate-key input before a late join failure offers an empty retry', async ({
  page,
}) => {
  await mount(page, 'key-pending')
  const key = page.getByLabel('Separate key')
  await key.fill(KEY)
  await key.press('Enter')

  await expect(key).toHaveCount(0)
  expect(await page.evaluate(async (path) => {
    const harness = (await import(path)) as C4Harness
    return harness.harnessState().joinedCapabilityCleared
  }, HARNESS_PATH)).toBe(true)

  await page.evaluate(async (path) => {
    const harness = (await import(path)) as C4Harness
    harness.failPendingJoin()
  }, HARNESS_PATH)
  await expect(page.getByRole('alert')).toHaveText('Could not connect to this share.')
  const retry = page.getByLabel('Separate key')
  await expect(retry).toBeVisible()
  await expect(retry).toHaveValue('')
  await expect(retry).toBeFocused()
})

test('renders bounded terminal failures and aborts through the controller signal', async ({ page }) => {
  await mount(page)
  await page.getByRole('button', { name: 'Download selected' }).click()
  await page.evaluate(async (path) => {
    const harness = (await import(path)) as C4Harness
    harness.failTerminal()
  }, HARNESS_PATH)
  await expect(page.getByRole('alert')).toHaveText(
    'The sender stopped the transfer: source file changed',
  )
  await expect(page.getByRole('alert')).toBeFocused()
  await expect(page.getByRole('progressbar', { name: 'Download progress' })).toBeVisible()

  await mount(page)
  await page.getByRole('button', { name: 'Download selected' }).click()
  await page.getByRole('button', { name: 'Stop download' }).click()
  await expect(page.getByRole('status')).toContainText('partial output cleaned up')
  await expect(page.getByRole('status')).toBeFocused()
  expect(await page.evaluate(async (path) => {
    const harness = (await import(path)) as C4Harness
    return harness.harnessState().aborted
  }, HARNESS_PATH)).toBe(true)
})

test('recovers canceled output and reports cleanup failure without false success', async ({ page }) => {
  await mount(page)
  await page.evaluate(async (path) => {
    const harness = (await import(path)) as C4Harness
    harness.cancelNextOutput()
  }, HARNESS_PATH)
  await page.getByRole('button', { name: 'Download selected' }).click()
  await expect(page.getByRole('alert')).toHaveText('The save prompt was canceled.')
  await expect(page.getByRole('button', { name: 'Download selected' })).toBeFocused()

  await page.evaluate(async (path) => {
    const harness = (await import(path)) as C4Harness
    harness.failAbortCleanup()
  }, HARNESS_PATH)
  await page.getByRole('button', { name: 'Download selected' }).click()
  await page.getByRole('button', { name: 'Stop download' }).click()
  await expect(page.getByRole('alert')).toHaveText(
    'The download could not be completed safely.',
  )
  await expect(page.getByRole('status')).toContainText('Download failed')
  await expect(page.getByRole('alert')).toBeFocused()
})

test('production boot erases the capability fragment before relay work completes', async ({ page }) => {
  const relay = encodeURIComponent('http://127.0.0.1:1')
  await page.goto(`/${SHARE_ID}?r=${relay}#${KEY}`)

  await expect.poll(() => page.evaluate(() => window.location.hash)).toBe('')
  await expect(page.getByRole('heading', { name: 'Save a shared download' })).toBeVisible()
})

test('production boot erases and never reflects a hostile malformed fragment', async ({ page }) => {
  const marker = 'DO-NOT-REFLECT-THIS-CAPABILITY'
  const relay = encodeURIComponent('http://127.0.0.1:1')
  await page.goto(`/${SHARE_ID}?r=${relay}#${marker}`)

  await expect.poll(() => page.evaluate(() => window.location.hash)).toBe('')
  await expect(page.getByRole('alert')).toHaveText('This is not a valid WindShare link.')
  await expect(page.locator('body')).not.toContainText(marker)
})

test('a persisted pagehide keeps the restored key-entry controller usable', async ({ page }) => {
  const relay = encodeURIComponent('http://127.0.0.1:1')
  await page.goto(`/${SHARE_ID}?r=${relay}`)
  const key = page.getByLabel('Separate key')
  await expect(key).toBeVisible()
  await page.evaluate(() => {
    const state = { instances: 0, closes: 0 }
    const stateWindow = window as unknown as { c4SocketState: typeof state }
    stateWindow.c4SocketState = state
    Object.defineProperty(window, 'WebSocket', {
      configurable: true,
      value: class FailingWebSocket {
        static readonly CONNECTING = 0
        static readonly OPEN = 1
        static readonly CLOSING = 2
        static readonly CLOSED = 3
        readonly listeners = new Map<string, Set<EventListener>>()
        readyState = FailingWebSocket.CONNECTING
        bufferedAmount = 0
        binaryType = 'blob'

        constructor() {
          state.instances += 1
        }

        addEventListener(type: string, listener: EventListener) {
          const listeners = this.listeners.get(type) ?? new Set<EventListener>()
          listeners.add(listener)
          this.listeners.set(type, listeners)
        }

        removeEventListener(type: string, listener: EventListener) {
          this.listeners.get(type)?.delete(listener)
        }

        close() {
          state.closes += 1
          this.readyState = FailingWebSocket.CLOSED
        }
      },
    })
  })
  await key.fill(KEY)
  await key.press('Enter')
  await expect.poll(() => page.evaluate(() =>
    (window as unknown as { c4SocketState: { instances: number } }).c4SocketState.instances,
  )).toBe(1)

  await page.evaluate(() => {
    window.dispatchEvent(new PageTransitionEvent('pagehide', { persisted: true }))
  })
  expect(await page.evaluate(() =>
    (window as unknown as { c4SocketState: { closes: number } }).c4SocketState.closes,
  )).toBe(0)

  await page.evaluate(() => {
    window.dispatchEvent(new PageTransitionEvent('pagehide', { persisted: false }))
  })
  await expect.poll(() => page.evaluate(() =>
    (window as unknown as { c4SocketState: { closes: number } }).c4SocketState.closes,
  )).toBe(1)
})
