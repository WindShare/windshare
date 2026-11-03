import { expect, test } from '@playwright/test'
import { mkdir, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { dirname, join, resolve } from 'node:path'
import { BROWSER_CONTRACT_HOST_PATH } from './contract-host'
import type { exerciseNativeNameBudget } from './fsa-name-budget-harness'

const HARNESS_PATH = '/test/browser/fsa-name-budget-harness.ts'
// Chromium's headless permission context grants native FSA writes under this test-only flag.
test.use({ launchOptions: { args: ['--enable-experimental-web-platform-features'] } })

test('native FSA saves expanded Unicode files and generated overlong directory roots', async ({ page, context }) => {
  const temporaryParent = resolve(tmpdir())
  const directory = await mkdtemp(join(temporaryParent, 'ws-fsa-'))
  if (dirname(resolve(directory)) !== temporaryParent) throw new Error('Unexpected native FSA test directory')
  const rawName = '\u0958'.repeat(process.platform === 'win32' ? 130 : 80)
  const source = join(directory, 'source')
  const destination = join(directory, 'output')
  try {
    await mkdir(source)
    await mkdir(destination)
    await writeFile(join(source, rawName), 'source')
    await page.goto(BROWSER_CONTRACT_HOST_PATH)
    await page.evaluate(() => {
      const runtime = globalThis as typeof globalThis & { nativeNameBudgetParent?: Promise<FileSystemDirectoryHandle> }
      runtime.nativeNameBudgetParent = new Promise((resolveParent, reject) => {
        document.addEventListener('dragover', event => event.preventDefault())
        document.addEventListener('drop', event => {
          event.preventDefault()
          const item = event.dataTransfer?.items[0] as DataTransferItem & {
            getAsFileSystemHandle(): Promise<FileSystemDirectoryHandle>
          }
          item.getAsFileSystemHandle().then(resolveParent, reject)
        }, { once: true })
      })
    })
    const cdp = await context.newCDPSession(page)
    for (const type of ['dragEnter', 'dragOver', 'drop'] as const) {
      await cdp.send('Input.dispatchDragEvent', {
        type, x: 20, y: 20, data: { items: [], files: [destination], dragOperationsMask: 1 },
      })
    }
    const results = await page.evaluate(async ({ harnessPath, rawName }) => {
      const harness = await import(harnessPath) as { exerciseNativeNameBudget: typeof exerciseNativeNameBudget }
      const runtime = globalThis as typeof globalThis & { nativeNameBudgetParent?: Promise<FileSystemDirectoryHandle> }
      const parent = await runtime.nativeNameBudgetParent
      if (parent === undefined) throw new Error('Native directory drop did not produce a handle')
      return harness.exerciseNativeNameBudget(parent, rawName)
    }, { harnessPath: HARNESS_PATH, rawName })
    expect(results).toHaveLength(2)
    for (const result of results) {
      expect(result.evidenceKind).toBe('component-byte-budget')
      expect(result.mappingName).toBe(result.logicalName)
      expect(result.physicalName).not.toBe(result.logicalName)
      const outputPath = result.kind === 'file'
        ? join(destination, result.physicalName)
        : join(destination, result.physicalName, 'received.txt')
      expect(await readFile(outputPath, 'utf8')).toBe(
        result.kind === 'file' ? 'file received' : 'directory received',
      )
    }
    expect(await readFile(join(source, rawName), 'utf8')).toBe('source')
  } finally {
    await page.close()
    await rm(directory, { recursive: true, force: true })
  }
})
