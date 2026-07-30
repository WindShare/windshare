import { resolve } from 'node:path'
import { pathToFileURL } from 'node:url'

import { runBrowserGateCli } from './orchestrator.mjs'

export * from './orchestrator.mjs'

const invokedPath = process.argv[1]
if (invokedPath !== undefined && pathToFileURL(resolve(invokedPath)).href === import.meta.url) {
  try {
    process.exitCode = await runBrowserGateCli(process.argv.slice(2))
  } catch (cause) {
    process.stderr.write(JSON.stringify({
      component: 'browser-orchestration',
      milestone: 'failed',
      error: cause instanceof Error ? cause.message : String(cause),
    }) + '\n')
    process.exitCode = 1
  }
}
