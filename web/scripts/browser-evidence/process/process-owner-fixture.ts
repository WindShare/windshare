import { execFile } from 'node:child_process'
import { mkdtemp, rm } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { promisify } from 'node:util'

import type { TestProcessOwnerArtifact } from './test-process-owner-client.mjs'

const REPOSITORY_ROOT = fileURLToPath(new URL('../../../../', import.meta.url))
const execFileAsync = promisify(execFile)

export interface TestProcessOwnerFixture {
  readonly owner: TestProcessOwnerArtifact
  dispose(): Promise<void>
}

export async function buildTestProcessOwnerFixture(): Promise<TestProcessOwnerFixture> {
  if (process.platform !== 'win32' && process.platform !== 'linux') {
    throw new Error(`the native process owner fixture is unsupported on ${process.platform}`)
  }
  const workspace = await mkdtemp(join(tmpdir(), 'windshare-test-process-owner-'))
  const ownerPath = join(
    workspace,
    process.platform === 'win32' ? 'testprocessowner.exe' : 'testprocessowner',
  )
  try {
    await execFileAsync(
      process.env.WINDSHARE_GO_EXECUTABLE ?? 'go',
      ['build', '-trimpath', '-buildvcs=false', '-ldflags=-buildid=', '-o', ownerPath, './cmd/testprocessowner'],
      { cwd: REPOSITORY_ROOT, windowsHide: true },
    )
  } catch (cause) {
    await rm(workspace, { recursive: true, force: true })
    throw cause
  }
  let disposed = false
  return Object.freeze({
    owner: Object.freeze({ path: ownerPath }),
    async dispose() {
      if (disposed) return
      disposed = true
      await rm(workspace, { recursive: true, force: true })
    },
  })
}
