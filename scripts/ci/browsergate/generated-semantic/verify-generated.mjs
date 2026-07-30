import { resolve } from 'node:path'
import { pathToFileURL } from 'node:url'

import { executeGeneratedSemanticCli } from './build/cli.mjs'

export async function runGeneratedSemanticMain(
  arguments_ = process.argv.slice(2),
  { write = (record) => process.stdout.write(record) } = {},
) {
  const execution = await executeGeneratedSemanticCli(arguments_)
  write(execution.record)
  return execution.exitCode
}

const entryPoint = process.argv[1]
if (entryPoint !== undefined && import.meta.url === pathToFileURL(resolve(entryPoint)).href) {
  process.exitCode = await runGeneratedSemanticMain()
}
