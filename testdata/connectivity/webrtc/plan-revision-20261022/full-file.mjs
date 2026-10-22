import assert from 'node:assert/strict'
import { createHash, randomBytes } from 'node:crypto'
import { mkdir, writeFile } from 'node:fs/promises'
import { fileURLToPath } from 'node:url'
import { join } from 'node:path'
import playwright from '@playwright/test'
import { startLinuxBrowser, lab } from './linux-browser.mjs'
import { DirectProductStack } from '../../../../web/e2e/fixtures/direct-product-stack'

const variant = process.argv[2] ?? 'baseline'
const sizeMiB = Number(process.argv[3] ?? 256)
const repeats = Number(process.argv[4] ?? 3)
const slowWriteMs = Number(process.argv[5] ?? 0)
const interaction = process.argv[6] ?? 'none'
const blockMiB = Number(process.argv[7] ?? 1)
const topology = process.argv[8] ?? 'loopback'
assert(['loopback', 'lan', 'lan-isolated'].includes(topology))
const remoteBrowser = topology !== 'loopback'
assert(['none', 'preview', 'cancel-preview'].includes(interaction))
assert(blockMiB === 1 || blockMiB === 4)
assert(['baseline', 'batch'].includes(variant))
assert(sizeMiB > 0 && sizeMiB <= 512 && repeats > 0 && repeats <= 5 && slowWriteMs >= 0)
const runId = variant + '-' + sizeMiB + '-' + slowWriteMs + '-' + interaction + '-' + blockMiB + '-' + topology + '-' + Date.now()
const root = fileURLToPath(new URL('../../../../', import.meta.url))
const output = join(root, '.tmp/webrtc-plan-revision-20261022', runId)
await mkdir(output, { recursive: true })
process.env.WINDSHARE_BROWSER_BINARY_DIRECTORY = join(root, '.tmp/webrtc-plan-revision-20261022', variant)
process.env.WS_PROBE_BATCH = variant === 'batch' ? '64' : '0'
const stack = new DirectProductStack('plan-' + runId.replaceAll('.', 'p'))
const report = { runId, variant, sizeMiB, slowWriteMs, interaction, blockMiB, topology, scope: 'Production CLI file read/seal, protocol, browser authentication and SingleFileStreamOutputSession to OPFS. Persistent browser profile; production p2p-only policy; existing test plan-settlement adapter. Connection/catalog setup and post-close SHA-256 excluded from throughput. Not UI, crash durability, resume or WAN evidence.', rounds: [] }
const fixture = randomBytes(sizeMiB * 1024 * 1024)
const expectedHash = createHash('sha256').update(fixture).digest('hex')
let browser, persistentContext, linux
try {
  await stack.start()
  const image = Buffer.alloc(4 * 1024 * 1024)
  Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=', 'base64').copy(image)
  const file = interaction === 'none' ? await stack.createFile('probe.bin', fixture)
    : await stack.createDirectory('interactive', [{ name: 'probe.bin', bytes: fixture }, { name: 'preview.png', bytes: image }])
  if (topology === 'loopback') {
    persistentContext = await playwright.chromium.launchPersistentContext(join(output, 'profile'), { channel: 'msedge', headless: true, permissions: ['local-network-access'] })
    browser = persistentContext.browser()
  }
  for (let n = 0; n < repeats; n++) {
    const share = await stack.share(file, { blockSizeBytes: blockMiB * 1024 * 1024, senderTrace: true })
    if (remoteBrowser && !linux) {
      const relays = new URL(share.bareLink).searchParams.getAll('r').map(url => new URL(url).port)
      linux = await startLinuxBrowser([new URL(stack.baseURL).port, ...relays], report)
      browser = linux.browser
      persistentContext = linux.context
      await persistentContext.grantPermissions(['local-network-access', 'microphone'])
      await linux.tcpControl('before')
    }
    report.browserVersion = browser.version()
    const context = persistentContext
    const page = await context.newPage()
    const errors = []
    page.on('pageerror', error => errors.push(error.message))
    page.on('console', event => { if (event.type() === 'error') console.error(event.text()) })
    page.on('websocket', socket => socket.on('socketerror', error => console.error('WebSocket:', error)))
    // Reuse the existing product-test binding without editing production or
    // duplicating its intent, discovery and output-settlement contracts.
    await page.route('**/e2e/fixtures/hot-switch-page-runtime.ts', async route => {
      const response = await route.fetch()
      const source = await response.text()
      assert(source.includes('async function bindDirectAtomicOperation('))
      await route.fulfill({ response, body: source + '\nexport { bindDirectAtomicOperation, loadProductModules };\n' })
    })
    const link = new URL(share.bareLink)
    await page.route(link.href, route => route.fulfill({ contentType: 'text/html', body: '<!doctype html><title>WindShare file probe</title>' }))
    await page.goto(link.href)
    if (remoteBrowser && n === 0) report.outputControl = await page.evaluate(async () => {
      const root = await navigator.storage.getDirectory()
      const handle = await root.getFileHandle('sink-control.bin', { create: true })
      const output = await handle.createWritable()
      const block = new Uint8Array(1024 * 1024).fill(31)
      const started = performance.now()
      for (let index = 0; index < 64; index++) await output.write(block)
      await output.close()
      const elapsedMs = performance.now() - started
      const size = (await handle.getFile()).size
      await root.removeEntry('sink-control.bin')
      return { bytes: size, elapsedMs, MBps: size / elapsedMs / 1000 }
    })
    const cdp = await context.newCDPSession(page)
    await cdp.send('Performance.enable')
    const metricsBefore = await cdp.send('Performance.getMetrics')
    try {
      const task = page.evaluate(async ({ key, bytes, slowWriteMs, interaction, topology, receiverHost, senderHost }) => {
        const { bindDirectAtomicOperation, loadProductModules } = await import('/e2e/fixtures/hot-switch-page-runtime.ts')
        const { DirectAtomicStreamOutput } = await import('/e2e/fixtures/hot-switch-page-evidence.ts')
        const modules = await loadProductModules()
        const peers = [], OriginalPeer = window.RTCPeerConnection
        // Isolate the LAN benchmark from the machine's VPN without changing its
        // network settings. Default-path runs remain separate product evidence.
        const isolated = topology === 'lan-isolated'
        const isCandidate = (candidate, address) => {
          if (!candidate) return true
          const fields = candidate.trim().replace(/^a=/, '').split(/\s+/)
          return fields[2]?.toLowerCase() === 'udp' && fields[4] === address && fields[7] === 'host'
        }
        const filterSDP = (description, address) => ({ type: description.type,
          sdp: description.sdp?.split('\r\n').filter(line => !line.startsWith('a=candidate:') || isCandidate(line, address)).join('\r\n') })
        window.RTCPeerConnection = class extends OriginalPeer {
          constructor(configuration) {
            super(isolated ? { ...configuration, iceServers: [] } : configuration)
            peers.push(this)
            if (isolated) this.addEventListener('icecandidate', event => {
              if (event.candidate && !isCandidate(event.candidate.candidate, receiverHost)) event.stopImmediatePropagation()
            })
          }
          get localDescription() { const description = super.localDescription; return isolated && description ? filterSDP(description, receiverHost) : description }
          setRemoteDescription(description) { return super.setRemoteDescription(isolated ? filterSDP(description, senderHost) : description) }
          addIceCandidate(candidate) { return isolated && !isCandidate(candidate?.candidate, senderHost) ? Promise.resolve() : super.addIceCandidate(candidate) }
        }
        const selectedPair = entries => {
          const transport = entries.find(row => row.type === 'transport' && row.selectedCandidatePairId)
          const pair = entries.find(row => row.id === transport?.selectedCandidatePairId)
          return { pair, local: entries.find(row => row.id === pair?.localCandidateId), remote: entries.find(row => row.id === pair?.remoteCandidateId) }
        }
        const requireIsolatedLAN = entries => {
          const { pair, local, remote } = selectedPair(entries)
          if (local?.address !== receiverHost || remote?.address !== senderHost || local.protocol !== 'udp' || local.candidateType !== 'host' || remote.candidateType !== 'host') {
            throw new Error('Selected LAN topology mismatch: ' + JSON.stringify({ pair, local, remote }))
          }
        }
        const admissions = [], dispatches = {}, traces = []
        window.__planProbe = { phase: 'join', admissions, dispatches, traces }
        const observe = event => { traces.push(event); if (traces.length > 512) traces.shift() }
        const gateway = new modules.gateway.V2BrowserReceiverGateway({
          policy: 'p2p-only', nativePeerUsable: () => true,
          protocolTrace: { current: observe },
          offersFactory: () => new modules.offer.BrowserOfferChannelFactory({ configuration: { iceServers: [] } }),
          onContentLaneAdmitted: observation => admissions.push(observation),
          onBlockDispatched: observation => { dispatches[observation.route] = (dispatches[observation.route] ?? 0) + 1 },
        })
        const setupStarted = performance.now()
        const joined = await gateway.join(key, window.location.href)
        let activation, disk
        try {
          const root = await navigator.storage.getDirectory()
          const handle = await root.getFileHandle('received.bin', { create: true })
          disk = await handle.createWritable()
          let firstWrite, closed, writes = 0, outputBytes = 0, outputWriteMs = 0, previewTask
          const interactive = {}
          let previewEntry
          if (interaction !== 'none') {
            let directory = joined.rootDirectory()
            let catalog = await joined.page(directory, 0)
            while (catalog.entries.length === 1 && catalog.entries[0].kind === 'directory') {
              directory = joined.childDirectory(directory, catalog.entries[0])
              catalog = await joined.page(directory, 0)
            }
            previewEntry = catalog.entries.find(entry => entry.name === 'preview.png')
            if (!previewEntry) throw new Error('Preview fixture is missing')
            joined.selectOnlyFile(catalog.entries.find(entry => entry.name === 'probe.bin'), directory.ancestry)
          }
          const startPreview = () => {
            const controller = new AbortController()
            const previewActivation = joined.beginPreviewConnectivity()
            interactive.startedMs = performance.now()
            const task = joined.preview(previewEntry, previewActivation, controller.signal)
              .then(async preview => {
                interactive.result = preview.current.kind
                interactive.completedMs = performance.now()
                await preview.close()
              }, error => {
                interactive.result = controller.signal.aborted ? 'cancelled' : 'failed'
                interactive.error = String(error)
                interactive.completedMs = performance.now()
              }).finally(() => previewActivation.close())
            if (interaction === 'cancel-preview') setTimeout(() => {
              interactive.abortRequestedMs = performance.now()
              controller.abort(new DOMException('Research preview cancellation', 'AbortError'))
            }, 100)
            return task
          }
          const delivery = {
            outputSession: (stream, id) => new DirectAtomicStreamOutput(
              new stream.SingleFileStreamOutputSession(id, new WritableStream({
                write: async chunk => {
                  firstWrite ??= performance.now()
                  if (interaction !== 'none' && writes === 0) {
                    await new Promise(resolve => setTimeout(resolve, 100))
                    previewTask = startPreview()
                    await new Promise(resolve => setTimeout(resolve, 1000))
                    interactive.outputReleasedMs = performance.now()
                  }
                  if (slowWriteMs > 0) await new Promise(resolve => setTimeout(resolve, slowWriteMs))
                  const writeStarted = performance.now()
                  await disk.write(chunk)
                  outputWriteMs += performance.now() - writeStarted
                  writes++; outputBytes += chunk.byteLength
                },
                close: async () => { await disk.close(); closed = performance.now() },
                abort: reason => disk.abort(reason),
              }))),
          }
          const operation = await bindDirectAtomicOperation({ transferBytes: bytes }, joined, modules, delivery, {})
          activation = joined.beginDownloadConnectivity()
          const connectionDeadline = performance.now() + 30000
          while (!admissions.some(row => row.route === 'direct')) {
            if (performance.now() > connectionDeadline) throw new Error('No direct lane: ' + JSON.stringify(admissions))
            await new Promise(resolve => setTimeout(resolve, 20))
          }
          window.__planProbe.initialPeerStats = await Promise.all(peers.map(async peer => [...(await peer.getStats()).values()].filter(row => ['transport','candidate-pair','local-candidate','remote-candidate'].includes(row.type))))
          if (isolated) window.__planProbe.initialPeerStats.forEach(requireIsolatedLAN)
          const started = performance.now()
          window.__planProbe.phase = 'transfer'
          const result = await joined.transferJob(operation.plans, operation.intent, activation, { selection: operation.selection, onProgress: event => { window.__planProbe.progress = event } }).run()
          window.__planProbe.phase = 'hash'
          await previewTask
          const file = await handle.getFile()
          const sha256 = Array.from(new Uint8Array(await crypto.subtle.digest('SHA-256', await file.arrayBuffer())), b => b.toString(16).padStart(2, '0')).join('')
          const stats = []
          for (const peer of peers) {
            const entries = [...(await peer.getStats()).values()]
            if (isolated) requireIsolatedLAN(entries)
            stats.push({ selected: selectedPair(entries), connectionState: peer.connectionState, channels: entries.filter(x => x.type === 'data-channel'), candidatePairs: entries.filter(x => x.type === 'candidate-pair' && x.state === 'succeeded'), candidates: entries.filter(x => x.type === 'local-candidate' || x.type === 'remote-candidate') })
          }
          return { workerStatus: result.worker.status, lifecycle: result.lifecycle.kind, size: file.size, sha256,
            elapsedMs: closed - started, firstWriteMs: firstWrite - started, setupMs: started - setupStarted,
            writes, outputBytes, outputWriteMs, admissions, dispatches, initialPeers: window.__planProbe.initialPeerStats.map(selectedPair), peers: stats, interactive }
        } finally {
          activation?.close()
          await joined.close()
          await disk?.abort().catch(() => {})
        }
      }, { key: share.key, bytes: fixture.length, slowWriteMs, interaction, topology, receiverHost: lab.receiverHost, senderHost: lab.senderHost })
      let timeout
      const monitor = process.env.WS_RESEARCH_DIAGNOSTIC === '1' ? setInterval(() => {
        page.evaluate(() => JSON.stringify(window.__planProbe, (_key, value) => typeof value === 'bigint' ? String(value) : value))
          .then(value => writeFile(join(output, n + '-progress.json'), value ?? '')).catch(() => {})
      }, 1000) : undefined
      const result = await Promise.race([task, new Promise((_, reject) => { timeout = setTimeout(() => reject(new Error('File probe exceeded 120 seconds')), 120000) })]).finally(() => { clearTimeout(timeout); clearInterval(monitor) })
      assert.equal(result.size, fixture.length)
      assert.equal(result.sha256, expectedHash)
      assert.equal(result.outputBytes, fixture.length)
      assert.equal(result.workerStatus, 'Succeeded')
      assert.equal(result.lifecycle, 'published')
      assert.equal(result.peers.length, 1, 'Single connection measurement')
      assert.deepEqual(Object.keys(result.dispatches), ['direct'])
      assert(result.elapsedMs > 0)
      if (interaction !== 'none') assert.equal(result.interactive.result, interaction === 'preview' ? 'image' : 'cancelled')
      await writeFile(join(output, n + '-sender.ndjson'), await stack.senderTraceArtifact(share))
      const metricsAfter = await cdp.send('Performance.getMetrics')
      const cpuSeconds = Object.fromEntries(metricsAfter.metrics.filter(metric => ['TaskDuration', 'ScriptDuration', 'V8CompileDuration'].includes(metric.name)).map(metric => [metric.name, metric.value - metricsBefore.metrics.find(before => before.name === metric.name).value]))
      const row = { repetition: n, cpuSeconds, ...result, MBps: result.size / result.elapsedMs / 1000, errors }
      report.rounds.push(row)
      console.log(JSON.stringify({ repetition: n, MBps: row.MBps, firstWriteMs: row.firstWriteMs, peers: row.peers.length, sha256: row.sha256, errors }))
    } catch (error) {
      await writeFile(join(output, n + '-sender.ndjson'), await stack.senderTraceArtifact(share))
      const diagnostic = await page.evaluate(() => JSON.stringify(window.__planProbe, (_key, value) => typeof value === 'bigint' ? String(value) : value, 2)).catch(() => '')
      await writeFile(join(output, n + '-failure.json'), diagnostic ?? '')
      throw error
    } finally {
      await page.close()
      await stack.stopSharing(share)
      await writeFile(join(output, 'report.json'), JSON.stringify(report, null, 2))
    }
  }
  await linux?.tcpControl('after')
  report.status = 'passed'
} catch (error) {
  report.status = 'failed'
  report.error = error.stack
  throw error
} finally {
  if (linux) await linux.close()
  else await persistentContext?.close()
  await stack.dispose()
  await writeFile(join(output, 'report.json'), JSON.stringify(report, null, 2))
  console.log(JSON.stringify({ report: join(output, 'report.json'), status: report.status }))
}
