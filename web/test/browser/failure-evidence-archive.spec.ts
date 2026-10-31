import { expect, test } from '@playwright/test'
import type { BrowserDiagnosticsSession } from '../../src/diagnostics/browser/session'

declare global { interface Window { failureEvidenceSession: BrowserDiagnosticsSession } }

test('trace-off failure snapshots save automatically and survive a real page reload', async ({ page }) => {
  await page.goto('/test/browser/contract-host.html')
  const runId = await page.evaluate(async () => {
    const compositionPath = '/src/application/diagnostics/composition.ts'
    const sessionPath = '/src/diagnostics/browser/session.ts'
    const archivePath = '/src/diagnostics/browser/indexeddb-archive.ts'
    const factsPath = '/src/diagnostics/incident/fact.ts'
    const { createBrowserDiagnosticsComposition } = await import(/* @vite-ignore */ compositionPath) as typeof import('../../src/application/diagnostics/composition')
    const { BrowserDiagnosticsSession } = await import(/* @vite-ignore */ sessionPath) as typeof import('../../src/diagnostics/browser/session')
    const { IndexedDBDiagnosticsArchive } = await import(/* @vite-ignore */ archivePath) as typeof import('../../src/diagnostics/browser/indexeddb-archive')
    const { unclassifiedFailureFact } = await import(/* @vite-ignore */ factsPath) as typeof import('../../src/diagnostics/incident/fact')
    const diagnostics = createBrowserDiagnosticsComposition({ build: { version: '0.0.0', mode: 'test' }, secureContext: true,
      consoleSink: { error: () => { throw new Error('Silent evidence must not notify') } },
    })
    window.failureEvidenceSession = new BrowserDiagnosticsSession({ runtime: diagnostics.runtime,
      observeCapture: diagnostics.observeEvidence, archive: new IndexedDBDiagnosticsArchive(() => indexedDB), pageUrl: location.href,
    })
    diagnostics.failures.record(unclassifiedFailureFact({ stage: 'protocol_operation', recoveryDisposition: 'none',
      error: new Error('recovered lane failure', { cause: new DOMException('socket reset', 'NetworkError') }),
      operation: 'session.read', context: { lane: 2 },
    }))
    return diagnostics.runtime.runtimeRunId
  })
  await expect.poll(() => page.evaluate(() => window.failureEvidenceSession.getSnapshot().savedAt)).not.toBeNull()
  expect(await page.evaluate(() => ({ status: window.failureEvidenceSession.status().state,
    incident: window.failureEvidenceSession.inspectLastFailure() }))).toEqual({ status: 'idle', incident: null })
  await page.reload()
  const saved = await page.evaluate(async runId => {
    const archivePath = '/src/diagnostics/browser/indexeddb-archive.ts'
    const { IndexedDBDiagnosticsArchive } = await import(/* @vite-ignore */ archivePath) as typeof import('../../src/diagnostics/browser/indexeddb-archive')
    const archive = new IndexedDBDiagnosticsArchive(() => indexedDB)
    const summaries = await archive.list()
    const summary = summaries.find(row => row.id === runId + '.0')!
    const file = await archive.readFile(summary.id)
    return { summary, text: file?.text }
  }, runId)
  expect(saved.summary.captureState).toBe('idle')
  expect(saved.text).toContain('recovered lane failure')
  expect(saved.text).toContain('socket reset')
  expect(saved.text).toContain('"line_type":"failure_evidence"')
  expect(saved.text).not.toContain('"line_type":"trace_event"')
})
