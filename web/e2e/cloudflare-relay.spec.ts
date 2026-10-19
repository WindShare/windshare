import { expect, test } from '@playwright/test'

import { runDownloadScenario } from './cloudflare/download-scenario'
import { IsolatedDeployment } from './cloudflare/isolated-deployment'
import { runCloudflareIsolationScenario } from './cloudflare/isolation-scenario'

test('Cloudflare receiver interruptions preserve the sender and healthy sibling', async ({ browser, page }, testInfo) => {
  const worker = new IsolatedDeployment(testInfo.outputPath('deployment'))
  try {
    const { target } = await worker.provision()
    await runCloudflareIsolationScenario(browser, page, testInfo, target)
  } finally {
    await worker.dispose()
  }
})

for (const kind of ['object-reset', 'redeploy'] as const) {
  test(`the original Cloudflare download continues after ${kind}`, async ({ page, request }, testInfo) => {
    const worker = new IsolatedDeployment(testInfo.outputPath('deployment'))
    try {
      const { target, deployment } = await worker.provision()
      await runDownloadScenario(page, request, testInfo, target, {
        kind,
        run: async (control, before) => {
          expect(before.versionId).toBe(deployment.versionId)
          expect(before.revision).toBe(deployment.revision)
          if (kind === 'object-reset') return control.resetOnce(before)
          const updated = await worker.redeploy()
          const after = await control.waitForVersion(updated.versionId)
          expect(after.revision).toBe(updated.revision)
          expect(after.storageMarker, 'The original object storage must survive deployment').toBe(before.storageMarker)
          return { beforeDeployment: deployment, afterDeployment: updated, before, after }
        },
      })
    } finally {
      await worker.dispose()
    }
  })
}
