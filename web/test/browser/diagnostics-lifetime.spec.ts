import { expect, test } from '@playwright/test'
import type { WindShareDiagnostics } from '../../src/diagnostics/export/developer-api'

const BASE64URL_IDENTITY_PATTERN = /^[A-Za-z0-9_-]{22}$/u
const EXPECTED_API_METHODS = Object.freeze([
  'clear',
  'disable',
  'enable',
  'export',
  'inspectLastFailure',
  'status',
])

test('diagnostic evidence stays page-local and manual disable survives reload', async ({ page }) => {
  const detailedTraceConsoleMessages: string[] = []
  page.on('console', (message) => {
    if (message.text().startsWith('windshare.receive')) {
      detailedTraceConsoleMessages.push(message.text())
    }
  })

  await page.goto('/')

  const initial = await page.evaluate(() => {
    const descriptor = Object.getOwnPropertyDescriptor(window, 'windshareDiagnostics')
    const diagnostics: WindShareDiagnostics = window.windshareDiagnostics
    return {
      descriptor: descriptor === undefined
        ? null
        : {
            configurable: descriptor.configurable,
            enumerable: descriptor.enumerable,
            writable: descriptor.writable,
          },
      frozen: Object.isFrozen(diagnostics),
      methods: Object.keys(diagnostics).sort(),
      status: diagnostics.status(),
      lastFailure: diagnostics.inspectLastFailure(),
      bundle: diagnostics.export(),
    }
  })
  const initialBundle = parseBundle(initial.bundle)

  expect(initial.descriptor).toEqual({
    configurable: false,
    enumerable: false,
    writable: false,
  })
  expect(initial.frozen).toBe(true)
  expect(initial.methods).toEqual(EXPECTED_API_METHODS)
  expect(initial.status).toMatchObject({
    schema_version: 1,
    state: 'idle',
    enabled: false,
    capture_generation: '0',
    retained_event_count: '0',
    retained_event_bytes: '0',
    incident_marker_count: '0',
  })
  expect(initial.lastFailure).toBeNull()
  expect(initialBundle.lineTypes).toEqual(['bundle_header'])
  expect(initialBundle.runtimeRunId).toMatch(BASE64URL_IDENTITY_PATTERN)
  expect(detailedTraceConsoleMessages).toEqual([])

  const clearedWhileEnabled = await page.evaluate(() => {
    const diagnostics: WindShareDiagnostics = window.windshareDiagnostics
    const enabled = diagnostics.enable()
    diagnostics.clear()
    return {
      enabled,
      afterClear: diagnostics.status(),
      lastFailure: diagnostics.inspectLastFailure(),
      bundle: diagnostics.export(),
    }
  })
  const clearedBundle = parseBundle(clearedWhileEnabled.bundle)

  expect(clearedWhileEnabled.enabled).toMatchObject({
    state: 'recording_pre_failure',
    enabled: true,
    capture_generation: '1',
  })
  expect(clearedWhileEnabled.afterClear).toMatchObject({
    state: 'recording_pre_failure',
    enabled: true,
    capture_generation: clearedWhileEnabled.enabled.capture_generation,
    expires_at: clearedWhileEnabled.enabled.expires_at,
    retained_event_count: '0',
    retained_event_bytes: '0',
    incident_marker_count: '0',
  })
  expect(clearedWhileEnabled.lastFailure).toBeNull()
  expect(clearedBundle.lineTypes).toEqual(['bundle_header', 'trace_capture'])
  expect(clearedBundle.runtimeRunId).toBe(initialBundle.runtimeRunId)

  const disabled = await page.evaluate(() => {
    const diagnostics: WindShareDiagnostics = window.windshareDiagnostics
    // Startup-owned retained work may legitimately complete between browser turns.
    // Clear and seal in one turn so this assertion isolates recorder lifetime semantics.
    diagnostics.clear()
    return {
      status: diagnostics.disable(),
      bundle: diagnostics.export(),
    }
  })
  const disabledBundle = parseBundle(disabled.bundle)

  expect(disabled.status).toMatchObject({
    state: 'sealed',
    enabled: false,
    capture_generation: clearedWhileEnabled.enabled.capture_generation,
    seal_reason: 'manual_disable',
    retained_event_count: '0',
    retained_event_bytes: '0',
    incident_marker_count: '0',
  })
  expect(disabled.status).not.toHaveProperty('expires_at')
  expect(disabledBundle.lineTypes).toEqual(['bundle_header', 'trace_capture'])
  expect(disabledBundle.runtimeRunId).toBe(initialBundle.runtimeRunId)
  expect(detailedTraceConsoleMessages).toEqual([])

  await page.reload()

  const reloaded = await page.evaluate(() => {
    const diagnostics: WindShareDiagnostics = window.windshareDiagnostics
    return {
      status: diagnostics.status(),
      lastFailure: diagnostics.inspectLastFailure(),
      bundle: diagnostics.export(),
    }
  })
  const reloadedBundle = parseBundle(reloaded.bundle)

  expect(reloaded.status).toMatchObject({
    state: 'idle',
    enabled: false,
    capture_generation: '0',
    retained_event_count: '0',
    retained_event_bytes: '0',
    incident_marker_count: '0',
  })
  expect(reloaded.lastFailure).toBeNull()
  expect(reloadedBundle.lineTypes).toEqual(['bundle_header'])
  expect(reloadedBundle.runtimeRunId).toMatch(BASE64URL_IDENTITY_PATTERN)
  expect(reloadedBundle.runtimeRunId).not.toBe(initialBundle.runtimeRunId)
  expect(detailedTraceConsoleMessages).toEqual([])
})

test('enabled diagnostics capture startup after reload and share-link navigation in a new tab', async ({ page, context }) => {
  await page.goto('/')
  const enabled = await page.evaluate(() => ({
    status: window.windshareDiagnostics.enable(),
    bundle: window.windshareDiagnostics.export(),
  }))
  const initialIdentity = parseBundle(enabled.bundle).runtimeRunId

  await page.reload()
  expect(await page.evaluate(() => window.windshareDiagnostics.status())).toMatchObject({
    enabled: true, expires_at: enabled.status.expires_at,
  })
  const reloadedIdentity = parseBundle(
    await page.evaluate(() => window.windshareDiagnostics.export()),
  ).runtimeRunId
  expect(reloadedIdentity).not.toBe(initialIdentity)

  const reopened = await context.newPage()
  await reopened.goto('/')
  expect(await reopened.evaluate(() => window.windshareDiagnostics.status())).toMatchObject({
    enabled: true, expires_at: enabled.status.expires_at,
  })
  // Invalid key material fails during controller startup without external networking.
  await reopened.goto('/AAAAAAAAAAAAAAAA#invalid-key')
  await expect.poll(() => reopened.evaluate(() => window.windshareDiagnostics.status().state))
    .toBe('sealed')
  const restored = await reopened.evaluate(() => ({
    status: window.windshareDiagnostics.status(),
    bundle: window.windshareDiagnostics.export(),
  }))
  expect(restored.status).toMatchObject({ capture_generation: '1', seal_reason: 'scope_terminal' })
  expect(restored.bundle).toContain('"event":"join_transition"')
  expect(restored.bundle).toContain('"transition":"started"')
  expect(parseBundle(restored.bundle).runtimeRunId).not.toBe(reloadedIdentity)

  await reopened.evaluate(() => window.windshareDiagnostics.disable())
  await page.reload()
  expect(await page.evaluate(() => window.windshareDiagnostics.status())).toMatchObject({
    enabled: false, state: 'idle',
  })
})

test('blocked browser storage does not break startup or manual capture', async ({ page }) => {
  await page.addInitScript(() => {
    Object.defineProperty(window, 'localStorage', {
      get() { throw new DOMException('Storage denied', 'SecurityError') },
    })
  })
  await page.goto('/')
  const statuses = await page.evaluate(() => ({
    initial: window.windshareDiagnostics.status(),
    enabled: window.windshareDiagnostics.enable(),
    disabled: window.windshareDiagnostics.disable(),
  }))
  expect(statuses.initial.enabled).toBe(false)
  expect(statuses.enabled.enabled).toBe(true)
  expect(statuses.disabled.enabled).toBe(false)
})

function parseBundle(encoded: string): Readonly<{
  lineTypes: readonly string[]
  runtimeRunId: string
}> {
  const records = encoded.trimEnd().split('\n')
    .filter((line) => line.length > 0)
    .map((line) => JSON.parse(line) as unknown)
  const header = records[0]
  if (!isRecord(header) || header.line_type !== 'bundle_header' ||
      typeof header.runtime_run_id !== 'string') {
    throw new TypeError('Diagnostics export omitted its bundle header identity')
  }
  return Object.freeze({
    lineTypes: Object.freeze(records.map((record) => {
      if (!isRecord(record) || typeof record.line_type !== 'string') {
        throw new TypeError('Diagnostics export contains an invalid line type')
      }
      return record.line_type
    })),
    runtimeRunId: header.runtime_run_id,
  })
}

function isRecord(value: unknown): value is Readonly<Record<string, unknown>> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}
