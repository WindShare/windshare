import { join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { defineConfig } from '@playwright/test'

import { createPlaywrightSuiteDeclarations } from '../../../playwright.suite-config.ts'
import {
  CHILD_EVIDENCE_CONTEXT_ENV,
  readChildEvidenceContext,
  type ChildEvidenceContext,
} from '../../../scripts/browser-evidence/child-evidence.js'

const REPOSITORY_ROOT = fileURLToPath(new URL('../../../../', import.meta.url))
const WEB_ROOT = fileURLToPath(new URL('../../../', import.meta.url))
const PREBUILT_SERVER_LAUNCHER = fileURLToPath(new URL(
  '../../../scripts/browser-evidence/prebuilt-server-launcher.mjs',
  import.meta.url,
))
const WEB_HOST = '127.0.0.1'
const MAXIMUM_NETWORK_PORT = 65_535
const WEB_PORT = environmentPort('WINDSHARE_PION_WEB_PORT', 4176)
const WEB_BASE_URL = `http://${WEB_HOST}:${WEB_PORT}`
const PION_ADDRESS = process.env['WINDSHARE_PION_HTTP_ADDRESS'] ?? '127.0.0.1:17849'
const DEFAULT_TOPOLOGY_PROFILE = fileURLToPath(new URL(
  '../../../../testdata/test-ice-topology/pr-same-host-kernel-route-ipv4.json',
  import.meta.url,
))
const SERVER_TIMEOUT_MS = 120_000
const EVIDENCE_CONTEXT = optionalEvidenceContext()
const PION_SERVER_ENV = pionServerEnvironment()
const SUITE_DECLARATIONS = createPlaywrightSuiteDeclarations('pion')

export default defineConfig({
  ...SUITE_DECLARATIONS,
  outputDir: EVIDENCE_CONTEXT === null
    ? '../../../test-results/d2-webrtc'
    : join(EVIDENCE_CONTEXT.artifactRoot, 'playwright'),
  fullyParallel: false,
  forbidOnly: true,
  retries: 0,
  workers: 1,
  reporter: 'line',
  timeout: 120_000,
  use: {
    baseURL: WEB_BASE_URL,
    locale: 'en-US',
    timezoneId: 'UTC',
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
    video: 'off',
  },
  webServer: [
    {
      command:
        `pnpm exec vite --config test/transport/webrtc/vite.config.ts ` +
        `--host ${WEB_HOST} --port ${WEB_PORT} --strictPort`,
      cwd: WEB_ROOT,
      url: WEB_BASE_URL,
      reuseExistingServer: false,
      timeout: SERVER_TIMEOUT_MS,
    },
    {
      command: `${JSON.stringify(process.execPath)} ${JSON.stringify(PREBUILT_SERVER_LAUNCHER)}`,
      cwd: REPOSITORY_ROOT,
      env: {
        WINDSHARE_D1_BROWSER_ADDR: PION_ADDRESS,
        WINDSHARE_D1_BROWSER_SCENARIO: 'happy',
        ...PION_SERVER_ENV,
      },
      url: `http://${PION_ADDRESS}/healthz`,
      reuseExistingServer: false,
      timeout: SERVER_TIMEOUT_MS,
    },
  ],
})

function pionServerEnvironment(): Record<string, string> {
  const environment: Record<string, string> = {
    WINDSHARE_TEST_ICE_TOPOLOGY_PROFILE:
      EVIDENCE_CONTEXT?.topologyProfilePath ??
      process.env['WINDSHARE_TEST_ICE_TOPOLOGY_PROFILE'] ??
      DEFAULT_TOPOLOGY_PROFILE,
  }
  const forwarded = {
    WINDSHARE_TEST_ICE_TOPOLOGY_RESOLUTION: EVIDENCE_CONTEXT?.topologyResolutionPath,
    WINDSHARE_TEST_ICE_TOPOLOGY_PROFILE_SHA256: EVIDENCE_CONTEXT?.topologyProfileSha256,
    WINDSHARE_TEST_ICE_TOPOLOGY_RESOLUTION_SHA256: EVIDENCE_CONTEXT?.topologyResolutionSha256,
  }
  for (const name of Object.keys(forwarded)) {
    const contextValue = forwarded[name as keyof typeof forwarded]
    const value = process.env[name]
    if (contextValue !== undefined) environment[name] = contextValue
    else if (value !== undefined && value !== '') environment[name] = value
  }
  return environment
}

function optionalEvidenceContext(): ChildEvidenceContext | null {
  const encoded = process.env[CHILD_EVIDENCE_CONTEXT_ENV]
  return encoded === undefined || encoded === '' ? null : readChildEvidenceContext()
}

function environmentPort(name: string, fallback: number): number {
  const value = Number(process.env[name] ?? fallback)
  if (!Number.isSafeInteger(value) || value < 1 || value > MAXIMUM_NETWORK_PORT) {
    throw new Error(`${name} must be a TCP port`)
  }
  return value
}
