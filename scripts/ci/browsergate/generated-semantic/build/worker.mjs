import { resolve } from 'node:path'
import { pathToFileURL } from 'node:url'

import { createGeneratedSemanticBuildConfig } from './config.mjs'
import {
  createGeneratedSemanticFailure,
  generatedSemanticCauseMessage,
} from './failure.mjs'
import {
  createGeneratedSemanticWorkerBuiltResult,
  createGeneratedSemanticWorkerFailedResult,
  distillGeneratedSemanticBuildResult,
  encodeGeneratedSemanticWorkerResult,
  parseGeneratedSemanticWorkerRequest,
} from './worker-protocol.mjs'

export async function executeGeneratedSemanticBuildWorker(
  request,
  { importVite = defaultImportVite } = {},
) {
  let tools = null
  try {
    const vite = await importVite(request.viteModulePath)
    tools = requireToolVersions(vite)
    if (typeof vite.build !== 'function') throw new Error('resolved Vite module has no build function')
    const config = createGeneratedSemanticBuildConfig(request)
    const rawResult = await vite.build(config)
    return createGeneratedSemanticWorkerBuiltResult({
      tools,
      builds: distillGeneratedSemanticBuildResult(rawResult),
    })
  } catch (cause) {
    return createGeneratedSemanticWorkerFailedResult(
      createGeneratedSemanticFailure(
        'build',
        'vite-build-failed',
        generatedSemanticCauseMessage(cause, 'generated semantic Vite build failed'),
      ),
      tools,
    )
  }
}

export async function runGeneratedSemanticWorkerMain(
  arguments_,
  dependencies = undefined,
) {
  let result
  try {
    if (!Array.isArray(arguments_) || arguments_.length !== 1) {
      throw new Error('generated semantic worker requires one request')
    }
    const request = parseGeneratedSemanticWorkerRequest(arguments_[0])
    result = await executeGeneratedSemanticBuildWorker(request, dependencies)
  } catch (cause) {
    result = createGeneratedSemanticWorkerFailedResult(createGeneratedSemanticFailure(
      'build',
      'worker-request-invalid',
      generatedSemanticCauseMessage(cause, 'generated semantic worker request is invalid'),
    ))
  }
  return Object.freeze({
    exitCode: result.outcome === 'built' ? 0 : 1,
    record: encodeGeneratedSemanticWorkerResult(result),
  })
}

async function defaultImportVite(modulePath) {
  return import(pathToFileURL(modulePath).href)
}

function requireToolVersions(vite) {
  const versions = {
    vite: vite?.version,
    rolldown: vite?.rolldownVersion,
  }
  for (const [name, version] of Object.entries(versions)) {
    if (typeof version !== 'string' || version.length === 0) {
      throw new Error(`resolved ${name} module does not expose its version`)
    }
  }
  return Object.freeze(versions)
}

const invokedPath = process.argv[1]
if (invokedPath !== undefined && pathToFileURL(resolve(invokedPath)).href === import.meta.url) {
  const execution = await runGeneratedSemanticWorkerMain(process.argv.slice(2))
  process.stdout.write(execution.record)
  process.exitCode = execution.exitCode
}
