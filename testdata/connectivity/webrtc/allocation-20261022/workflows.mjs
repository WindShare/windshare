import assert from 'node:assert/strict'
import { readFile, writeFile, mkdir } from 'node:fs/promises'
import { pathToFileURL, fileURLToPath } from 'node:url'
import { join } from 'node:path'

const root = fileURLToPath(new URL('../../../../', import.meta.url))
const mode = process.env.WS_ALLOCATION_WORKFLOW ?? 'file'
assert(['file', 'zip', 'reserve-zero'].includes(mode))
let source = await readFile(new URL('../slowdown-20261022/full-file.mjs', import.meta.url), 'utf8')
function replace(before, after) {
  assert.equal(source.split(before).length, 2, 'Probe source contract: ' + before)
  source = source.replace(before, after)
}
replace("from './linux-browser.mjs'", "from " + JSON.stringify(new URL('../slowdown-20261022/linux-browser.mjs', import.meta.url).href))
replace("from '../../../../web/e2e/fixtures/direct-product-stack'", "from " + JSON.stringify(pathToFileURL(join(root, 'web/e2e/fixtures/direct-product-stack.ts')).href))
replace("fileURLToPath(new URL('../../../../', import.meta.url))", JSON.stringify(root))
replace("label: process.env.WS_RESEARCH_LABEL,", "label: process.env.WS_RESEARCH_LABEL, allocationWorkflow: " + JSON.stringify(mode) + ",")
if (mode === 'reserve-zero') {
  replace('const errors = []', `const errors = []
    await page.route('**/src/content/scheduling/range-window.ts', async route => {
      const response = await route.fetch(), body = await response.text()
      const reserve = 'Math.min(interactive, Math.floor(maximum / 2))'
      assert(body.includes(reserve))
      await route.fulfill({ response, body: body.replace(reserve, '0') })
    })`)
}
if (mode === 'zip') {
  const helper = await readFile(new URL('./direct-zip.mjs', import.meta.url), 'utf8')
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
          if (result.worker.status !== 'Succeeded' || result.lifecycle.kind !== 'published') throw new Error('ZIP transfer failed: ' + JSON.stringify({ worker: result.worker, lifecycle: result.lifecycle }, (_key, value) => typeof value === 'bigint' ? String(value) : value instanceof Error ? { message: value.message, cause: value.cause, stack: value.stack } : value))
          const zipSnapshot = zipOperation.snapshot()
          closed = performance.now()
          firstWrite = zipSnapshot.writes.find(write => write.bytes >= Math.min(bytes, joined.descriptor.chunkSize))?.at
          writes = zipSnapshot.writes.length
          outputWriteMs = zipSnapshot.writes.reduce((sum, write) => sum + write.end - write.at, 0)
          writeTimeline.push(...zipSnapshot.writes)
          const archive = await zipOperation.read()
          outputBytes = archive.file.size`)
  replace('const file = memory ? new Blob([memory]) : await handle.getFile()', 'const file = archive.file')
  replace('writes, outputBytes, outputWriteMs, admissions', 'archiveBytes: archive.archiveBytes, writes, outputBytes, outputWriteMs, admissions')
  replace('await disk?.abort().catch(() => {})', 'await disk?.abort().catch(() => {})\n          await zipOperation?.close()')
  replace("scope: 'Production CLI file read/seal, protocol, browser authentication and SingleFileStreamOutputSession to the recorded sinkMode (OPFS or memory). Persistent browser profile; production p2p-only policy; existing test plan-settlement adapter. Connection/catalog setup and post-close SHA-256 excluded from throughput. Not UI, crash durability, resume or WAN evidence.'",
    "scope: 'Production CLI read/seal, protocol, authentication, Direct ZIP transfer, writer, journal and publication. Test picker selects OPFS and the existing activation fixture binds the authenticated share. Measures run through published settlement; ZIP member extraction and SHA-256 excluded. Not UI, crash or WAN evidence.'")
}
const directory = join(root, '.tmp/single-connection-implementation')
await mkdir(directory, { recursive: true })
const generated = join(directory, 'workflow-' + mode + '.mjs')
await writeFile(generated, source)
await import(pathToFileURL(generated).href)
