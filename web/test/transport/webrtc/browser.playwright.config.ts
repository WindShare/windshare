import { join } from 'node:path'
import { defineConfig } from '@playwright/test'

import { createPlaywrightSuiteDeclarations } from '../../../playwright.suite-config.ts'
import {
  CHILD_EVIDENCE_CONTEXT_ENV,
  readChildEvidenceContext,
  type ChildEvidenceContext,
} from '../../../scripts/browser-evidence/child-evidence.js'

const WEB_BASE_URL = ownedWebBaseURL()
const EVIDENCE_CONTEXT = optionalEvidenceContext()
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
})

function optionalEvidenceContext(): ChildEvidenceContext | null {
  const encoded = process.env[CHILD_EVIDENCE_CONTEXT_ENV]
  return encoded === undefined || encoded === '' ? null : readChildEvidenceContext()
}

function ownedWebBaseURL(): string {
  const encoded = process.env.WINDSHARE_WEB_BASE_URL ?? 'http://127.0.0.1:1'
  const parsed = new URL(encoded)
  const port = Number(parsed.port)
  if (
    parsed.protocol !== 'http:' || parsed.hostname !== '127.0.0.1' ||
    !Number.isSafeInteger(port) || port < 1 || port > 65_535 ||
    parsed.pathname !== '/' || parsed.search !== '' || parsed.hash !== ''
  ) throw new Error('WINDSHARE_WEB_BASE_URL must identify an owned loopback HTTP listener')
  return parsed.href.slice(0, -1)
}
