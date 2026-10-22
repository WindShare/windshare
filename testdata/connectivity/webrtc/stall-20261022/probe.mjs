// #region DEBUG
import assert from 'node:assert/strict'
import { readFile, writeFile, mkdir } from 'node:fs/promises'
import { fileURLToPath, pathToFileURL } from 'node:url'
import { join } from 'node:path'
import { execFileSync } from 'node:child_process'

const root = fileURLToPath(new URL('../../../../', import.meta.url))
const directory = join(root, '.tmp/webrtc-stall-20261022')
await mkdir(directory, { recursive: true })
let source = execFileSync('git',['show','f6d058cd:testdata/connectivity/webrtc/slowdown-20261022/full-file.mjs'],{cwd:root,encoding:'utf8',windowsHide:true})
function replace(before, after) {
  assert.equal(source.split(before).length, 2, 'Stall probe contract: ' + before)
  source = source.replace(before, after)
}
replace("import assert from 'node:assert/strict'", "import assert from 'node:assert/strict'\nimport { appendFileSync } from 'node:fs'\nconst debugEvidence = (...values) => appendFileSync(" + JSON.stringify(join(root,'.agents/debug.log')) + ", JSON.stringify({ time:Date.now(), hypothesis:'[DEBUG H1 H2 H3 H4]', values })+'\\n')")
replace("from './linux-browser.mjs'", 'from '+JSON.stringify(new URL('./linux-browser.mjs',import.meta.url).href))
replace("from '../../../../web/e2e/fixtures/direct-product-stack'", 'from '+JSON.stringify(pathToFileURL(join(root,'.tmp/webrtc-stall-20261022/web-snapshot/web/e2e/fixtures/direct-product-stack.ts')).href))
replace("fileURLToPath(new URL('../../../../', import.meta.url))", JSON.stringify(root))
replace("join(root, '.tmp/webrtc-slowdown-20261022', runId)", "join(root, '.tmp/webrtc-stall-20261022/runs', runId)")
replace("await mkdir(output, { recursive: true })", "await mkdir(output, { recursive: true })\nprocess.env.WS_STALL_TRACE = join(output,'sender')\nprocess.env.WS_STALL_OUTPUT = output")
source = source.replaceAll('console.log(', 'debugEvidence(').replaceAll('console.error(', 'debugEvidence(')
const generated = join(directory, 'probe.mjs')
await writeFile(generated,source)
await import(pathToFileURL(generated).href)
// #endregion DEBUG
