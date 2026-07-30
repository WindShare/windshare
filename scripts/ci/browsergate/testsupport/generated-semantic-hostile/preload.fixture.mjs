import { appendFileSync } from 'node:fs'
import { isAbsolute, resolve } from 'node:path'

const recordPath = new URL(import.meta.url).searchParams.get('recordPath')
if (recordPath === null || !isAbsolute(recordPath) || resolve(recordPath) !== recordPath) {
  throw new Error('hostile generated semantic preload requires a canonical record path')
}

// This deliberately benign side effect makes bootstrap inheritance observable.
// A second record means the verifier leaked its parent NODE_OPTIONS into the worker.
appendFileSync(recordPath, `${JSON.stringify({
  schemaVersion: 'windshare.generated-semantic-hostile-preload/v1',
  pid: process.pid,
  parentPid: process.ppid,
  entryPoint: resolve(process.argv[1] ?? ''),
  workingDirectory: resolve(process.cwd()),
})}\n`, { encoding: 'utf8' })
