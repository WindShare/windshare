import { defineConfig } from '@playwright/test'

const spikeAddress = '127.0.0.1:17845'
const spikeURL = `http://${spikeAddress}`

export default defineConfig({
  testDir: './browser',
  timeout: 90_000,
  fullyParallel: false,
  workers: 1,
  reporter: [['line']],
  use: {
    baseURL: spikeURL,
    browserName: 'chromium',
    channel: 'chrome',
    headless: true,
  },
  webServer: {
    command: 'go run ./cmd/spike-server',
    url: `${spikeURL}/healthz`,
    reuseExistingServer: false,
    timeout: 60_000,
    env: {
      GOWORK: 'off',
      WINDSHARE_SPIKE_ADDR: spikeAddress,
    },
  },
})
