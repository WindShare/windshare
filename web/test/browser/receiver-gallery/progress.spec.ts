import { expect, test } from '@playwright/test'
import { capture, CURRENT_TASK, expectNoHorizontalOverflow, GALLERY_PATH, showScenario } from './assertions'
import type { V2ReceiverProgress } from '../../../src/ui/v2-model'

const MIB = 1024n * 1024n

test('task progress counts received bytes while discovery is open and uses an exact denominator afterwards', async ({ page }) => {
  await page.clock.install()
  await page.goto(GALLERY_PATH)
  await showScenario(page, 'folder')
  const task = page.locator(CURRENT_TASK)
  await expect(task.getByText(/Calculating total/)).toBeVisible()
  expect(await task.getByRole('progressbar').getAttribute('value')).toBeNull()
  await task.scrollIntoViewIfNeeded()
  await capture(page, 'progress-counting')
  const update = async (patch: Partial<V2ReceiverProgress>) => page.evaluate(value => {
    const gallery = window as typeof window & { windshareAdvanceProgress(patch: Partial<V2ReceiverProgress>): void }
    gallery.windshareAdvanceProgress(value)
  }, patch)
  await update({ writtenBytes: 0n, materializedBytes: 0n })
  await page.clock.runFor(1000)
  for (let second = 1; second <= 4; second++) {
    await update({ writtenBytes: BigInt(second) * MIB, materializedBytes: BigInt(second) * MIB })
    await page.clock.runFor(1000)
  }
  await expect(task.getByText(/MiB\/s/)).toBeVisible()
  await expect(task.getByText(/About .* left/)).toHaveCount(0)
  await update({ discovery: 'complete', discoveredBytes: 16n * MIB, discoveredFiles: 20 })
  await page.clock.runFor(1000)
  await expect(task.getByRole('progressbar')).toHaveAttribute('value', '25')
  await expect(task.getByText(/Calculating total/)).toHaveCount(0)
  await expect(task.getByText(/4.0 MiB \/ 16.0 MiB/)).toBeVisible()
  for (let second = 5; second <= 10; second++) {
    await update({ writtenBytes: BigInt(second) * MIB, materializedBytes: BigInt(second) * MIB })
    await page.clock.runFor(1000)
  }
  await expect(task.getByText(/1.0 MiB\/s · About/)).toBeVisible()
  await task.scrollIntoViewIfNeeded()
  await capture(page, 'progress-exact')
  await page.setViewportSize({ width: 320, height: 568 })
  await expectNoHorizontalOverflow(page)
  await task.scrollIntoViewIfNeeded()
  await capture(page, 'progress-narrow', true)
  await page.clock.runFor(6000)
  await expect(task.getByText(/0 B\/s/)).toBeVisible()
  await expect(task.getByText(/About .* left/)).toHaveCount(0)
})
