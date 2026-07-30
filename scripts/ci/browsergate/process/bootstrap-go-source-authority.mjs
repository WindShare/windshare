import { mkdir, mkdtemp, readdir, rm, writeFile } from 'node:fs/promises'
import { basename, dirname, extname, join, resolve } from 'node:path'

export const BOOTSTRAP_GO_WORKSPACE_MODE = 'off'

const GO_SOURCE_FILENAME_PATTERN = /^[A-Za-z0-9][A-Za-z0-9_.-]*\.go$/u
const GO_TEST_SOURCE_SUFFIX = '_test.go'
const UNSUPPORTED_PACKAGE_SOURCE_EXTENSIONS = new Set([
  '.c', '.cc', '.cpp', '.cxx', '.f', '.f90', '.for', '.h', '.hh', '.hpp', '.m',
  '.s', '.S', '.swig', '.swigcxx', '.syso',
])
const UNSUPPORTED_COMPILED_SOURCE_FIELDS = Object.freeze([
  'CgoFiles',
  'CFiles',
  'CXXFiles',
  'MFiles',
  'HFiles',
  'FFiles',
  'SFiles',
  'SwigFiles',
  'SwigCXXFiles',
  'SysoFiles',
  'EmbedFiles',
  'InvalidGoFiles',
  'CompiledGoFiles',
])
const OWNER_PACKAGES = Object.freeze({
  linux: Object.freeze({
    platform: 'linux',
    goOS: 'linux',
    kind: 'linux-process-owner',
    packagePath: './web/scripts/browser-evidence/linuxprocessowner',
    importPath: 'github.com/windshare/windshare/web/scripts/browser-evidence/linuxprocessowner',
  }),
  win32: Object.freeze({
    platform: 'win32',
    goOS: 'windows',
    kind: 'windows-job',
    packagePath: './web/scripts/browser-evidence/windowsjob',
    importPath: 'github.com/windshare/windshare/web/scripts/browser-evidence/windowsjob',
  }),
})

export function bootstrapOwnerPackage(platform, packagePath) {
  const owner = OWNER_PACKAGES[platform]
  if (owner === undefined || packagePath !== owner.packagePath) {
    throw new Error('bootstrap build is restricted to the current platform owner package')
  }
  return owner
}

export function parseBootstrapGoSourceInventory(encoded, { repositoryRoot, owner }) {
  if (typeof encoded !== 'string' || encoded === '') {
    throw new Error('bootstrap Go package inventory must be nonempty JSON text')
  }
  let value
  try {
    value = JSON.parse(encoded)
  } catch (cause) {
    throw new Error('bootstrap Go package inventory is invalid JSON', { cause })
  }
  if (value === null || typeof value !== 'object' || Array.isArray(value)) {
    throw new Error('bootstrap Go package inventory must be an object')
  }
  const packageDirectory = join(
    repositoryRoot,
    ...owner.packagePath.slice(2).split('/'),
  )
  if (
    value.Dir !== packageDirectory || value.Root !== repositoryRoot ||
    value.ImportPath !== owner.importPath || value.Name !== 'main' ||
    value.Incomplete === true || value.Error !== undefined ||
    value.DepsErrors !== undefined &&
      (!Array.isArray(value.DepsErrors) || value.DepsErrors.length > 0)
  ) {
    throw new Error('bootstrap Go package inventory identifies an unexpected package')
  }
  if (
    value.Module === null || typeof value.Module !== 'object' || Array.isArray(value.Module) ||
    value.Module.Path !== 'github.com/windshare/windshare' || value.Module.Main !== true ||
    value.Module.Dir !== repositoryRoot || value.Module.GoMod !== join(repositoryRoot, 'go.mod')
  ) {
    throw new Error('bootstrap Go package inventory is outside the bootstrap module authority')
  }
  for (const field of UNSUPPORTED_COMPILED_SOURCE_FIELDS) {
    const files = value[field]
    if (files !== undefined && (!Array.isArray(files) || files.length > 0)) {
      throw new Error(`bootstrap Go package uses unsupported compiled input field ${field}`)
    }
  }
  if (!Array.isArray(value.GoFiles) || value.GoFiles.length < 1) {
    throw new Error('bootstrap Go package has no production Go sources')
  }
  const filenames = value.GoFiles.map((filename) => {
    if (
      typeof filename !== 'string' || !GO_SOURCE_FILENAME_PATTERN.test(filename) ||
      basename(filename) !== filename || filename.endsWith(GO_TEST_SOURCE_SUFFIX)
    ) throw new Error('bootstrap Go package contains a non-canonical production source filename')
    return filename
  })
  const canonicalFilenames = [...new Set(filenames)].sort()
  if (canonicalFilenames.length !== filenames.length) {
    throw new Error('bootstrap Go package repeats a production source')
  }
  const relativeDirectory = owner.packagePath.slice(2)
  return Object.freeze({
    platform: owner.platform,
    goOS: owner.goOS,
    kind: owner.kind,
    packagePath: owner.packagePath,
    packageDirectory,
    relativePaths: Object.freeze(canonicalFilenames.map(
      (filename) => `${relativeDirectory}/${filename}`,
    )),
  })
}

export function assertBootstrapGoSourceInventory(expected, observed) {
  if (
    observed === null || typeof observed !== 'object' ||
    expected.platform !== observed.platform || expected.goOS !== observed.goOS ||
    expected.kind !== observed.kind || expected.packagePath !== observed.packagePath ||
    expected.packageDirectory !== observed.packageDirectory ||
    !sameStrings(expected.relativePaths, observed.relativePaths)
  ) throw new Error('bootstrap Go package source inventory changed across the build')
}

export async function createBootstrapGoSourceAuthority({
  repositoryRoot,
  runtimeRoot,
  owner,
  metadataRelativePaths,
  maximumSourceBytes,
  holdFile,
}) {
  requireSourceAuthorityOptions({ metadataRelativePaths, maximumSourceBytes, holdFile })
  const snapshotPrefix = join(runtimeRoot, `.bootstrap-module-${owner.kind}-`)
  if (dirname(snapshotPrefix) !== runtimeRoot || resolve(snapshotPrefix) !== snapshotPrefix) {
    throw new Error('bootstrap module snapshot root escaped its runtime authority')
  }
  const relativeDirectory = owner.packagePath.slice(2)
  const sourcePackageRoot = join(repositoryRoot, ...relativeDirectory.split('/'))
  const candidateRelativePaths = await productionGoCandidatePaths(
    sourcePackageRoot,
    relativeDirectory,
  )
  const metadata = []
  const candidates = []
  let snapshotRoot
  let snapshotCreated = false
  let closed = false
  try {
    snapshotRoot = await mkdtemp(snapshotPrefix)
    snapshotCreated = true
    await mkdir(join(snapshotRoot, ...relativeDirectory.split('/')), {
      recursive: true,
      mode: 0o700,
    })
    for (const relativePath of metadataRelativePaths) {
      metadata.push(await holdAndSnapshot({
        repositoryRoot,
        snapshotRoot,
        relativePath,
        maximumSourceBytes,
        holdFile,
      }))
    }
    for (const relativePath of candidateRelativePaths) {
      candidates.push(await holdAndSnapshot({
        repositoryRoot,
        snapshotRoot,
        relativePath,
        maximumSourceBytes,
        holdFile,
      }))
    }
    const snapshotPackageRoot = join(snapshotRoot, ...relativeDirectory.split('/'))
    const authority = Object.freeze({
      moduleRoot: snapshotRoot,
      metadataSources: Object.freeze(metadata.map(originalSourceRecord)),
      candidateRelativePaths,
      async readSnapshotMetadata(relativePath) {
        if (closed) throw new Error('bootstrap Go source authority is closed')
        const record = metadata.find((source) => source.relativePath === relativePath)
        if (record === undefined) {
          throw new Error('bootstrap Go source authority omitted requested metadata')
        }
        return record.snapshot.readBytes()
      },
      select(inventory) {
        if (closed) throw new Error('bootstrap Go source authority is closed')
        if (inventory.packageDirectory !== snapshotPackageRoot) {
          throw new Error('bootstrap Go inventory did not originate from the private module snapshot')
        }
        const selected = inventory.relativePaths.map((relativePath) => {
          const candidate = candidates.find((record) => record.relativePath === relativePath)
          if (candidate === undefined) {
            throw new Error('bootstrap Go inventory selected a source outside the held candidate set')
          }
          return candidate
        })
        return Object.freeze({
          inventory,
          buildPaths: Object.freeze(selected.map(({ snapshot }) => snapshot.path)),
          receiptSources: Object.freeze([
            ...metadata.map(originalSourceRecord),
            ...selected.map(originalSourceRecord),
          ]),
        })
      },
      async assertLive() {
        if (closed) throw new Error('bootstrap Go source authority is closed')
        const [sourceCandidates, snapshotCandidates] = await Promise.all([
          productionGoCandidatePaths(sourcePackageRoot, relativeDirectory),
          productionGoCandidatePaths(snapshotPackageRoot, relativeDirectory),
        ])
        if (
          !sameStrings(candidateRelativePaths, sourceCandidates) ||
          !sameStrings(candidateRelativePaths, snapshotCandidates)
        ) throw new Error('bootstrap Go package candidate inventory changed across the build')
        await Promise.all([
          ...metadata.flatMap(authorityAssertions),
          ...candidates.flatMap(authorityAssertions),
        ])
      },
      async close() {
        if (closed) return
        closed = true
        await Promise.allSettled([
          ...metadata.flatMap(authorityClosures),
          ...candidates.flatMap(authorityClosures),
        ])
        await rm(snapshotRoot, { recursive: true, force: true })
      },
    })
    return authority
  } catch (cause) {
    closed = true
    await Promise.allSettled([
      ...metadata.flatMap(authorityClosures),
      ...candidates.flatMap(authorityClosures),
    ])
    if (snapshotCreated) await rm(snapshotRoot, { recursive: true, force: true }).catch(() => undefined)
    throw cause
  }
}

async function productionGoCandidatePaths(packageRoot, relativeDirectory) {
  const entries = await readdir(packageRoot, { withFileTypes: true })
  const candidates = []
  for (const entry of entries) {
    const extension = extname(entry.name)
    if (UNSUPPORTED_PACKAGE_SOURCE_EXTENSIONS.has(extension)) {
      throw new Error(`bootstrap Go package contains unsupported source ${entry.name}`)
    }
    if (!entry.name.endsWith('.go') || entry.name.endsWith(GO_TEST_SOURCE_SUFFIX)) continue
    if (!GO_SOURCE_FILENAME_PATTERN.test(entry.name)) {
      throw new Error('bootstrap Go package contains a non-canonical source filename')
    }
    candidates.push(`${relativeDirectory}/${entry.name}`)
  }
  if (candidates.length < 1) throw new Error('bootstrap Go package has no source candidates')
  return Object.freeze(candidates.sort())
}

async function holdAndSnapshot({
  repositoryRoot,
  snapshotRoot,
  relativePath,
  maximumSourceBytes,
  holdFile,
}) {
  const original = await holdFile(
    join(repositoryRoot, ...relativePath.split('/')),
    maximumSourceBytes,
    `bootstrap source ${relativePath}`,
  )
  let snapshot
  try {
    const bytes = await original.readBytes()
    const snapshotPath = join(snapshotRoot, ...relativePath.split('/'))
    try {
      await writeFile(snapshotPath, bytes, { flag: 'wx', mode: 0o600 })
    } finally {
      bytes.fill(0)
    }
    snapshot = await holdFile(
      snapshotPath,
      maximumSourceBytes,
      `bootstrap source snapshot ${relativePath}`,
    )
    if (snapshot.byteLength !== original.byteLength || snapshot.sha256 !== original.sha256) {
      throw new Error(`bootstrap source snapshot differs from ${relativePath}`)
    }
    return Object.freeze({ relativePath, original, snapshot })
  } catch (cause) {
    await Promise.allSettled([
      original.close(),
      ...(snapshot === undefined ? [] : [snapshot.close()]),
    ])
    throw cause
  }
}

function requireSourceAuthorityOptions({ metadataRelativePaths, maximumSourceBytes, holdFile }) {
  if (
    !Array.isArray(metadataRelativePaths) || metadataRelativePaths.length < 1 ||
    metadataRelativePaths.some((path) =>
      typeof path !== 'string' || path === '' || path === '.' || path === '..' ||
      path.includes('/') || path.includes('\\') || basename(path) !== path,
    ) || new Set(metadataRelativePaths).size !== metadataRelativePaths.length
  ) throw new Error('bootstrap module metadata paths are invalid')
  if (!Number.isSafeInteger(maximumSourceBytes) || maximumSourceBytes < 1) {
    throw new Error('bootstrap maximum source bytes must be positive')
  }
  if (typeof holdFile !== 'function') throw new Error('bootstrap source holder is required')
}

function originalSourceRecord({ relativePath, original }) {
  return Object.freeze({ relativePath, authority: original })
}

function authorityAssertions({ original, snapshot }) {
  return [original.assertLive(), snapshot.assertLive()]
}

function authorityClosures({ original, snapshot }) {
  return [original.close(), snapshot.close()]
}

function sameStrings(left, right) {
  return Array.isArray(left) && Array.isArray(right) && left.length === right.length &&
    left.every((value, index) => value === right[index])
}
