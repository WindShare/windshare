import { test } from '@playwright/test'

import { runHotSwitchScenario } from './fixtures/hot-switch-scenario'

test('uses native peer hot-switch or the real relay fallback for this browser', async ({
  browserName,
  page,
}, testInfo) => {
  // The runner starts the owned direct Vite origin before probing native RTC.
  // This keeps the product scenario independent of a contract-config base URL.
  await runHotSwitchScenario({ browserName, mode: 'native-capability', page, testInfo })
})
