import { lstat, mkdtemp, readdir, readFile, rm } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join, posix, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { build as viteBuild } from 'vite'
import { consoleDistDirectory, selectConsoleApps } from './applications.mjs'

const TEMP_DIR_PREFIX = 'windshare-console-dist-'

async function collectFiles(rootDir, prefix = '', files = new Map()) {
  const entries = (await readdir(rootDir, { withFileTypes: true }))
    .toSorted((left, right) => left.name.localeCompare(right.name))
  for (const entry of entries) {
    const fullPath = join(rootDir, entry.name)
    const relativePath = prefix ? posix.join(prefix, entry.name) : entry.name
    const stat = await lstat(fullPath)
    if (stat.isDirectory()) {
      await collectFiles(fullPath, relativePath, files)
    } else if (stat.isFile()) {
      files.set(relativePath, await readFile(fullPath))
    } else {
      throw new Error(`Unsupported non-regular file in console bundle: ${relativePath}`)
    }
  }
  return files
}

async function readBundleFiles(directory, label) {
  try {
    return await collectFiles(directory)
  } catch (error) {
    if (error && typeof error === 'object' && error.code === 'ENOENT') {
      throw new Error(`${label} directory is missing: ${directory}`, { cause: error })
    }
    throw error
  }
}

export async function compareDirectories(checkedInDir, rebuiltDir) {
  const [checkedIn, rebuilt] = await Promise.all([
    readBundleFiles(checkedInDir, 'Checked-in console bundle'),
    readBundleFiles(rebuiltDir, 'Rebuilt console bundle'),
  ])
  const missing = []
  const changed = []
  for (const [relativePath, expectedBytes] of rebuilt) {
    const actualBytes = checkedIn.get(relativePath)
    if (!actualBytes) {
      missing.push(relativePath)
    } else if (!actualBytes.equals(expectedBytes)) {
      changed.push(relativePath)
    }
  }
  const extra = [...checkedIn.keys()].filter(relativePath => !rebuilt.has(relativePath))
  return {
    matches: missing.length === 0 && extra.length === 0 && changed.length === 0,
    fileCount: rebuilt.size,
    missing,
    extra,
    changed,
  }
}

export function formatDifferences(appName, diff) {
  const lines = [`Embedded ${appName} console-dist/ does not match a clean rebuild:`]
  for (const file of diff.missing) lines.push(`  missing: ${file}`)
  for (const file of diff.extra) lines.push(`  extra:   ${file}`)
  for (const file of diff.changed) lines.push(`  changed: ${file}`)
  lines.push(`Run 'pnpm -C consoles build:${appName}' and commit the updated console-dist/.`)
  return lines.join('\n')
}

export async function defaultBuildBundle({ workspace, appName, outDir }) {
  await viteBuild({
    mode: appName,
    configFile: resolve(workspace, 'vite.config.mjs'),
    logLevel: 'silent',
    build: { outDir, emptyOutDir: true },
  })
}

export async function verifyConsoleBundles({
  workspace = fileURLToPath(new URL('..', import.meta.url)),
  target = 'all',
  buildBundle = defaultBuildBundle,
  distDirectory = consoleDistDirectory,
  tempBaseDir = tmpdir(),
} = {}) {
  const apps = selectConsoleApps(target)
  // Build into a disposable directory so verification never mutates the working tree.
  const tempRoot = await mkdtemp(join(tempBaseDir, TEMP_DIR_PREFIX))
  try {
    const verified = []
    const failures = []
    for (const appName of apps) {
      const rebuiltDir = join(tempRoot, appName)
      await buildBundle({ workspace, appName, outDir: rebuiltDir })
      const diff = await compareDirectories(distDirectory(workspace, appName), rebuiltDir)
      if (diff.matches) {
        verified.push({ appName, fileCount: diff.fileCount })
      } else {
        failures.push(formatDifferences(appName, diff))
      }
    }
    if (failures.length > 0) {
      throw new Error(failures.join('\n\n'))
    }
    return verified
  } finally {
    await rm(tempRoot, { recursive: true, force: true })
  }
}

export function parseTargetArg(argv) {
  const positional = argv.filter(arg => arg !== '--')
  if (positional.length > 1) {
    throw new Error(`Expected at most one console target, got: ${positional.join(' ')}`)
  }
  return positional[0] ?? 'all'
}

function isDirectExecution() {
  return process.argv[1] !== undefined && fileURLToPath(import.meta.url) === resolve(process.argv[1])
}

if (isDirectExecution()) {
  try {
    const target = parseTargetArg(process.argv.slice(2))
    const verified = await verifyConsoleBundles({ target })
    for (const { appName, fileCount } of verified) {
      console.log(`console-dist (${appName}): verified ${fileCount} files`)
    }
  } catch (error) {
    console.error(error instanceof Error ? error.message : String(error))
    process.exitCode = 1
  }
}
