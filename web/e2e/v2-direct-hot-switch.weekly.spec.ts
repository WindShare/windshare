import { expect, test } from '@playwright/test'

import { runHotSwitchScenario } from './fixtures/hot-switch-scenario'

test('continues on an authenticated peer lane after the relay is cut', async ({
  browserName,
  page,
}, testInfo) => {
  expect(browserName).toBe('chromium')
  await runHotSwitchScenario({
    browserName,
    mode: 'direct',
    page,
    testInfo,
  })
})
