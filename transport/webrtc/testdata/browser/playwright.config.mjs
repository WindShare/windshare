import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { PLAYWRIGHT_BROWSER_PROJECTS } from '../../../../web/playwright.projects.ts'

const here = path.dirname(fileURLToPath(import.meta.url))
const repositoryRoot = path.resolve(here, '../../../..')
const address = '127.0.0.1:17846'
const baseURL = `http://${address}`
const scenario = process.env.WINDSHARE_D1_BROWSER_SCENARIO ?? 'happy'

export default {
  testDir: path.join(here, 'browser'),
  timeout: 90_000,
  fullyParallel: false,
  workers: 1,
  reporter: [['line']],
  projects: PLAYWRIGHT_BROWSER_PROJECTS,
  use: {
    baseURL,
    headless: true,
  },
  webServer: {
    command: 'go run ./transport/webrtc/testdata/browser/server',
    cwd: repositoryRoot,
    url: `${baseURL}/healthz`,
    reuseExistingServer: false,
    timeout: 60_000,
    env: {
      WINDSHARE_D1_BROWSER_ADDR: address,
      WINDSHARE_D1_BROWSER_SCENARIO: scenario,
    },
  },
}
