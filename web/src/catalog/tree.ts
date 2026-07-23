import {
  type CatalogChild,
  type CatalogDirectoryFailure,
  type CatalogDirectoryGeneration,
  type CatalogDirectoryNode,
  type CatalogDirectoryState,
  type CatalogFileNode,
  type CatalogModifiedTime,
  type CatalogNamePolicy,
  type CatalogNode,
  type CatalogNodeId,
  type CatalogVisibleRow,
  type CatalogVisibleWindow,
  type DirectoryId,
  type FileId,
  type ScanAttemptId,
} from './model'

interface MutableDirectoryState {
  state: CatalogDirectoryState
}

interface VisibleNode {
  readonly node: CatalogNode
  readonly depth: number
}

interface VisibleDirectoryFrame {
  readonly children: readonly CatalogChild[]
  readonly depth: number
  nextIndex: number
}

export class CatalogTreeError extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'CatalogTreeError'
  }
}

/**
 * Stores only committed direct-child generations. Expansion is deliberately a
 * separate projection so collapsing UI rows cannot discard catalog identity.
 */
export class ProgressiveCatalogTree {
  readonly rootId: DirectoryId

  readonly #namePolicy: CatalogNamePolicy
  readonly #nodes = new Map<CatalogNodeId, CatalogNode>()
  readonly #directories = new Map<DirectoryId, MutableDirectoryState>()
  readonly #retryFallbacks = new Map<DirectoryId, CatalogDirectoryFailure>()
  readonly #expanded = new Set<DirectoryId>()

  constructor(rootId: DirectoryId, namePolicy: CatalogNamePolicy) {
    this.rootId = rootId
    this.#namePolicy = namePolicy
    const root = Object.freeze({
      kind: 'directory' as const,
      id: rootId,
      name: '',
      parentId: undefined,
    })
    this.#nodes.set(rootId, root)
    this.#directories.set(rootId, { state: Object.freeze({ status: 'undiscovered' }) })
    this.#expanded.add(rootId)
  }

  get nodeCount(): number {
    return this.#nodes.size
  }

  node(id: CatalogNodeId): CatalogNode | undefined {
    return this.#nodes.get(id)
  }

  requireNode(id: CatalogNodeId): CatalogNode {
    const node = this.node(id)
    if (node === undefined) {
      throw new CatalogTreeError('catalog node is unknown')
    }
    return node
  }

  requireDirectory(id: DirectoryId): CatalogDirectoryNode {
    const node = this.requireNode(id)
    if (node.kind !== 'directory') {
      throw new CatalogTreeError('catalog node is not a directory')
    }
    return node
  }

  requireFile(id: FileId): CatalogFileNode {
    const node = this.requireNode(id)
    if (node.kind !== 'file') {
      throw new CatalogTreeError('catalog node is not a file')
    }
    return node
  }

  directoryState(id: DirectoryId): CatalogDirectoryState {
    this.requireDirectory(id)
    const state = this.#directories.get(id)?.state
    if (state === undefined) {
      throw new CatalogTreeError('directory state is missing')
    }
    return state
  }

  beginDirectoryLoad(id: DirectoryId): boolean {
    const current = this.directoryState(id)
    if (current.status === 'ready' || current.status === 'loading') {
      return false
    }
    if (current.status === 'failed') {
      throw new CatalogTreeError(
        'a committed directory failure must be reused until an explicit retry is admitted',
      )
    }
    this.#directories.get(id)!.state = Object.freeze({ status: 'loading' })
    return true
  }

  abandonDirectoryLoad(id: DirectoryId): void {
    const current = this.directoryState(id)
    if (current.status === 'loading') {
      const retryFallback = this.#retryFallbacks.get(id)
      this.#directories.get(id)!.state = retryFallback === undefined
        ? Object.freeze({ status: 'undiscovered' })
        : Object.freeze({ status: 'failed', failure: retryFallback })
      this.#retryFallbacks.delete(id)
    }
  }

  /** The catalog client admits cooldown before invoking this explicit transition. */
  beginDirectoryRetry(id: DirectoryId, failedAttemptId: ScanAttemptId): void {
    const current = this.directoryState(id)
    if (current.status !== 'failed' || current.failure.kind !== 'retryable') {
      throw new CatalogTreeError('only a retryable directory failure may start another attempt')
    }
    if (current.failure.attemptId !== failedAttemptId) {
      throw new CatalogTreeError('directory retry does not match the failed scan attempt')
    }
    this.#retryFallbacks.set(id, current.failure)
    this.#directories.get(id)!.state = Object.freeze({ status: 'loading' })
  }

  failDirectory(id: DirectoryId, failure: CatalogDirectoryFailure): void {
    const current = this.directoryState(id)
    if (current.status === 'ready') {
      throw new CatalogTreeError('a committed directory generation cannot fail later')
    }
    const candidate = snapshotFailure(failure)
    if (current.status === 'failed') {
      if (sameDirectoryFailure(current.failure, candidate)) {
        return
      }
      throw new CatalogTreeError('a directory scan attempt has a conflicting terminal failure')
    }
    this.#retryFallbacks.delete(id)
    this.#directories.get(id)!.state = Object.freeze({
      status: 'failed',
      failure: candidate,
    })
  }

  publishDirectory(
    input: CatalogDirectoryGeneration,
  ): CatalogDirectoryGeneration {
    const directory = this.requireDirectory(input.directoryId)
    const current = this.directoryState(directory.id)
    const candidate = this.#validateGeneration(input, directory)
    if (current.status === 'ready') {
      if (sameGeneration(current.generation, candidate)) {
        return current.generation
      }
      throw new CatalogTreeError('a directory already has a different committed generation')
    }

    // Validation above is intentionally complete before either map changes.
    for (const child of candidate.children) {
      const node = materializeNode(directory.id, child)
      this.#nodes.set(node.id, node)
      if (node.kind === 'directory') {
        this.#directories.set(node.id, {
          state: Object.freeze({ status: 'undiscovered' }),
        })
      }
    }
    this.#directories.get(directory.id)!.state = Object.freeze({
      status: 'ready',
      generation: candidate,
    })
    this.#retryFallbacks.delete(directory.id)
    return candidate
  }

  children(id: DirectoryId): readonly CatalogNode[] | undefined {
    const state = this.directoryState(id)
    if (state.status !== 'ready') {
      return undefined
    }
    return Object.freeze(state.generation.children.map((child) => this.requireNode(child.id)))
  }

  setExpanded(id: DirectoryId, expanded: boolean): void {
    this.requireDirectory(id)
    if (id === this.rootId || expanded) {
      this.#expanded.add(id)
    } else {
      this.#expanded.delete(id)
    }
  }

  isExpanded(id: DirectoryId): boolean {
    return this.#expanded.has(id)
  }

  visibleWindow(maximumRows: number): CatalogVisibleWindow {
    if (!Number.isSafeInteger(maximumRows) || maximumRows <= 0) {
      throw new RangeError('maximum visible catalog rows must be a positive safe integer')
    }
    const rows: CatalogVisibleRow[] = []
    const rootChildren = this.#childDescriptors(this.rootId)
    const stack: VisibleDirectoryFrame[] = rootChildren.length === 0
      ? []
      : [{ children: rootChildren, depth: 0, nextIndex: 0 }]
    let truncated = false

    while (true) {
      const item = this.#nextVisibleNode(stack)
      if (item === undefined) {
        break
      }
      if (rows.length === maximumRows) {
        truncated = true
        break
      }
      rows.push(this.#visibleRow(item))
      this.#pushVisibleChildren(stack, item)
    }

    return Object.freeze({ rows: Object.freeze(rows), truncated })
  }

  ancestry(id: CatalogNodeId): readonly CatalogNode[] {
    const ancestry: CatalogNode[] = []
    let current: CatalogNode | undefined = this.requireNode(id)
    while (current !== undefined) {
      ancestry.push(current)
      current = current.parentId === undefined
        ? undefined
        : this.requireDirectory(current.parentId)
      if (ancestry.length > this.#nodes.size) {
        throw new CatalogTreeError('catalog ancestry contains a cycle')
      }
    }
    ancestry.reverse()
    return Object.freeze(ancestry)
  }

  outputPath(id: CatalogNodeId): readonly string[] {
    return Object.freeze(
      this.ancestry(id)
        .filter((node) => node.id !== this.rootId)
        .map((node) => node.name),
    )
  }

  isDescendant(nodeId: CatalogNodeId, ancestorId: DirectoryId): boolean {
    let current: CatalogNode | undefined = this.requireNode(nodeId)
    while (current.parentId !== undefined) {
      if (current.parentId === ancestorId) {
        return true
      }
      current = this.requireDirectory(current.parentId)
    }
    return false
  }

  #visibleRow(item: VisibleNode): CatalogVisibleRow {
    const { node, depth } = item
    return Object.freeze({
      node,
      depth,
      expanded: node.kind === 'directory' && this.isExpanded(node.id),
      ...(node.kind === 'directory'
        ? { directoryState: this.directoryState(node.id).status }
        : {}),
    })
  }

  #pushVisibleChildren(stack: VisibleDirectoryFrame[], item: VisibleNode): void {
    const { node, depth } = item
    if (node.kind !== 'directory' || !this.isExpanded(node.id)) {
      return
    }
    const children = this.#childDescriptors(node.id)
    if (children.length > 0) {
      stack.push({ children, depth: depth + 1, nextIndex: 0 })
    }
  }

  #nextVisibleNode(stack: VisibleDirectoryFrame[]): VisibleNode | undefined {
    while (stack.length > 0) {
      const frame = stack.at(-1)
      if (frame === undefined || frame.nextIndex >= frame.children.length) {
        stack.pop()
        continue
      }
      const child = frame.children[frame.nextIndex]
      frame.nextIndex += 1
      if (child !== undefined) {
        return { node: this.requireNode(child.id), depth: frame.depth }
      }
    }
    return undefined
  }

  #childDescriptors(id: DirectoryId): readonly CatalogChild[] {
    const state = this.directoryState(id)
    return state.status === 'ready' ? state.generation.children : []
  }

  #validateGeneration(
    input: CatalogDirectoryGeneration,
    directory: CatalogDirectoryNode,
  ): CatalogDirectoryGeneration {
    if (input.generation.length === 0) {
      throw new CatalogTreeError('directory generation identity must not be empty')
    }
    const children: CatalogChild[] = []
    const ids = new Set<CatalogNodeId>()
    const names = new Set<string>()
    for (const rawChild of input.children) {
      const child = snapshotChild(rawChild, this.#namePolicy)
      if (ids.has(child.id)) {
        throw new CatalogTreeError('directory generation repeats a child ID')
      }
      ids.add(child.id)
      const collisionKey = this.#namePolicy.collisionKey(child.name)
      if (names.has(collisionKey)) {
        throw new CatalogTreeError('directory generation contains a sibling name collision')
      }
      names.add(collisionKey)

      const existing = this.#nodes.get(child.id)
      if (existing !== undefined) {
        if (existing.parentId !== directory.id || !sameNodeAndChild(existing, child)) {
          throw new CatalogTreeError('catalog node identity is already bound to another object')
        }
      }
      if (child.kind === 'directory' && child.id === directory.id) {
        throw new CatalogTreeError('a directory cannot contain itself')
      }
      children.push(child)
    }
    return Object.freeze({
      directoryId: input.directoryId,
      generation: input.generation,
      children: Object.freeze(children),
    })
  }
}

function snapshotChild(child: CatalogChild, policy: CatalogNamePolicy): CatalogChild {
  const name = policy.validate(child.name)
  if (name !== child.name) {
    throw new CatalogTreeError('catalog name policy must validate without rewriting')
  }
  const modifiedTime = snapshotModifiedTime(child.modifiedTime)
  if (child.kind === 'directory') {
    return Object.freeze({ kind: 'directory', id: child.id, name, ...modifiedTime })
  }
  if (child.expectedSize < 0n) {
    throw new CatalogTreeError('catalog file size must not be negative')
  }
  return Object.freeze({
    kind: 'file',
    id: child.id,
    name,
    expectedSize: child.expectedSize,
    ...modifiedTime,
  })
}

function snapshotModifiedTime(
  modifiedTime: CatalogChild['modifiedTime'],
): { readonly modifiedTime?: CatalogModifiedTime } {
  if (modifiedTime === undefined) {
    return {}
  }
  if (modifiedTime.precisionMilliseconds <= 0n) {
    throw new CatalogTreeError('catalog modification-time precision must be positive')
  }
  return {
    modifiedTime: Object.freeze({
      milliseconds: modifiedTime.milliseconds,
      precisionMilliseconds: modifiedTime.precisionMilliseconds,
    }),
  }
}

function materializeNode(parentId: DirectoryId, child: CatalogChild): CatalogNode {
  if (child.kind === 'directory') {
    return Object.freeze({ ...child, parentId })
  }
  return Object.freeze({ ...child, parentId })
}

function sameNodeAndChild(node: CatalogNode, child: CatalogChild): boolean {
  return node.kind === child.kind &&
    node.id === child.id &&
    node.name === child.name &&
    (node.kind === 'directory' ||
      (child.kind === 'file' && node.expectedSize === child.expectedSize))
}

function sameGeneration(
  left: CatalogDirectoryGeneration,
  right: CatalogDirectoryGeneration,
): boolean {
  if (
    left.directoryId !== right.directoryId ||
    left.generation !== right.generation ||
    left.children.length !== right.children.length
  ) {
    return false
  }
  return left.children.every((child, index) => {
    const other = right.children[index]
    return other !== undefined &&
      child.kind === other.kind &&
      child.id === other.id &&
      child.name === other.name &&
      (child.kind === 'directory' ||
        (other.kind === 'file' && child.expectedSize === other.expectedSize)) &&
      sameModifiedTime(child.modifiedTime, other.modifiedTime)
  })
}

function sameModifiedTime(
  left: CatalogChild['modifiedTime'],
  right: CatalogChild['modifiedTime'],
): boolean {
  return left === undefined
    ? right === undefined
    : right !== undefined &&
      left.milliseconds === right.milliseconds &&
      left.precisionMilliseconds === right.precisionMilliseconds
}

function snapshotFailure(failure: CatalogDirectoryFailure): CatalogDirectoryFailure {
  if (typeof failure.attemptId !== 'string' || failure.attemptId.length === 0) {
    throw new CatalogTreeError('catalog scan attempt identity must not be empty')
  }
  if (failure.message.length === 0) {
    throw new CatalogTreeError('catalog directory failure message must not be empty')
  }
  if (
    failure.retryAfterMilliseconds !== undefined &&
    (!Number.isSafeInteger(failure.retryAfterMilliseconds) ||
      failure.retryAfterMilliseconds < 0)
  ) {
    throw new CatalogTreeError('catalog retry delay must be a non-negative safe integer')
  }
  if (failure.kind === 'retryable' && failure.retryAfterMilliseconds === undefined) {
    throw new CatalogTreeError('retryable catalog failure must include a retry delay')
  }
  if (failure.kind !== 'retryable' && failure.retryAfterMilliseconds !== undefined) {
    throw new CatalogTreeError('only a retryable catalog failure may include a retry delay')
  }
  return Object.freeze({
    attemptId: failure.attemptId,
    kind: failure.kind,
    message: failure.message,
    ...(failure.retryAfterMilliseconds === undefined
      ? {}
      : { retryAfterMilliseconds: failure.retryAfterMilliseconds }),
  })
}

function sameDirectoryFailure(
  left: CatalogDirectoryFailure,
  right: CatalogDirectoryFailure,
): boolean {
  return left.attemptId === right.attemptId &&
    left.kind === right.kind &&
    left.message === right.message &&
    left.retryAfterMilliseconds === right.retryAfterMilliseconds
}
