import { expect, it } from 'vitest'

import {
  runArtifactGuardCleanBootstrap,
  type CleanBootstrapCommand,
} from './artifact-guard-clean-bootstrap.ts'

it('keeps clean-bootstrap execution behind one explicit deterministic capability', async () => {
  let observed: CleanBootstrapCommand | undefined
  const summary = await runArtifactGuardCleanBootstrap(Object.freeze({
    execute: async (command: CleanBootstrapCommand) => {
      observed = command
      return Object.freeze({
        exitCode: 0,
        stdout: JSON.stringify({ guardExport: 'function', entryCount: 0, expandedBytes: 0 }),
        stderr: '',
      })
    },
  }))

  expect(summary).toEqual({ guardExport: 'function', entryCount: 0, expandedBytes: 0 })
  expect(observed?.executable).toBe(process.execPath)
  expect(observed?.arguments).toHaveLength(1)
  expect(observed?.arguments[0]).toMatch(/bootstrap\.mjs$/u)
  expect(observed?.environment.NODE_OPTIONS).toBeUndefined()
  expect(observed?.environment.NODE_PATH).toBeUndefined()
  expect(observed?.environment.WINDSHARE_HOSTILE_ZIP_MARKER).toMatch(/hostile-zip-executed\.txt$/u)
})

it('fails closed before workspace execution without bootstrap DI', async () => {
  await expect(runArtifactGuardCleanBootstrap(undefined as never))
    .rejects.toThrow(/explicit executor capability/u)
})
