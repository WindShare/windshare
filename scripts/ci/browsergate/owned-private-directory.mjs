import { randomBytes } from 'node:crypto'
import {
  lstatSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  realpathSync,
  renameSync,
  rmSync,
} from 'node:fs'
import { tmpdir } from 'node:os'
import { basename, dirname, isAbsolute, join, relative, resolve, sep } from 'node:path'

const CHILD_NAME_PATTERN = /^[a-z][a-z0-9-]{0,63}$/u
const TOMBSTONE_SUFFIX = '.windshare-retired-'

const DEFAULT_STORAGE = Object.freeze({
  allocateRoot(prefix) {
    return resolve(mkdtempSync(join(tmpdir(), prefix)))
  },
  createDirectory(path) {
    mkdirSync(path, { mode: 0o700 })
  },
  canonicalize(path) {
    return resolve(realpathSync.native(path))
  },
  inspect(path) {
    return lstatSync(path, { bigint: true })
  },
  list(path) {
    return readdirSync(path)
  },
  move(source, destination) {
    renameSync(source, destination)
  },
  remove(path) {
    rmSync(path, { recursive: true, force: true })
  },
})

export function createOwnedPrivateDirectoryAuthority({
  prefix,
  childNames,
  forbiddenPaths = [],
  storage = DEFAULT_STORAGE,
  tombstoneNonceFactory = () => randomBytes(16).toString('hex'),
  trace = () => undefined,
} = {}) {
  const validatedPrefix = requirePrefix(prefix)
  const children = requireChildNames(childNames)
  const io = requireStorage(storage)
  const forbidden = Object.freeze(forbiddenPaths.map((path, index) =>
    requireCanonicalPath(
      io.canonicalize(requireCanonicalPath(path, `owned private directory forbidden path ${index}`)),
      `owned private directory physical forbidden path ${index}`,
    )))
  requireFunction(tombstoneNonceFactory, 'owned private directory tombstone nonce factory')
  requireFunction(trace, 'owned private directory trace sink')

  const allocatedPath = requireCanonicalPath(
    io.canonicalize(requireCanonicalPath(
      io.allocateRoot(validatedPrefix),
      'owned private directory allocated root',
    )),
    'owned private directory allocated root',
  )
  let rootIdentity
  let state
  try {
    if (forbidden.some((path) => pathsOverlap(allocatedPath, path))) {
      throw new Error('owned private directory must not overlap a forbidden path')
    }
    if (io === DEFAULT_STORAGE) {
      const physicalTemporaryRoot = requireCanonicalPath(
        io.canonicalize(resolve(tmpdir())),
        'owned private directory physical temporary root',
      )
      if (dirname(allocatedPath) !== physicalTemporaryRoot || !basename(allocatedPath).startsWith(validatedPrefix)) {
        throw new Error('owned private directory default allocation escaped its exact temporary namespace')
      }
    }
    const rootMetadata = requireDirectoryMetadata(io, allocatedPath, 'owned private directory root')
    requireExactEntries(io, allocatedPath, [], 'fresh owned private directory root', rootMetadata)
    rootIdentity = directoryIdentity(rootMetadata)

    const childPaths = {}
    const childIdentities = {}
    for (const name of children) {
      const path = resolve(allocatedPath, name)
      io.createDirectory(path)
      childPaths[name] = path
      childIdentities[name] = directoryIdentity(
        requireDirectoryMetadata(io, path, `owned private directory child ${name}`),
      )
    }
    state = Object.freeze({
      outcome: 'active',
      root: allocatedPath,
      rootIdentity,
      childPaths: Object.freeze(childPaths),
      childIdentities: Object.freeze(childIdentities),
    })
    revalidate()
    emitTrace(trace, 'private-directory-allocated', { root: state.root })
  } catch (error) {
    if (rootIdentity === undefined) throw error
    try {
      retireOwnedRoot(io, allocatedPath, rootIdentity, tombstoneNonceFactory)
    } catch (cleanupError) {
      throw new AggregateError(
        [error, cleanupError],
        'owned private directory construction and rollback both failed',
      )
    }
    throw error
  }

  function paths() {
    revalidate()
    return Object.freeze({ root: state.root, ...state.childPaths })
  }

  function revalidate() {
    if (state?.outcome !== 'active') {
      throw new Error('owned private directory authority is not active')
    }
    const rootMetadata = requireDirectoryMetadata(io, state.root, 'owned private directory root')
    assertIdentity(rootMetadata, state.rootIdentity, 'owned private directory root')
    requireExactEntries(io, state.root, children, 'owned private directory root', rootMetadata)
    for (const name of children) {
      const metadata = requireDirectoryMetadata(
        io,
        state.childPaths[name],
        `owned private directory child ${name}`,
      )
      assertIdentity(metadata, state.childIdentities[name], `owned private directory child ${name}`)
    }
    return true
  }

  function dispose() {
    if (state.outcome === 'disposed') return
    if (state.outcome === 'compromised') {
      throw new Error('owned private directory cleanup previously refused a foreign replacement')
    }
    if (state.outcome === 'active') {
      revalidate()
      const tombstone = tombstonePath(state.root, tombstoneNonceFactory)
      try {
        io.move(state.root, tombstone)
      } catch (error) {
        throw new Error(`owned private directory quarantine failed: ${errorMessage(error)}`, {
          cause: error,
        })
      }
      state = Object.freeze({ ...state, outcome: 'quarantined', tombstone })
    }
    let metadata
    try {
      metadata = requireDirectoryMetadata(io, state.tombstone, 'owned private directory tombstone')
      assertIdentity(metadata, state.rootIdentity, 'owned private directory tombstone')
    } catch (error) {
      state = Object.freeze({ ...state, outcome: 'compromised' })
      throw error
    }
    io.remove(state.tombstone)
    requireAbsent(io, state.tombstone, 'owned private directory tombstone')
    const retiredRoot = state.root
    state = Object.freeze({ outcome: 'disposed' })
    emitTrace(trace, 'private-directory-disposed', { root: retiredRoot })
  }

  return Object.freeze({ dispose, paths, revalidate })
}

function retireOwnedRoot(io, root, identity, tombstoneNonceFactory) {
  const metadata = requireDirectoryMetadata(io, root, 'owned private directory rollback root')
  assertIdentity(metadata, identity, 'owned private directory rollback root')
  const tombstone = tombstonePath(root, tombstoneNonceFactory)
  io.move(root, tombstone)
  const quarantined = requireDirectoryMetadata(io, tombstone, 'owned private directory rollback tombstone')
  assertIdentity(quarantined, identity, 'owned private directory rollback tombstone')
  io.remove(tombstone)
  requireAbsent(io, tombstone, 'owned private directory rollback tombstone')
}

function tombstonePath(root, nonceFactory) {
  const nonce = nonceFactory()
  if (typeof nonce !== 'string' || !/^[0-9a-f]{32}$/u.test(nonce)) {
    throw new Error('owned private directory tombstone nonce must contain exactly 16 random bytes')
  }
  const tombstone = resolve(dirname(root), `${root.slice(dirname(root).length + 1)}${TOMBSTONE_SUFFIX}${nonce}`)
  if (dirname(tombstone) !== dirname(root) || tombstone === root) {
    throw new Error('owned private directory tombstone escaped its exact parent')
  }
  return tombstone
}

function requireExactEntries(io, path, expected, label, expectedMetadata) {
  assertIdentity(expectedMetadata, directoryIdentity(expectedMetadata), label)
  const actual = io.list(path).sort((left, right) => left.localeCompare(right))
  const after = requireDirectoryMetadata(io, path, label)
  assertIdentity(after, directoryIdentity(expectedMetadata), label)
  const ordered = [...expected].sort((left, right) => left.localeCompare(right))
  if (actual.length !== ordered.length || actual.some((entry, index) => entry !== ordered[index])) {
    throw new Error(`${label} contains an unauthorized entry`)
  }
}

function requireDirectoryMetadata(io, path, label) {
  let metadata
  try {
    metadata = io.inspect(path)
  } catch (error) {
    throw new Error(`${label} is unavailable: ${errorMessage(error)}`, { cause: error })
  }
  if (metadata.isSymbolicLink() || !metadata.isDirectory()) {
    throw new Error(`${label} must be one non-symlink directory`)
  }
  if (typeof metadata.dev !== 'bigint' || typeof metadata.ino !== 'bigint') {
    throw new Error(`${label} inspection must preserve BigInt filesystem identity`)
  }
  return metadata
}

function directoryIdentity(metadata) {
  return Object.freeze({ dev: metadata.dev, ino: metadata.ino })
}

function assertIdentity(metadata, identity, label) {
  if (metadata.dev !== identity.dev || metadata.ino !== identity.ino) {
    throw new Error(`${label} no longer identifies the authority-owned directory`)
  }
}

function requireAbsent(io, path, label) {
  try {
    io.inspect(path)
  } catch (error) {
    if (error?.code === 'ENOENT') return
    throw new Error(`${label} absence could not be proven: ${errorMessage(error)}`, { cause: error })
  }
  throw new Error(`${label} remains after recursive removal`)
}

function requireStorage(value) {
  if (!isRecord(value)) throw new Error('owned private directory storage must be an object')
  for (const name of ['allocateRoot', 'canonicalize', 'createDirectory', 'inspect', 'list', 'move', 'remove']) {
    requireFunction(value[name], `owned private directory storage ${name}`)
  }
  return value
}

function requireChildNames(value) {
  if (!Array.isArray(value) || value.length < 1 || value.some((name) => (
    typeof name !== 'string' || !CHILD_NAME_PATTERN.test(name)
  ))) throw new Error('owned private directory requires safe named children')
  if (new Set(value).size !== value.length) {
    throw new Error('owned private directory child names must be unique')
  }
  return Object.freeze([...value])
}

function requirePrefix(value) {
  if (typeof value !== 'string' || !CHILD_NAME_PATTERN.test(value.replace(/-$/u, ''))) {
    throw new Error('owned private directory prefix is invalid')
  }
  return value
}

function requireCanonicalPath(value, label) {
  if (typeof value !== 'string' || !isAbsolute(value) || resolve(value) !== value) {
    throw new Error(`${label} must be an absolute canonical path`)
  }
  return value
}

function pathsOverlap(left, right) {
  const leftToRight = relative(left, right)
  const rightToLeft = relative(right, left)
  return leftToRight === ''
    || (leftToRight !== '..' && !leftToRight.startsWith(`..${sep}`) && !isAbsolute(leftToRight))
    || (rightToLeft !== '..' && !rightToLeft.startsWith(`..${sep}`) && !isAbsolute(rightToLeft))
}

function emitTrace(trace, milestone, context) {
  try {
    trace(milestone, context)
  } catch {
    // Observers cannot strand an ownership transition or resurrect retired authority.
  }
}

function requireFunction(value, label) {
  if (typeof value !== 'function') throw new Error(`${label} must be a function`)
}

function errorMessage(error) {
  return error instanceof Error ? error.message : String(error)
}

function isRecord(value) {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}
