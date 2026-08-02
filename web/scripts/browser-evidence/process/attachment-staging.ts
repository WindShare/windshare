import { randomBytes } from 'node:crypto'
import type { BigIntStats } from 'node:fs'
import {
  constants,
  copyFile,
  lstat,
  mkdtemp,
  open,
  rename,
  rm,
  mkdir,
  writeFile,
} from 'node:fs/promises'
import type { FileHandle } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { basename, dirname, join, resolve } from 'node:path'

const RUNNER_ARTIFACTS_DIRECTORY = 'runner'
const CHILD_STAGING_SEPARATOR = '-child-attachments-'
const RUNNER_STAGING_PREFIX = 'windshare-browser-runner-'
const CLEANUP_NONCE_BYTES = 16
const MKTEMP_SUFFIX_LENGTH = 6
const STAGING_FAULT_BACKUP_SUFFIX = '.fault-owned'
const STAGING_FAULT_MARKER_NAME = 'foreign.txt'
const STAGING_FAULT_MARKER_TEXT = 'declarative staging fault replacement'
const STAGING_PARTIAL_COLLECTION_NAME = 'partial-collection.txt'

export const BROWSER_SAMPLE_STAGING_FAULT_EVIDENCE = Object.freeze({
  backupSuffix: STAGING_FAULT_BACKUP_SUFFIX,
  markerName: STAGING_FAULT_MARKER_NAME,
  markerText: STAGING_FAULT_MARKER_TEXT,
  partialCollectionName: STAGING_PARTIAL_COLLECTION_NAME,
})

export const BROWSER_SAMPLE_STAGING_FAULT_CUTS = Object.freeze([
  'replace-root-before-finalize',
  'replace-root-before-rollback',
  'fail-after-finalize',
  'replace-root-and-fail-after-finalize',
] as const)

export type BrowserSampleStagingFaultCut =
  (typeof BROWSER_SAMPLE_STAGING_FAULT_CUTS)[number]

type StagingState = 'active' | 'finalizing' | 'finalized' | 'committed' | 'disposed'

export interface FinalizedArtifactCollection {
  readonly absoluteRoot: string
}

export interface StagingLifecycleSettlement {
  readonly phase: 'ownership-transfer'
  readonly failures: readonly unknown[]
}

export function requireFinalizedArtifactCollectionRoot(
  sampleDirectory: string,
  artifactRoot: string,
): string {
  const canonicalSampleDirectory = resolve(sampleDirectory)
  const canonicalArtifactRoot = resolve(artifactRoot)
  if (sampleDirectory !== canonicalSampleDirectory || artifactRoot !== canonicalArtifactRoot) {
    throw new Error('artifact collection authority paths must be canonical and absolute')
  }
  if (dirname(canonicalArtifactRoot) !== dirname(canonicalSampleDirectory)) {
    throw new Error('artifact collection must be a private direct sibling of its sample directory')
  }
  const prefix = `.${basename(canonicalSampleDirectory)}${CHILD_STAGING_SEPARATOR}`
  const artifactName = basename(canonicalArtifactRoot)
  if (!artifactName.startsWith(prefix) || artifactName.length !== prefix.length + MKTEMP_SUFFIX_LENGTH) {
    throw new Error('artifact collection does not match its parent-issued sample capability')
  }
  return canonicalArtifactRoot
}

/**
 * The collection path is deliberately absent from result.json. It is an
 * out-of-band parent capability used only after the child tree exits, then fed
 * to the independent guard transaction that decides upload authority.
 */
export class BrowserSampleStaging {
  readonly sampleDirectory: string
  readonly childAttachmentRoot: string
  readonly runnerStagingRoot: string
  readonly #sampleHandle: FileHandle
  readonly #childHandle: FileHandle
  readonly #runnerHandle: FileHandle
  readonly #faultCut: BrowserSampleStagingFaultCut | undefined
  #state: StagingState = 'active'
  #collection: FinalizedArtifactCollection | null = null
  #runnerStagingRemoved = false

  private constructor(
    sampleDirectory: string,
    childAttachmentRoot: string,
    runnerStagingRoot: string,
    sampleHandle: FileHandle,
    childHandle: FileHandle,
    runnerHandle: FileHandle,
    faultCut: BrowserSampleStagingFaultCut | undefined,
  ) {
    this.sampleDirectory = sampleDirectory
    this.childAttachmentRoot = childAttachmentRoot
    this.runnerStagingRoot = runnerStagingRoot
    this.#sampleHandle = sampleHandle
    this.#childHandle = childHandle
    this.#runnerHandle = runnerHandle
    this.#faultCut = faultCut
  }

  static async create(
    sampleDirectory: string,
    faultCut?: BrowserSampleStagingFaultCut,
  ): Promise<BrowserSampleStaging> {
    if (
      faultCut !== undefined &&
      !BROWSER_SAMPLE_STAGING_FAULT_CUTS.includes(faultCut)
    ) throw new Error('browser sample staging fault cut is invalid')
    const canonicalSampleDirectory = resolve(sampleDirectory)
    const childPrefix = join(
      dirname(canonicalSampleDirectory),
      `.${basename(canonicalSampleDirectory)}${CHILD_STAGING_SEPARATOR}`,
    )
    let sampleHandle: FileHandle | undefined
    let childAttachmentRoot: string | undefined
    let childHandle: FileHandle | undefined
    let runnerStagingRoot: string | undefined
    let runnerHandle: FileHandle | undefined
    try {
      sampleHandle = await openOwnedDirectory(canonicalSampleDirectory, 'sample directory')
      childAttachmentRoot = resolve(await mkdtemp(childPrefix))
      childHandle = await openOwnedDirectory(childAttachmentRoot, 'child attachment staging')
      runnerStagingRoot = resolve(await mkdtemp(join(tmpdir(), RUNNER_STAGING_PREFIX)))
      runnerHandle = await openOwnedDirectory(runnerStagingRoot, 'runner attachment staging')
      return new BrowserSampleStaging(
        canonicalSampleDirectory,
        childAttachmentRoot,
        runnerStagingRoot,
        sampleHandle,
        childHandle,
        runnerHandle,
        faultCut,
      )
    } catch (cause) {
      const cleanupFailures = await settleOperations([
        ownedRemoval(childAttachmentRoot, childHandle, 'child attachment staging'),
        ownedRemoval(runnerStagingRoot, runnerHandle, 'runner attachment staging'),
        closeHandle(sampleHandle),
      ])
      if (cleanupFailures.length > 0) {
        throw new AggregateError(
          [cause, ...cleanupFailures],
          'browser sample staging creation and rollback both failed',
          { cause },
        )
      }
      throw cause
    }
  }

  get artifactRoot(): string {
    if (this.#collection === null) throw new Error('artifact collection is not finalized')
    return this.#collection.absoluteRoot
  }

  runnerPath(name: 'stdout.log' | 'stderr.log'): string {
    this.#requireState('active')
    return join(this.runnerStagingRoot, name)
  }

  childPath(...segments: readonly string[]): string {
    this.#requireState('active')
    return join(this.childAttachmentRoot, ...segments)
  }

  async finalize(): Promise<FinalizedArtifactCollection> {
    this.#requireState('active')
    this.#state = 'finalizing'
    await Promise.all([
      assertOwnedDirectory(this.sampleDirectory, this.#sampleHandle, 'sample directory'),
      assertOwnedDirectory(this.childAttachmentRoot, this.#childHandle, 'child attachment staging'),
      assertOwnedDirectory(this.runnerStagingRoot, this.#runnerHandle, 'runner attachment staging'),
    ])
    await requireAbsent(
      join(this.childAttachmentRoot, RUNNER_ARTIFACTS_DIRECTORY),
      'child attachment staging must not claim the reserved runner namespace',
    )
    await this.#applyFaultCut('replace-root-before-finalize')
    const runnerRoot = join(this.childAttachmentRoot, RUNNER_ARTIFACTS_DIRECTORY)
    await mkdir(runnerRoot, { mode: 0o700 })
    await requireAllSettled([
      copyTrustedFile(
        join(this.runnerStagingRoot, 'stdout.log'),
        join(runnerRoot, 'stdout.log'),
      ),
      copyTrustedFile(
        join(this.runnerStagingRoot, 'stderr.log'),
        join(runnerRoot, 'stderr.log'),
      ),
    ], 'trusted runner artifact copies did not all complete')
    await removeOwnedDirectory(
      this.runnerStagingRoot,
      this.#runnerHandle,
      'runner attachment staging',
    )
    this.#runnerStagingRemoved = true
    await Promise.all([
      assertOwnedDirectory(this.sampleDirectory, this.#sampleHandle, 'sample directory'),
      assertOwnedDirectory(this.childAttachmentRoot, this.#childHandle, 'finalized artifact collection'),
    ])
    this.#collection = Object.freeze({
      absoluteRoot: requireFinalizedArtifactCollectionRoot(
        this.sampleDirectory,
        this.childAttachmentRoot,
      ),
    })
    this.#state = 'finalized'
    await this.#applyFaultCut('fail-after-finalize')
    await this.#applyFaultCut('replace-root-and-fail-after-finalize')
    return this.#collection
  }

  /** Ownership transfers at the already-completed result commit. Handle release
   * is diagnostic cleanup and therefore settles without revoking that commit. */
  async commit(): Promise<StagingLifecycleSettlement> {
    this.#requireState('finalized')
    this.#state = 'committed'
    const failures = await settleOperations([
      closeHandle(this.#runnerHandle),
      closeHandle(this.#childHandle),
      closeHandle(this.#sampleHandle),
    ])
    return Object.freeze({ phase: 'ownership-transfer', failures: Object.freeze(failures) })
  }

  async dispose(): Promise<void> {
    if (this.#state === 'disposed') return
    if (this.#state === 'committed') {
      throw new Error('committed artifact collection cannot be rolled back')
    }
    await this.#applyFaultCut('replace-root-before-rollback')
    const failures = await settleOperations([
      removeOwnedDirectory(
        this.childAttachmentRoot,
        this.#childHandle,
        this.#collection === null ? 'child attachment staging' : 'finalized artifact collection',
      ),
      ...(this.#runnerStagingRemoved
        ? []
        : [removeOwnedDirectory(
            this.runnerStagingRoot,
            this.#runnerHandle,
            'runner attachment staging',
          )]),
    ])
    failures.push(...await settleOperations([
      closeHandle(this.#runnerHandle),
      closeHandle(this.#childHandle),
      closeHandle(this.#sampleHandle),
    ]))
    this.#state = 'disposed'
    if (failures.length > 0) {
      throw new AggregateError(failures, 'browser sample staging cleanup did not fully settle')
    }
  }

  async #applyFaultCut(expected: BrowserSampleStagingFaultCut): Promise<void> {
    if (this.#faultCut !== expected) return
    if (expected === 'fail-after-finalize') {
      await writeFile(
        join(this.childAttachmentRoot, STAGING_PARTIAL_COLLECTION_NAME),
        'partial',
        { encoding: 'utf8', flag: 'wx', mode: 0o600 },
      )
      throw new Error('declarative staging failure after finalization')
    }

    const ownedBackup = `${this.childAttachmentRoot}${STAGING_FAULT_BACKUP_SUFFIX}`
    await rename(this.childAttachmentRoot, ownedBackup)
    await mkdir(this.childAttachmentRoot, { mode: 0o700 })
    await writeFile(
      join(this.childAttachmentRoot, STAGING_FAULT_MARKER_NAME),
      STAGING_FAULT_MARKER_TEXT,
      { encoding: 'utf8', flag: 'wx', mode: 0o600 },
    )
    if (expected === 'replace-root-and-fail-after-finalize') {
      throw new Error('declarative staging path replacement after finalization')
    }
  }

  #requireState(expected: StagingState): void {
    if (this.#state !== expected) {
      throw new Error(`browser sample staging is ${this.#state}; expected ${expected}`)
    }
  }
}

async function copyTrustedFile(source: string, destination: string): Promise<void> {
  await copyFile(source, destination, constants.COPYFILE_EXCL)
}

async function openOwnedDirectory(path: string, label: string): Promise<FileHandle> {
  const handle = await open(path, 'r')
  try {
    await assertOwnedDirectory(path, handle, label)
    return handle
  } catch (cause) {
    await handle.close().catch(() => undefined)
    throw cause
  }
}

async function assertOwnedDirectory(path: string, handle: FileHandle, label: string): Promise<void> {
  const [opened, named] = await Promise.all([
    handle.stat({ bigint: true }),
    lstat(path, { bigint: true }),
  ])
  if (
    !opened.isDirectory() || !named.isDirectory() || named.isSymbolicLink() ||
    !sameFileIdentity(opened, named)
  ) {
    throw new Error(`${label} no longer names its owner-held directory`)
  }
}

async function removeOwnedDirectory(path: string, handle: FileHandle, label: string): Promise<void> {
  const cleanupPath = await quarantineOwnedDirectory(path, handle, label)
  await rm(cleanupPath, { recursive: true, force: false })
}

async function quarantineOwnedDirectory(
  path: string,
  handle: FileHandle,
  label: string,
): Promise<string> {
  await assertOwnedDirectory(path, handle, label)
  const cleanupPath = join(
    dirname(path),
    `.${basename(path)}-cleanup-${randomBytes(CLEANUP_NONCE_BYTES).toString('hex')}`,
  )
  await rename(path, cleanupPath)
  // A second descriptor/path comparison closes the pre-rename race. If the
  // named path was swapped, its bytes are left intact at the nonce tombstone.
  await assertOwnedDirectory(cleanupPath, handle, `${label} cleanup quarantine`)
  return cleanupPath
}

function sameFileIdentity(left: BigIntStats, right: BigIntStats): boolean {
  return left.dev === right.dev && left.ino === right.ino
}

async function requireAbsent(path: string, message: string): Promise<void> {
  try {
    await lstat(path)
  } catch (cause) {
    if (isMissingPath(cause)) return
    throw cause
  }
  throw new Error(message)
}

async function requireAllSettled(
  operations: readonly Promise<unknown>[],
  message: string,
): Promise<void> {
  const failures = await settleOperations(operations)
  if (failures.length > 0) throw new AggregateError(failures, message)
}

async function settleOperations(operations: readonly Promise<unknown>[]): Promise<unknown[]> {
  const settled = await Promise.allSettled(operations)
  return settled.flatMap((result) => result.status === 'rejected' ? [result.reason] : [])
}

function ownedRemoval(
  path: string | undefined,
  handle: FileHandle | undefined,
  label: string,
): Promise<void> {
  if (path === undefined) return Promise.resolve()
  if (handle === undefined) return Promise.reject(new Error(`${label} was created without an owner handle`))
  return removeOwnedDirectory(path, handle, label).finally(() => handle.close())
}

function closeHandle(handle: FileHandle | undefined): Promise<void> {
  return handle?.close() ?? Promise.resolve()
}

function isMissingPath(cause: unknown): boolean {
  return typeof cause === 'object' && cause !== null && 'code' in cause &&
    (cause as NodeJS.ErrnoException).code === 'ENOENT'
}
