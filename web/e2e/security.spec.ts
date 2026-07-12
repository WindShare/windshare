import { browserMetrics, installBrowserHarness, readOpfs } from './fixtures/browser'
import {
  disableCapabilityArtifacts,
  expect,
  expectFragmentErased,
  navigateToShare,
  test,
} from './fixtures/test'

disableCapabilityArtifacts()

test('authenticated traversal manifest is rejected before selection or output access', async ({
  page,
  stack,
  baseUrl,
}) => {
  const share = await stack.hostileShare(baseUrl)
  expect(share.link).toBeDefined()

  await installBrowserHarness(page, { directoryPicker: true, rejectPeerConnections: true })
  await navigateToShare(page, share.link!)
  await expectFragmentErased(page)
  await expect(page.getByRole('alert')).toHaveText(
    'The share link or separate key is invalid.',
  )

  const metrics = await browserMetrics(page)
  expect(metrics.fragmentPresentAtFirstSocket).toBe(false)
  expect(metrics.requests).toHaveLength(0)
  expect(metrics.output.pickerCalls).toBe(0)
  expect(metrics.signalOffers).toBe(0)
  expect(metrics.signalIceCandidates).toBe(0)
  expect(await readOpfs(page)).toEqual([])
  await expect(page.getByText('../escape.txt')).toHaveCount(0)
})
