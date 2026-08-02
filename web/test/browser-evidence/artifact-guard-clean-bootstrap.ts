import { access, cp, mkdir, mkdtemp, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

const browserEvidenceSource = fileURLToPath(new URL('../../scripts/browser-evidence/', import.meta.url))
const publisherProtocolSource = fileURLToPath(new URL(
  '../../scripts/browser-network-matrix/cli/publisher-helper-protocol.ts',
  import.meta.url,
))

export interface CleanBootstrapCommand {
  readonly executable: string
  readonly arguments: readonly string[]
  readonly cwd: string
  readonly environment: Readonly<Record<string, string>>
  readonly timeoutMs: number
}

export interface CleanBootstrapExecution {
  readonly exitCode: number
  readonly stdout: string
  readonly stderr: string
}

export interface CleanBootstrapExecutor {
  execute(command: CleanBootstrapCommand): Promise<CleanBootstrapExecution>
}

export interface CleanBootstrapSummary {
  readonly guardExport: 'function'
  readonly entryCount: 0
  readonly expandedBytes: 0
}

const CLEAN_BOOTSTRAP_TIMEOUT_MS = 15_000

/**
 * Workspace construction is pure orchestration; acquiring a child process is a
 * caller-owned integration capability. This keeps the same hostile module
 * scenario available to both the contract double and the native preflight.
 */
export async function runArtifactGuardCleanBootstrap(
  executor: CleanBootstrapExecutor,
): Promise<CleanBootstrapSummary> {
  requireExecutor(executor)
  const workspace = await mkdtemp(join(tmpdir(), 'windshare-guard-clean-bootstrap-'))
  const copiedEvidence = join(workspace, 'web', 'scripts', 'browser-evidence')
  const copiedPublisherProtocol = join(
    workspace,
    'web',
    'scripts',
    'browser-network-matrix',
    'cli',
    'publisher-helper-protocol.ts',
  )
  const markerPath = join(workspace, 'hostile-zip-executed.txt')
  try {
    await mkdir(dirname(copiedEvidence), { recursive: true })
    await cp(browserEvidenceSource, copiedEvidence, { recursive: true })
    await mkdir(dirname(copiedPublisherProtocol), { recursive: true })
    await cp(publisherProtocolSource, copiedPublisherProtocol)
    await writeFile(join(workspace, 'web', 'package.json'), JSON.stringify({ type: 'module' }), 'utf8')
    await installHostileZipModule(workspace)
    const bootstrapPath = join(workspace, 'bootstrap.mjs')
    await writeFile(bootstrapPath, bootstrapProgram(), 'utf8')
    const environment = Object.fromEntries(
      Object.entries(process.env).filter((entry): entry is [string, string] => entry[1] !== undefined),
    )
    environment.WINDSHARE_HOSTILE_ZIP_MARKER = markerPath
    delete environment.NODE_OPTIONS
    delete environment.NODE_PATH
    const execution = await executor.execute(Object.freeze({
      executable: process.execPath,
      arguments: Object.freeze([bootstrapPath]),
      cwd: workspace,
      environment: Object.freeze(environment),
      timeoutMs: CLEAN_BOOTSTRAP_TIMEOUT_MS,
    }))
    if (execution.exitCode !== 0 || execution.stderr !== '') {
      throw new Error('clean guard bootstrap did not exit successfully and silently')
    }
    const summary = parseSummary(execution.stdout)
    await requireAbsent(markerPath)
    return summary
  } finally {
    await rm(workspace, { recursive: true, force: true })
  }
}

function requireExecutor(value: unknown): asserts value is CleanBootstrapExecutor {
  if (
    typeof value !== 'object' || value === null || Object.keys(value).length !== 1 ||
    !Object.hasOwn(value, 'execute') ||
    typeof (value as { readonly execute?: unknown }).execute !== 'function'
  ) throw new Error('clean guard bootstrap requires one explicit executor capability')
}

function parseSummary(encoded: string): CleanBootstrapSummary {
  let value: unknown
  try {
    value = JSON.parse(encoded)
  } catch (cause) {
    throw new Error('clean guard bootstrap emitted invalid JSON', { cause })
  }
  if (
    typeof value !== 'object' || value === null ||
    Object.keys(value).sort().join(',') !== 'entryCount,expandedBytes,guardExport' ||
    !Object.hasOwn(value, 'guardExport') || !Object.hasOwn(value, 'entryCount') ||
    !Object.hasOwn(value, 'expandedBytes') ||
    (value as Record<string, unknown>).guardExport !== 'function' ||
    (value as Record<string, unknown>).entryCount !== 0 ||
    (value as Record<string, unknown>).expandedBytes !== 0
  ) throw new Error('clean guard bootstrap emitted an unexpected contract')
  return Object.freeze({ guardExport: 'function', entryCount: 0, expandedBytes: 0 })
}

async function requireAbsent(path: string): Promise<void> {
  try {
    await access(path)
  } catch (cause) {
    if (
      typeof cause === 'object' && cause !== null && 'code' in cause &&
      (cause as NodeJS.ErrnoException).code === 'ENOENT'
    ) return
    throw cause
  }
  throw new Error('hostile lifecycle-private Zip.js executed during clean bootstrap')
}

async function installHostileZipModule(workspace: string): Promise<void> {
  const moduleRoot = join(workspace, 'web', 'node_modules', '@zip.js', 'zip.js')
  await mkdir(moduleRoot, { recursive: true })
  await writeFile(join(moduleRoot, 'package.json'), JSON.stringify({
    name: '@zip.js/zip.js',
    type: 'module',
    exports: './index.js',
  }), 'utf8')
  await writeFile(join(moduleRoot, 'index.js'), [
    "import { writeFileSync } from 'node:fs'",
    "writeFileSync(process.env.WINDSHARE_HOSTILE_ZIP_MARKER, 'executed', 'utf8')",
    "throw new Error('trusted host executed hostile lifecycle-private Zip.js')",
  ].join('\n'), 'utf8')
}

function bootstrapProgram(): string {
  return [
    "const guard = await import('./web/scripts/browser-evidence/artifact/guard.ts')",
    "const archive = await import('./web/scripts/browser-evidence/archive/trusted-zip.ts')",
    "if (typeof guard.startScanSampleArtifacts !== 'function') throw new Error('guard export is absent')",
    "const bytes = Buffer.from('504b0506000000000000000000000000000000000000', 'hex')",
    'const summary = await archive.scanTrustedZip({',
    '  byteLength: bytes.byteLength,',
    '  async readExactly(offset, length) { return bytes.subarray(offset, offset + length) },',
    '}, { maximumEntries: 1, maximumExpandedBytes: 1, maximumPathBytes: 1024 }, {',
    "  start() { throw new Error('empty ZIP unexpectedly started an entry') },",
    "  chunk() { throw new Error('empty ZIP unexpectedly emitted bytes') },",
    "  end() { throw new Error('empty ZIP unexpectedly ended an entry') },",
    '})',
    'process.stdout.write(JSON.stringify({',
    "  guardExport: typeof guard.startScanSampleArtifacts,",
    '  entryCount: summary.entryCount,',
    '  expandedBytes: summary.expandedBytes,',
    '}))',
  ].join('\n')
}
