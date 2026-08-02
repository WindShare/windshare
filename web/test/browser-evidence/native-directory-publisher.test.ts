import { execFile } from 'node:child_process'
import { mkdtemp, readFile, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { performance } from 'node:perf_hooks'
import { promisify } from 'node:util'

import { describe, expect, it } from 'vitest'

import { GuardExecutionLease } from '../../scripts/browser-evidence/execution/guard-execution-lease.ts'
import { createNativeDirectoryPublisher } from '../../scripts/browser-evidence/filesystem/native-directory-publisher.ts'

const execFilePromise = promisify(execFile)

describe('native directory publisher execution lease', () => {
  it('forcibly settles a helper that never exits before returning deadline failure', async () => {
    const workspace = await mkdtemp(join(tmpdir(), 'windshare-never-exit-publisher-'))
    try {
      const pidPath = join(workspace, 'publisher.pid')
      const sourcePath = join(workspace, 'main.go')
      const executablePath = join(
        workspace,
        process.platform === 'win32' ? 'never-exit.exe' : 'never-exit',
      )
      await writeFile(sourcePath, `package main

import (
	"io"
	"os"
	"strconv"
	"time"
)

func main() {
	_ = os.WriteFile(${JSON.stringify(pidPath)}, []byte(strconv.Itoa(os.Getpid())), 0600)
	_, _ = io.Copy(io.Discard, os.Stdin)
	for { time.Sleep(time.Hour) }
}
`, 'utf8')
      await execFilePromise(process.env.WINDSHARE_GO_EXECUTABLE ?? 'go', ['build', '-trimpath', '-buildvcs=false', '-o', executablePath, sourcePath], {
        cwd: workspace,
        windowsHide: true,
      })
      const startedAt = performance.now()
      const executionLease = GuardExecutionLease.start({
        totalBudgetMs: 12_000,
        cleanupReserveMs: 2_000,
        nativeOperationBudgetMs: 8_000,
      })
      const publisher = createNativeDirectoryPublisher({
        path: executablePath,
      })
      const failure = await publisher.invoke({
        operation: 'prepare-existing-directory',
        parentPath: workspace,
        outputName: 'sealed',
        stagingName: '.browser-evidence-upload-00000000000000000000000000000000',
        inventory: { directories: [], files: [] },
        manifestPath: 'manifest.json',
        expectedManifestSha256: 'a'.repeat(64),
      }, executionLease.primaryWindow('never-exit native publisher')).then(
        () => undefined,
        (cause: unknown) => cause,
      )
      expect(failure).toBeInstanceOf(Error)
      expect(flattenErrorMessages(failure).join('\n')).toMatch(/deadline exceeded/u)
      expect(performance.now() - startedAt).toBeLessThan(10_000)

      const pid = Number.parseInt(await readFile(pidPath, 'utf8'), 10)
      expect(Number.isSafeInteger(pid) && pid > 0).toBe(true)
      expect(() => process.kill(pid, 0)).toThrow()
    } finally {
      await rm(workspace, { recursive: true, force: true })
    }
  }, 20_000)
})

function flattenErrorMessages(value: unknown): readonly string[] {
  if (!(value instanceof Error)) return [String(value)]
  const nested = value instanceof AggregateError
    ? value.errors.flatMap(flattenErrorMessages)
    : []
  return [value.message, ...nested]
}
