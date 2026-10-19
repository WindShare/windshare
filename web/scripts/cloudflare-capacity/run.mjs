import { mkdir } from 'node:fs/promises'
import { join, resolve } from 'node:path'
import { chromium } from '@playwright/test'
import { preview } from 'vite'
import { build, event, ProductionDeployment, save, WEB } from './deployment.mjs'
import { runScenario } from './scenario.mjs'

const scenarios = [
  { name: 'idle', fileMiB: 1, feedbackDelaysMs: [0], idleMs: 300_000 },
  { name: 'single', fileMiB: 32, feedbackDelaysMs: [0] },
  { name: 'paired', fileMiB: 16, feedbackDelaysMs: [0, 400] },
]
const output = resolve(process.argv[2] ?? join(WEB, 'test-results/cloudflare-capacity', new Date().toISOString().replaceAll(':', '-')))
// Reusing a directory could mix old deployment records into a new measurement.
await mkdir(resolve(output, '..'), { recursive: true })
await mkdir(output)
event('capacity-run-start', { output })
const built = await build(output)
const frontend = await preview({
  root: WEB, configFile: false, build: { outDir: built.frontend },
  preview: { host: '127.0.0.1', port: 0, strictPort: true },
})
const address = frontend.httpServer.address()
if (!address || typeof address === 'string') throw new Error('Frontend did not bind a TCP port')
const frontendURL = 'http://127.0.0.1:' + address.port
const browser = await chromium.launch()
const results = []
try {
  for (const scenario of scenarios) {
    const directory = join(output, scenario.name)
    const deployment = new ProductionDeployment(directory)
    try {
      await deployment.provision()
      results.push(await runScenario(browser, built, frontendURL, deployment, directory, scenario))
    } finally {
      await deployment.dispose()
    }
  }
} finally {
  await browser.close()
  await new Promise((resolveClose, reject) => frontend.httpServer.close(error => error ? reject(error) : resolveClose()))
  await save(join(output, 'results.json'), { build: built.identity, results, finishedAt: new Date().toISOString() })
}
event('capacity-run-complete', { output, scenarios: results.length })
