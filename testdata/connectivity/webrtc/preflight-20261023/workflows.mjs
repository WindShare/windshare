import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'

export async function extendWorkflow(original, config) {
  let source = original
  const replace = (before, after) => {
    assert.equal(source.split(before).length, 2, 'Workflow anchor: ' + before)
    source = source.replace(before, after)
  }
  if (config.workflow === 'zip') {
    const helper = await readFile(new URL('../allocation-20261022/direct-zip.mjs', import.meta.url), 'utf8')
    replace('const errors = []', `const errors = []
      await page.route('**/__research/direct-zip.mjs', route => route.fulfill({ contentType: 'text/javascript', body: ${JSON.stringify(helper)} }))
      await page.route('**/test/browser/direct-zip/production-activation.ts', async route => {
        const response = await route.fetch()
        let body = await response.text()
        assert(body.includes('const rootId = id(16, 2)'))
        body = body.replace('const rootId = id(16, 2)', 'const rootId = window.__zipProbeShare.syntheticRootId')
        assert(body.includes('shareInstance: id(16, 3)'))
        body = body.replace('shareInstance: id(16, 3)', 'shareInstance: window.__zipProbeShare.shareInstanceId')
        body = body.replaceAll('createDirectorySelectionResultRoot', 'createSyntheticSelectionResultRoot')
        await route.fulfill({ response, body })
      })`)
    replace('let activation, disk', 'let activation, disk, zipOperation')
    replace('const operation = await bindDirectAtomicOperation({ transferBytes: bytes }, joined, modules, delivery, {})',
      "const operation = zipOperation = await (await import('/__research/direct-zip.mjs')).bindProductionZip(joined)")
    replace('{ selection: operation.selection, onProgress:', '{ selection: operation.selection, transferJobId: operation.transferJobId, onProgress:')
    replace("window.__planProbe.phase = 'hash'", `window.__planProbe.phase = 'hash'
      if (result.worker.status !== 'Succeeded' || result.lifecycle.kind !== 'published') throw new Error('ZIP failed')
      closed = performance.now()
      const zipSnapshot = zipOperation.snapshot()
      firstWrite = zipSnapshot.writes.find(write => write.bytes >= Math.min(bytes, joined.descriptor.chunkSize))?.at
      writes = zipSnapshot.writes.length
      outputWriteMs = zipSnapshot.writes.reduce((sum, write) => sum + write.end - write.at, 0)
      writeTimeline.push(...zipSnapshot.writes)
      const archive = await zipOperation.read()
      outputBytes = archive.file.size`)
    replace('const file = memory ? new Blob([memory]) : await handle.getFile()', 'const file = archive.file')
    replace('writes, outputBytes, outputWriteMs, admissions', 'archiveBytes: archive.archiveBytes, writes, outputBytes, outputWriteMs, admissions')
    replace('await disk?.abort().catch(() => {})', 'await disk?.abort().catch(() => {})\n await zipOperation?.close()')
    replace("scope: 'Production CLI file read/seal, protocol, browser authentication and SingleFileStreamOutputSession to the recorded sinkMode (OPFS or memory). Persistent browser profile; production p2p-only policy; existing test plan-settlement adapter. Connection/catalog setup and post-close SHA-256 excluded from throughput. Not UI, crash durability, resume or WAN evidence.'",
      "scope: 'Production CLI read/seal, authenticated protocol, Direct ZIP writer/journal/publication with test activation and OPFS picker. ZIP extraction/hash after publication excluded. Not UI, crash durability, resume or WAN evidence.'")
  }
  if (config.receivers > 1) {
    assert(config.workflow === 'file' && config.receivers === 2 && !config.profile)
    replace('    const page = await context.newPage()', `    const page = await context.newPage()
    await page.addInitScript(index => { window.__preflightReceiverIndex = index }, n)`)
    replace("root.getFileHandle('received.bin',", "root.getFileHandle('received-' + window.__preflightReceiverIndex + '.bin',")
    replace("remoteBrowser ? sampleSockets : () => {}", "remoteBrowser ? phase => sampleSockets(phase + ':' + n) : () => {}")
    replace('    const share = await stack.share(file, { blockSizeBytes: blockMiB * 1024 * 1024, senderTrace: traceEnabled })',
      '    const share = sharedShare')
    replace('  for (let n = 0; n < repeats; n++) {', `
      const sharedShare = await stack.share(file, { blockSizeBytes: blockMiB * 1024 * 1024, senderTrace: traceEnabled })
      const relays = new URL(sharedShare.bareLink).searchParams.getAll('r').map(url => new URL(url).port)
      linux = await startLinuxBrowser([new URL(stack.baseURL).port, ...relays], report)
      browser = linux.browser
      persistentContext = linux.context
      await persistentContext.grantPermissions(['local-network-access', 'microphone'])
      await linux.tcpControl('before')
      let waiting = 0, release
      const ready = new Promise(resolve => { release = resolve })
      const runReceiver = async n => {
    `)
    replace("    const errors = []", `    await page.exposeFunction('concurrentReady', () => { if (++waiting === ${config.receivers}) release(); return ready })
    const errors = []`)
    replace("          await window.sampleTransferMetrics('before')", "          await window.concurrentReady()\n          await window.sampleTransferMetrics('before')")
    replace('      await stack.stopSharing(share)', '      // The shared sender outlives both receiver tasks.')
    replace("  }\n  await linux?.tcpControl('after')", `  }
      const results = await Promise.allSettled(Array.from({ length: ${config.receivers} }, (_, index) => runReceiver(index)))
      await stack.stopSharing(sharedShare)
      const failure = results.find(result => result.status === 'rejected')
      if (failure) throw failure.reason
      await linux?.tcpControl('after')`)
  }
  return source
}
