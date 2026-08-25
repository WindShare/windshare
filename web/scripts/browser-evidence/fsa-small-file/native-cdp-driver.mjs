import { mkdirSync, writeFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { pathToFileURL } from 'node:url'

const options = parseArguments(process.argv.slice(2))
const clientUrl = pathToFileURL(resolve(options.replayRoot, 'src/cdp-client.mjs')).href
const { CdpClient, delay, evaluate } = await import(clientUrl)

function parseArguments(argv) {
  const values = {
    endpoint: null,
    url: null,
    replayRoot: null,
    mode: null,
    trigger: null,
    evidence: null,
    screenshot: null,
    timeoutMilliseconds: 180_000,
  }
  for (let index = 0; index < argv.length; index += 1) {
    const argument = argv[index]
    if (argument === '--endpoint') values.endpoint = argv[++index]
    else if (argument === '--url') values.url = argv[++index]
    else if (argument === '--replay-root') values.replayRoot = argv[++index]
    else if (argument === '--mode') values.mode = argv[++index]
    else if (argument === '--trigger') values.trigger = argv[++index]
    else if (argument === '--evidence') values.evidence = argv[++index]
    else if (argument === '--screenshot') values.screenshot = argv[++index]
    else if (argument === '--timeout-ms') values.timeoutMilliseconds = Number.parseInt(argv[++index], 10)
    else throw new Error(`Unknown argument: ${argument}`)
  }
  for (const name of ['endpoint', 'url', 'replayRoot', 'mode', 'trigger', 'evidence']) {
    if (!values[name]) throw new Error(`Missing required --${name}`)
  }
  if (values.mode !== 'baseline' && values.mode !== 'product') throw new Error('--mode must be baseline or product')
  return values
}

function writeJson(path, value) {
  mkdirSync(dirname(path), { recursive: true })
  writeFileSync(path, `${JSON.stringify(value, null, 2)}\n`, { encoding: 'utf8', flag: 'wx' })
}

async function openTarget(endpoint, url) {
  const response = await fetch(`${endpoint.replace(/\/$/, '')}/json/new?${encodeURIComponent(url)}`, {
    method: 'PUT',
    signal: AbortSignal.timeout(5_000),
  })
  if (!response.ok) throw new Error(`CDP target creation failed: ${response.status}`)
  const target = await response.json()
  if (target.type !== 'page' || typeof target.webSocketDebuggerUrl !== 'string') {
    throw new Error('CDP target creation returned an invalid page target')
  }
  return target
}

async function waitFor(client, expression, label) {
  const deadline = Date.now() + options.timeoutMilliseconds
  let last
  while (Date.now() < deadline) {
    last = await evaluate(client, expression)
    if (last) return last
    await delay(100)
  }
  throw new Error(`Timed out waiting for ${label}; last=${JSON.stringify(last)}`)
}

async function click(client, expression, label) {
  const rectangle = await waitFor(client, expression, label)
  const x = rectangle.x + rectangle.width / 2
  const y = rectangle.y + rectangle.height / 2
  await client.call('Input.dispatchMouseEvent', { type: 'mouseMoved', x, y })
  await client.call('Input.dispatchMouseEvent', { type: 'mousePressed', x, y, button: 'left', buttons: 1, clickCount: 1 })
  await client.call('Input.dispatchMouseEvent', { type: 'mouseReleased', x, y, button: 'left', buttons: 0, clickCount: 1 })
  return { x, y, rectangle }
}

function parseDiagnostics(bundle) {
  const records = bundle.trimEnd().split('\n').filter(Boolean).map(line => JSON.parse(line))
  const header = records.find(record => record?.line_type === 'bundle_header')
  const summaries = records.filter(record =>
    record?.line_type === 'trace_event' && record?.record?.event === 'performance_summary')
  if (summaries.length !== 1) throw new Error(`Expected one performance summary, observed ${summaries.length}`)
  return { header, summary: summaries[0].record.payload, recordCount: records.length }
}

const target = await openTarget(options.endpoint, options.url)
let client
try {
  client = await CdpClient.connect(target.webSocketDebuggerUrl)
  await client.call('Page.enable')
  await client.call('Runtime.enable')
  await client.call('Page.bringToFront')
  await evaluate(client, `(() => {
    const token = 'Browser Native UI Replay - ';
    if (!document.title.startsWith(token)) document.title = token + document.title;
    return document.title;
  })()`)
  let clickEvidence
  if (options.mode === 'baseline') {
    await waitFor(client, 'globalThis.__windShareFsaEvidenceReady === true', 'baseline readiness')
    clickEvidence = await click(client, `(() => {
      const button = document.querySelector('#run-evidence');
      if (!(button instanceof HTMLButtonElement) || button.disabled) return null;
      const rect = button.getBoundingClientRect();
      return rect.width > 0 && rect.height > 0
        ? { x: rect.x, y: rect.y, width: rect.width, height: rect.height }
        : null;
    })()`, 'baseline action')
  } else {
    await waitFor(client, `(() => {
      if (typeof globalThis.windshareDiagnostics?.enable !== 'function') return false;
      const heading = document.querySelector('h1');
      return heading?.textContent?.trim() === 'Browse and save shared files';
    })()`, 'product receiver readiness')
    await evaluate(client, 'globalThis.windshareDiagnostics.enable()')
    clickEvidence = await click(client, `(() => {
      const buttons = [...document.querySelectorAll('button')];
      const matches = buttons.filter(button =>
        button.textContent?.trim() === 'Save using original folder hierarchy' && !button.disabled);
      if (matches.length !== 1) return null;
      const rect = matches[0].getBoundingClientRect();
      return rect.width > 0 && rect.height > 0
        ? { x: rect.x, y: rect.y, width: rect.width, height: rect.height }
        : null;
    })()`, 'DirectTree product action')
  }
  writeJson(options.trigger, {
    schema: 'windshare/fsa-small-file-native-trigger/v1',
    mode: options.mode,
    triggeredAt: new Date().toISOString(),
    targetId: target.id,
    url: options.url,
    click: clickEvidence,
  })

  let result
  if (options.mode === 'baseline') {
    result = await waitFor(
      client,
      'globalThis.__windShareFsaEvidenceResult ?? null',
      'pure-FSA completion',
    )
    if (result.ok !== true) throw new Error(`Baseline page failed: ${JSON.stringify(result.error)}`)
  } else {
    const bundle = await waitFor(client, `(() => {
      const diagnostics = globalThis.windshareDiagnostics;
      if (typeof diagnostics?.export !== 'function') return null;
      const body = diagnostics.export();
      return body.includes('"event":"performance_summary"') ? body : null;
    })()`, 'Published performance summary')
    const parsed = parseDiagnostics(bundle)
    const milestones = parsed.summary.milestones
    if (milestones?.authority_acquired === null || milestones?.published === null) {
      throw new Error('Product summary is missing authority or Published milestones')
    }
    result = {
      schema: 'windshare/fsa-small-file-native-product-raw/v1',
      ok: true,
      capturedAt: new Date().toISOString(),
      diagnostics: parsed,
      lifecycle: 'Published',
      publicationInvariants: {
        activeNativeWork: 0,
        candidateCheckpoints: 0,
        basis: 'Published requires the terminal scheduler cut and sealed ledger candidate check',
      },
    }
  }
  writeJson(options.evidence, {
    schema: 'windshare/fsa-small-file-native-cdp-evidence/v1',
    capturedAt: new Date().toISOString(),
    mode: options.mode,
    target: { id: target.id, url: options.url, title: await evaluate(client, 'document.title') },
    result,
  })
  if (options.screenshot) {
    const screenshot = await client.call('Page.captureScreenshot', { format: 'png', fromSurface: true })
    mkdirSync(dirname(options.screenshot), { recursive: true })
    writeFileSync(options.screenshot, Buffer.from(screenshot.data, 'base64'), { flag: 'wx' })
  }
} catch (error) {
  let page = null
  try {
    page = await evaluate(client, `(() => ({
      title: document.title,
      bodyText: document.body?.innerText?.slice(0, 16_384) ?? '',
      diagnosticsStatus: typeof globalThis.windshareDiagnostics?.status === 'function'
        ? globalThis.windshareDiagnostics.status()
        : null,
    }))()`)
  } catch {}
  writeJson(options.evidence, {
    schema: 'windshare/fsa-small-file-native-cdp-evidence/v1',
    capturedAt: new Date().toISOString(),
    mode: options.mode,
    target: { id: target.id, url: options.url },
    result: {
      ok: false,
      error: error instanceof Error
        ? { name: error.name, message: error.message, stack: error.stack ?? null }
        : { name: 'Error', message: String(error), stack: null },
      page,
    },
  })
  process.exitCode = 2
} finally {
  try { await client?.call('Target.closeTarget', { targetId: target.id }) } catch {}
  client?.close()
}
