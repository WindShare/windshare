import { expect, test, type Download } from '@playwright/test'
import type { WindShareDiagnostics } from '../src/diagnostics/export/developer-api'
import {
  PROTOCOL_MESSAGE_KINDS_V1,
  type ProtocolMessageKindV1,
} from '../src/diagnostics/incident'
import {
  Uint8ArrayReader,
  Uint8ArrayWriter,
  ZipReader,
  type FileEntry,
} from '@zip.js/zip.js'

import {
  capabilityUrl,
  DirectProductStack,
} from './fixtures/direct-product-stack'
import {
  createCapabilityRedactor,
  withCapabilityRedaction,
} from './fixtures/capability-redactor'

const DIRECTORY_NAME = 'micro-share'
const FILE_NAME = 'pixel.png'
const FILE_BYTES = Uint8Array.from(Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=',
  'base64',
))
const ARCHIVE_NAME = 'windshare.zip'
const SYNTHETIC_RESULT_ROOT_NAME = 'windshare'
const DOWNLOAD_TIMEOUT_MILLISECONDS = 20_000
const TRACE_CORRELATION_TIMEOUT_MILLISECONDS = 10_000
const BASE64URL_IDENTITY_PATTERN = /^[A-Za-z0-9_-]{22}$/u
const RECONSTRUCTION_HARNESS_PATH = '/test/browser/diagnostics-reconstruction-harness.ts'

test('receives an explicit directory artifact from the real sender and relay', async ({ browserName, page }, testInfo) => {
  const scenarioId = microDirectoryScenarioId(browserName)
  const stack = new DirectProductStack(scenarioId)
  const pageErrors: string[] = []
  const detailedTraceConsoleMessages: string[] = []
  let redactor: ReturnType<typeof createCapabilityRedactor> | undefined
  page.on('pageerror', (error) => pageErrors.push(error.message))
  page.on('console', (message) => {
    const value = message.text()
    if (value.startsWith('windshare.receive')) detailedTraceConsoleMessages.push(value)
  })
  await stack.start()
  try {
    const directory = await stack.createDirectory(DIRECTORY_NAME, [
      { name: FILE_NAME, bytes: FILE_BYTES },
    ])
    const share = await stack.share(directory, { senderTrace: true })

    // The portable path avoids an operating-system picker while still exercising
    // the production UI, output authority, ZIP writer, and browser download.
    await page.addInitScript(() => {
      const reconstructionDirectory = navigator.storage?.getDirectory?.bind(navigator.storage)
      Object.defineProperty(window, '__windshareReconstructionDirectory', {
        configurable: false,
        enumerable: false,
        writable: false,
        value: reconstructionDirectory,
      })
      Object.defineProperties(window, {
        // The ordinary smoke owns the relay product path; weekly hot-switch owns
        // native peer negotiation, so an external STUN dependency cannot delay CI.
        RTCPeerConnection: { configurable: true, value: undefined },
        showDirectoryPicker: { configurable: true, value: undefined },
        showSaveFilePicker: { configurable: true, value: undefined },
      })
      if (navigator.storage !== undefined) {
        Object.defineProperty(navigator.storage, 'getDirectory', {
          configurable: true,
          value: undefined,
        })
      }
    })
    const navigationUrl = capabilityUrl(share)
    redactor = createCapabilityRedactor({
      completeUrl: navigationUrl,
      fragment: new URL(navigationUrl).hash,
      separateKey: share.key,
    })
    await withCapabilityRedaction(() => page.goto(navigationUrl), {
      completeUrl: navigationUrl,
      fragment: new URL(navigationUrl).hash,
      separateKey: share.key,
    })

    const defaultDiagnostics = await page.evaluate(() => {
      const diagnostics: WindShareDiagnostics = window.windshareDiagnostics
      return {
        status: diagnostics.status(),
        bundle: diagnostics.export(),
      }
    })
    expect(defaultDiagnostics.status).toMatchObject({
      state: 'idle',
      enabled: false,
      retained_event_count: '0',
      retained_event_bytes: '0',
    })
    expect(parseNdjson(defaultDiagnostics.bundle).map(lineType)).toEqual(['bundle_header'])
    expect(detailedTraceConsoleMessages).toEqual([])

    // The controller and protocol session already exist: enabling here proves
    // long-lived producers consult the revocable source for the current receive.
    const enabledDiagnostics = await page.evaluate(() => {
      const diagnostics: WindShareDiagnostics = window.windshareDiagnostics
      return diagnostics.enable()
    })
    expect(enabledDiagnostics).toMatchObject({
      state: 'recording_pre_failure',
      enabled: true,
    })

    const browseStatus = page.locator('.status-line')
    await expect(page.getByRole('heading', { name: 'Browse and save shared files' })).toBeVisible()
    await expect(browseStatus).toHaveText('Choose what to receive.')
    await expect(page.getByText(DIRECTORY_NAME, { exact: true })).toBeVisible()
    await page.getByRole('button', { name: 'Open' }).click()
    await expect(browseStatus).toHaveText('Choose what to receive.')
    await expect(page.getByText(FILE_NAME, { exact: true })).toBeVisible()
    await page.getByRole('button', { name: 'Preview' }).click()
    const preview = page.getByRole('img', { name: `Preview of ${FILE_NAME}` })
    await expect(preview).toBeVisible()
    expect(await preview.evaluate(async (image) => {
      const response = await fetch((image as HTMLImageElement).src)
      return [...new Uint8Array(await response.arrayBuffer())]
    })).toEqual([...FILE_BYTES])
    await page.getByRole('button', { name: 'Close preview' }).click()
    const artifactAction = page.getByRole('button', { name: 'Check then download' })
    await expect(artifactAction).toBeVisible()
    await expect(page.getByText(
      'Checks that the complete ZIP fits before receiving any file content. The browser takes over when the package is ready.',
      { exact: true },
    )).toBeVisible()
    await expect.poll(() => new URL(page.url()).hash).toBe('')

    const downloadStarted = page.waitForEvent('download', {
      timeout: DOWNLOAD_TIMEOUT_MILLISECONDS,
    })
    await artifactAction.click()
    const download = await downloadStarted
    await expect(page.getByText('Download started', { exact: true })).toBeVisible({
      timeout: DOWNLOAD_TIMEOUT_MILLISECONDS,
    })
    await expect(page.getByText(
      'The browser took over the download. WindShare cannot confirm where or whether it was saved.',
      { exact: true },
    )).toBeVisible()
    await expect(page.getByText('Ready to save', { exact: true })).toHaveCount(0)
    await expect(page.getByText('Saved', { exact: true })).toHaveCount(0)
    await expect(page.getByText(/1 file\(s\), .* total/u)).toBeVisible()
    await assertDirectoryDownload(download)

    const browserDiagnostics = await page.evaluate(() => {
      const diagnostics: WindShareDiagnostics = window.windshareDiagnostics
      return {
        status: diagnostics.disable(),
        bundle: diagnostics.export(),
      }
    })
    const browserRecords = parseNdjson(browserDiagnostics.bundle)
    expect(browserDiagnostics.status).toMatchObject({
      state: 'sealed',
      enabled: false,
      seal_reason: 'manual_disable',
    })
    expect(BigInt(browserDiagnostics.status.retained_event_count)).toBeGreaterThan(0n)
    expect(browserRecords.map(lineType)).toContain('trace_capture')
    expect(browserRecords.map(lineType)).toContain('trace_event')

    await expect.poll(async () => {
      const senderRecords = await stack.senderTraceRecords(share)
      return findSharedProtocolCorrelation(browserRecords, senderRecords) !== null
    }, {
      message: 'browser and traced sender should expose one shared protocol correlation',
      timeout: TRACE_CORRELATION_TIMEOUT_MILLISECONDS,
    }).toBe(true)
    const joined = findSharedProtocolCorrelation(
      browserRecords,
      await stack.senderTraceRecords(share),
    )
    if (joined === null) throw new Error('Shared protocol correlation disappeared after observation')
    expect(joined.correlation.protocolSessionId).toMatch(BASE64URL_IDENTITY_PATTERN)
    expect(joined.correlation.protocolOperationId).toMatch(BASE64URL_IDENTITY_PATTERN)
    expect(joined.browser).toMatchObject({ schemaVersion: 1, event: 'protocol_operation' })
    expect(joined.sender).toMatchObject({
      schemaVersion: 3,
      event: 'protocol_operation',
      command: 'share',
    })
    // Runtime identities are local evidence boundaries, never a cross-process join key.
    expect(joined.browser.runtimeRunId).not.toBe(joined.sender.runtimeRunId)
    expect(detailedTraceConsoleMessages).toEqual([])

    const reconstructionStorageAvailable = await page.evaluate(() =>
      typeof (window as typeof window & Readonly<{
        __windshareReconstructionDirectory?: unknown
      }>).__windshareReconstructionDirectory === 'function')
    const reconstructionAttempt = await page.evaluate(async ({ path, correlation }) => {
      const harness = await import(path) as typeof import(
        '../test/browser/diagnostics-reconstruction-harness'
      )
      return harness.reconstructFSAContinuationFailure(correlation)
    }, {
      path: RECONSTRUCTION_HARNESS_PATH,
      correlation: {
        protocolSessionId: joined.correlation.protocolSessionId,
        protocolOperationId: joined.correlation.protocolOperationId,
        requestKind: joined.browser.requestKind,
      },
    })
    if (reconstructionAttempt.kind === 'unavailable') {
      expect(reconstructionStorageAvailable).toBe(false)
      expect(reconstructionAttempt.reason).toBe('origin-private-storage-unavailable')
      await testInfo.attach('fsa-continuation-reconstruction-capability', {
        body: JSON.stringify(reconstructionAttempt),
        contentType: 'application/json',
      })
      return
    }
    expect(reconstructionStorageAvailable).toBe(true)
    const reconstruction = reconstructionAttempt.result
    const reconstructionRecords = parseNdjson(reconstruction.bundle)
    const senderArtifact = await stack.senderTraceArtifact(share)
    const senderRecords = parseNdjson(senderArtifact)
    const reconstructionJoin = findSharedProtocolCorrelation(
      reconstructionRecords,
      senderRecords,
    )
    expect(reconstructionJoin).not.toBeNull()
    expect(reconstructionJoin?.browser.runtimeRunId)
      .not.toBe(reconstructionJoin?.sender.runtimeRunId)
    expect(reconstruction).toMatchObject({
      incidentConsoleCount: 1,
      actionErrorName: 'AggregateError',
      restoredLifecycle: 'resumable-receive',
    })
    assertFSAReconstruction(reconstructionRecords)
    for (const forbidden of [
      ...reconstruction.forbiddenSentinels,
      navigationUrl,
      share.key,
      DIRECTORY_NAME,
      FILE_NAME,
    ]) {
      expect(reconstruction.bundle).not.toContain(forbidden)
      expect(senderArtifact).not.toContain(forbidden)
    }
    expect(reconstruction.bundle).not.toMatch(/"(?:user_agent|platform|path|handle|message|stack)"/u)
    await testInfo.attach('fsa-continuation-browser-bundle', {
      body: reconstruction.bundle,
      contentType: 'application/x-ndjson',
    })
    await testInfo.attach('fsa-continuation-go-sender-trace', {
      body: senderArtifact,
      contentType: 'application/x-ndjson',
    })
  } catch (error) {
    const pageDiagnostic = await page.evaluate(() => ({
      status: document.querySelector('[role="status"]')?.textContent ?? null,
      error: document.querySelector('[role="alert"]')?.textContent ?? null,
    })).catch(() => ({ status: null, error: null }))
    await testInfo.attach('direct-stack-diagnostic', {
      body: redactor?.text({
        component: 'browser-direct-smoke',
        scenarioId,
        milestone: 'failed',
        pageErrors,
        ...pageDiagnostic,
        processes: stack.diagnostic(),
      }) ?? JSON.stringify({
        component: 'browser-direct-smoke',
        scenarioId,
        milestone: 'failed',
        pageErrors,
        ...pageDiagnostic,
        processes: stack.diagnostic(),
      }),
      contentType: 'application/json',
    }).catch(() => undefined)
    const message = error instanceof Error ? error.message : String(error)
    // Attach only the recursively redacted snapshot; Playwright must never
    // retain the original capability-bearing assertion as an Error.cause.
    throw new Error(redactor?.redactText(message) ?? message, {
      // eslint-disable-next-line preserve-caught-error -- safe cause is the only permitted boundary value
      cause: redactor?.value(error),
    })
  } finally {
    try {
      await stack.dispose()
    } finally {
      redactor?.clear()
    }
  }
})

interface ProtocolCorrelationView {
  readonly protocolSessionId: string
  readonly protocolOperationId: string
}

interface ProtocolEnvelopeView {
  readonly schemaVersion: number
  readonly event: 'protocol_operation'
  readonly runtimeRunId: string
  readonly command?: string
  readonly requestKind: ProtocolMessageKindV1
  readonly correlation: ProtocolCorrelationView
}

interface SharedProtocolCorrelation {
  readonly correlation: ProtocolCorrelationView
  readonly browser: ProtocolEnvelopeView
  readonly sender: ProtocolEnvelopeView
}

function parseNdjson(encoded: string): readonly unknown[] {
  return Object.freeze(
    encoded.trimEnd().split('\n')
      .filter((line) => line.length > 0)
      .map((line): unknown => JSON.parse(line) as unknown),
  )
}

function lineType(value: unknown): string {
  if (!isRecord(value) || typeof value.line_type !== 'string') {
    throw new TypeError('Diagnostic bundle contains an invalid line type')
  }
  return value.line_type
}

function findSharedProtocolCorrelation(
  browserRecords: readonly unknown[],
  senderRecords: readonly unknown[],
): SharedProtocolCorrelation | null {
  const browserOperations = browserRecords
    .map(protocolEnvelope)
    .filter((record): record is ProtocolEnvelopeView => record !== null)
  const senderOperations = senderRecords
    .map(protocolEnvelope)
    .filter((record): record is ProtocolEnvelopeView => record !== null)
  for (const browser of browserOperations) {
    const sender = senderOperations.find((candidate) =>
      candidate.correlation.protocolSessionId === browser.correlation.protocolSessionId &&
      candidate.correlation.protocolOperationId === browser.correlation.protocolOperationId &&
      candidate.requestKind === browser.requestKind)
    if (sender !== undefined) {
      return Object.freeze({
        correlation: browser.correlation,
        browser,
        sender,
      })
    }
  }
  return null
}

function protocolEnvelope(value: unknown): ProtocolEnvelopeView | null {
  const candidate = isRecord(value) && value.line_type === 'trace_event'
    ? value.record
    : value
  if (!isRecord(candidate) ||
      candidate.event !== 'protocol_operation' ||
      typeof candidate.schema_version !== 'number' ||
      typeof candidate.runtime_run_id !== 'string' ||
      !isRecord(candidate.correlation) ||
      typeof candidate.correlation.protocol_session_id !== 'string' ||
      typeof candidate.correlation.protocol_operation_id !== 'string' ||
      !isRecord(candidate.payload) ||
      !isProtocolMessageKind(candidate.payload.request_kind) ||
      (candidate.command !== undefined && typeof candidate.command !== 'string')) {
    return null
  }
  return Object.freeze({
    schemaVersion: candidate.schema_version,
    event: 'protocol_operation',
    runtimeRunId: candidate.runtime_run_id,
    ...(candidate.command === undefined ? {} : { command: candidate.command }),
    requestKind: candidate.payload.request_kind,
    correlation: Object.freeze({
      protocolSessionId: candidate.correlation.protocol_session_id,
      protocolOperationId: candidate.correlation.protocol_operation_id,
    }),
  })
}

function assertFSAReconstruction(records: readonly unknown[]): void {
  const zeroHealth = {
    fact_overflow_count: '0',
    incident_history_eviction_count: '0',
    console_suppression_count: '0',
    late_link_eviction_count: '0',
    trace_dropped_count: '0',
    trace_overwritten_count: '0',
    trace_sampled_count: '0',
    trace_coalesced_count: '0',
  }
  const header = records.find((record) =>
    isRecord(record) && record.line_type === 'bundle_header')
  expect(header).toMatchObject({ diagnostics_health_at_export: zeroHealth })
  const incidentLine = records.find((record) =>
    isRecord(record) && record.line_type === 'incident')
  if (!isRecord(incidentLine) || !isRecord(incidentLine.record)) {
    throw new TypeError('FSA reconstruction omitted its incident')
  }
  expect(incidentLine.record).toMatchObject({
    event: 'failure_incident',
    payload: {
      presentation: { boundary: 'retained_action', outcome: 'failed' },
      trigger: {
        kind: 'native_output_failure',
        stage: 'reopen',
      },
      contributors: [{
        count: '1',
        representative: {
          kind: 'native_output_failure',
          stage: 'continuation',
          recovery_disposition: 'resumable_receive',
        },
      }],
      consequences: [{
        count: '1',
        representative: {
          kind: 'native_output_failure',
          stage: 'cleanup',
          recovery_disposition: 'needs_attention',
        },
      }],
      fact_count: '3',
      overflow_fact_count: '0',
      diagnostics_health_at_seal: zeroHealth,
      context: {
        controller: { phase: 'browsing' },
        lifecycle: { state: 'resumable_receive' },
        progress: { file_errors: '1' },
        output: { plan_kind: 'direct_tree' },
      },
    },
  })

  const trace = records
    .filter((record) => isRecord(record) && record.line_type === 'trace_event')
    .map((line) => (line as Readonly<Record<string, unknown>>).record)
    .filter(isRecord)
  const eventIndex = (
    event: string,
    payload: Readonly<Record<string, unknown>>,
  ): number => trace.findIndex((record) => {
    const observedPayload = record.payload
    return record.event === event && isRecord(observedPayload) &&
      Object.entries(payload).every(([key, value]) => observedPayload[key] === value)
  })
  const protocolIndex = eventIndex('protocol_operation', {})
  const reopenIndex = eventIndex('reopen', {
    backend: 'file_system_access',
    transition: 'failed',
  })
  const continuationIndex = eventIndex('continuation', {
    backend: 'file_system_access',
    transition: 'admission_failed',
  })
  const cleanupIndex = eventIndex('cleanup', {
    backend: 'file_system_access',
    transition: 'failed',
  })
  const actionIndex = eventIndex('retained_action', { transition: 'failed' })
  expect(protocolIndex).toBeGreaterThanOrEqual(0)
  expect(reopenIndex).toBeGreaterThan(protocolIndex)
  expect(continuationIndex).toBeGreaterThan(reopenIndex)
  expect(cleanupIndex).toBeGreaterThan(continuationIndex)
  expect(actionIndex).toBeGreaterThan(cleanupIndex)
}

function isProtocolMessageKind(value: unknown): value is ProtocolMessageKindV1 {
  return typeof value === 'string' &&
    PROTOCOL_MESSAGE_KINDS_V1.includes(value as ProtocolMessageKindV1)
}

function isRecord(value: unknown): value is Readonly<Record<string, unknown>> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function microDirectoryScenarioId(browserName: string): string {
  if (browserName !== 'chromium' && browserName !== 'firefox' && browserName !== 'webkit') {
    throw new TypeError(`Unsupported browser engine for direct smoke: ${browserName}`)
  }
  return `${browserName}-micro-directory`
}

async function assertDirectoryDownload(download: Download): Promise<void> {
  expect(download.suggestedFilename()).toBe(ARCHIVE_NAME)
  const reader = new ZipReader(new Uint8ArrayReader(await readDownload(download)))
  try {
    const entries = await reader.getEntries()
    const file = entries.find(
      (entry): entry is FileEntry =>
        'getData' in entry && entry.filename ===
          `${SYNTHETIC_RESULT_ROOT_NAME}/${DIRECTORY_NAME}/${FILE_NAME}`,
    )
    if (file === undefined) {
      throw new Error('Downloaded directory archive is missing the expected file')
    }
    expect(await file.getData(new Uint8ArrayWriter())).toEqual(FILE_BYTES)
  } finally {
    await reader.close()
  }
}

async function readDownload(download: Download): Promise<Uint8Array> {
  const stream = await download.createReadStream()
  if (stream === null) throw new Error('Playwright download stream is unavailable')
  const chunks: Buffer[] = []
  for await (const chunk of stream) chunks.push(Buffer.from(chunk))
  return Buffer.concat(chunks)
}
