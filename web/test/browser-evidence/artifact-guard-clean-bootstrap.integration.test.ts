import { execFile } from 'node:child_process'
import { promisify } from 'node:util'

import { expect, it } from 'vitest'

import {
  runArtifactGuardCleanBootstrap,
  type CleanBootstrapCommand,
} from './artifact-guard-clean-bootstrap.ts'

const execFileAsync = promisify(execFile)

it('bootstraps the trusted guard without repository node_modules or hostile Zip.js', async () => {
  const summary = await runArtifactGuardCleanBootstrap(Object.freeze({
    execute: async (command: CleanBootstrapCommand) => {
      const { stdout, stderr } = await execFileAsync(command.executable, [...command.arguments], {
        cwd: command.cwd,
        env: command.environment,
        timeout: command.timeoutMs,
        windowsHide: true,
      })
      return Object.freeze({ exitCode: 0, stdout, stderr })
    },
  }))
  expect(summary).toEqual({ guardExport: 'function', entryCount: 0, expandedBytes: 0 })
})
