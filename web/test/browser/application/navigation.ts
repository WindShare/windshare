import { expect, type Page, type Response } from '@playwright/test'

export async function openApplication(page: Page, url: string): Promise<Response | null> {
  const response = await page.goto(url)
  await waitForApplicationInitialization(page)
  return response
}

export async function reloadApplication(page: Page): Promise<Response | null> {
  const response = await page.reload()
  await waitForApplicationInitialization(page)
  return response
}

async function waitForApplicationInitialization(page: Page): Promise<void> {
  // Async module evaluation can outlive the browser load event. The composition
  // root installs diagnostics after initializing the receiver, once per document.
  await expect.poll(() => page.evaluate(() => window.windshareDiagnostics !== undefined), {
    message: 'Application initialization must install diagnostics in the current document',
  }).toBe(true)
}
