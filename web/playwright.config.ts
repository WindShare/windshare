import { isAbsolute, join } from 'node:path'
import { defineConfig } from '@playwright/test'

import { createPlaywrightSuiteDeclarations } from './playwright.suite-config.ts'

const WEB_BASE_URL = ownedWebBaseURL()
const BROWSER_TEST_TIMEOUT_MS = 120_000
const CHILD_EVIDENCE_CONTEXT_ENV = 'WINDSHARE_BROWSER_EVIDENCE_CONTEXT'
const EVIDENCE_ARTIFACT_ROOT = evidenceArtifactRoot()
const OUTPUT_DIRECTORY = EVIDENCE_ARTIFACT_ROOT === null
  ? 'test-results'
  : join(EVIDENCE_ARTIFACT_ROOT, 'playwright')
const SUITE_DECLARATIONS = createPlaywrightSuiteDeclarations(
  'main',
  'base',
  [],
)

export default defineConfig({
  ...SUITE_DECLARATIONS,
  outputDir: OUTPUT_DIRECTORY,
  fullyParallel: false,
  forbidOnly: true,
  retries: 0,
  workers: 1,
  reporter: 'line',
  timeout: BROWSER_TEST_TIMEOUT_MS,
  use: {
    baseURL: WEB_BASE_URL,
    locale: 'en-US',
    timezoneId: 'UTC',
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
    video: 'off',
  },
})

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

function evidenceArtifactRoot(): string | null {
  const encoded = process.env[CHILD_EVIDENCE_CONTEXT_ENV]
  if (encoded === undefined || encoded === '') return null
  const context: unknown = JSON.parse(encoded)
  if (
    context === null || typeof context !== 'object' || Array.isArray(context) ||
    !Object.hasOwn(context, 'artifactRoot')
  ) throw new Error('browser evidence context is missing its artifact root')
  const artifactRoot = (context as { readonly artifactRoot?: unknown }).artifactRoot
  if (typeof artifactRoot !== 'string' || !isAbsolute(artifactRoot)) {
    throw new Error('browser evidence artifact root must be absolute')
  }
  return artifactRoot
}
