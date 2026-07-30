import assert from 'node:assert/strict'
import { resolve } from 'node:path'

import { executeOwnedRuntimeCommand } from './runtime-command-owner.mjs'

await missingOwnerFailsClosed('win32', 'Job helper')
await missingOwnerFailsClosed('linux', 'subreaper helper')
await unsupportedPlatformFailsClosed('darwin', 'descendant authority')
await unsupportedPlatformFailsClosed('freebsd', 'unsupported runtime command platform')

async function missingOwnerFailsClosed(platform, expectedMessage) {
  await assert.rejects(
    executeOwnedRuntimeCommand(baseRequest(platform)),
    new RegExp(expectedMessage, 'u'),
  )
}

async function unsupportedPlatformFailsClosed(platform, expectedMessage) {
  await assert.rejects(executeOwnedRuntimeCommand(baseRequest(platform)), new RegExp(expectedMessage, 'u'))
}

function baseRequest(platform) {
  return Object.freeze({
    operationId: `runtime-owner-${platform}-contract`,
    command: Object.freeze({
      executable: process.execPath,
      arguments: Object.freeze(['-e', 'process.exit(0)']),
      cwd: resolve(import.meta.dirname),
    }),
    platform,
    inheritedEnvironment: Object.freeze({}),
    deadlineMs: 5_000,
    terminationGraceMs: 1_000,
  })
}

process.stdout.write('runtime command owner fail-closed contracts: PASS\n')
