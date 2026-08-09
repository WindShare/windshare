import { expect, test, type Page } from '@playwright/test'

import { requireOriginPrivateStorage } from './browser-storage-support'
import type { ArtifactActionActivationProof } from './artifact-action-user-activation-harness'

const HARNESS_PATH = '/test/browser/artifact-action-user-activation-harness.ts'
const ACTION_LABEL = 'Save using original folder hierarchy'

test('explicit DirectoryTree action starts one picker in the trusted Chromium activation stack', async ({
  browserName,
  page,
}) => {
  await page.goto('/')
  await requireOriginPrivateStorage(page, browserName)

  const beforeClick = await installHarness(page)
  expect(beforeClick).toMatchObject({
    actionLabel: ACTION_LABEL,
    selectedArtifactKind: 'directory-tree',
    selectedPlanKind: 'direct-tree',
    pickerCallsBeforeClick: 0,
    pickerCalls: 0,
  })

  await page.getByRole('button', { name: ACTION_LABEL, exact: true }).click()

  expect(await readProof(page)).toEqual({
    actionLabel: ACTION_LABEL,
    selectedArtifactKind: 'directory-tree',
    selectedPlanKind: 'direct-tree',
    pickerCallsBeforeClick: 0,
    pickerCalls: 1,
    pickerMode: 'readwrite',
    clickWasTrusted: true,
    userActivationWasActive: true,
    pickerStartedBeforeActionReturned: true,
    authorityReleased: true,
  })
})

async function installHarness(page: Page): Promise<ArtifactActionActivationProof> {
  return page.evaluate(async (path) => {
    const harness = await import(path) as typeof import('./artifact-action-user-activation-harness')
    return harness.installArtifactActionActivationHarness()
  }, HARNESS_PATH)
}

async function readProof(page: Page): Promise<ArtifactActionActivationProof> {
  return page.evaluate(async (path) => {
    const harness = await import(path) as typeof import('./artifact-action-user-activation-harness')
    return harness.readArtifactActionActivationProof()
  }, HARNESS_PATH)
}
