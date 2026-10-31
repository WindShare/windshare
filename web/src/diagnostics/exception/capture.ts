import {
  MAX_EXCEPTION_BRANCHES, MAX_EXCEPTION_BYTES, MAX_EXCEPTION_DEPTH,
  MAX_EXCEPTION_NODES, MAX_EXCEPTION_TEXT_BYTES,
  type DiagnosticExceptionProjection, type DiagnosticExceptionTextProjector,
} from './model'
import { isNativeDOMException, isNativeError, isNativeTypeError, javascriptExceptionKind, nativeErrorClass } from './native'
import { boundedDiagnosticText, diagnosticBytes } from './text'

const NODE_OVERHEAD_BYTES = 512
const identityText: DiagnosticExceptionTextProjector = value => value
const associated = new WeakMap<object, DiagnosticExceptionProjection>()
interface Budget { bytes: number; nodes: number }
type Read = Readonly<{ value: unknown; available: boolean }>

export function associateDiagnosticException(value: unknown, projection: DiagnosticExceptionProjection): void {
  if (isObject(value)) associated.set(value, projection)
}
export function projectDiagnosticException(
  thrown: unknown, projectText: DiagnosticExceptionTextProjector = identityText,
): DiagnosticExceptionProjection {
  const snapshot = capture(thrown, projectText, { bytes: MAX_EXCEPTION_BYTES, nodes: MAX_EXCEPTION_NODES }, new Set(), 0)
  if (projectText === identityText && isObject(thrown)) associated.set(thrown, snapshot)
  return snapshot
}

function dimensions(node: DiagnosticExceptionProjection): { nodes: number; depth: number } {
  const children = [...node.errors, ...(node.cause === null ? [] : [node.cause])].map(dimensions)
  return { nodes: 1 + children.reduce((sum, child) => sum + child.nodes, 0),
    depth: children.length === 0 ? 0 : 1 + Math.max(...children.map(child => child.depth)) }
}
function capture(
  thrown: unknown, projector: DiagnosticExceptionTextProjector,
  budget: Budget, ancestors: Set<unknown>, depth: number,
): DiagnosticExceptionProjection {
  const reusable = isObject(thrown) ? associated.get(thrown) : undefined
  if (reusable !== undefined && projector === identityText) {
    const size = dimensions(reusable)
    const bytes = diagnosticBytes(reusable)
    if (size.nodes <= budget.nodes && depth + size.depth <= MAX_EXCEPTION_DEPTH && bytes <= budget.bytes) {
      budget.nodes -= size.nodes
      budget.bytes -= bytes
      return reusable
    }
  }
  return new ExceptionCapture(thrown, projector, budget, ancestors, depth).snapshot()
}

/** One shared budget covers the whole graph, including escaped text and node overhead. */
class ExceptionCapture {
  readonly #truncated: string[] = []
  readonly #unavailable: string[] = []
  #omittedBranches = 0
  readonly thrown: unknown
  readonly projector: DiagnosticExceptionTextProjector
  readonly budget: Budget
  readonly ancestors: Set<unknown>
  readonly depth: number
  constructor(
    thrown: unknown, projector: DiagnosticExceptionTextProjector,
    budget: Budget, ancestors: Set<unknown>, depth: number,
  ) {
    this.thrown = thrown
    this.projector = projector
    this.budget = budget
    this.ancestors = ancestors
    this.depth = depth
    budget.bytes -= NODE_OVERHEAD_BYTES
    budget.nodes--
  }

  snapshot(): DiagnosticExceptionProjection {
    const dom = isNativeDOMException(this.thrown) ? this.thrown : undefined
    const type = isNativeTypeError(this.thrown) ? this.thrown : undefined
    const error = dom ?? (isNativeError(this.thrown) ? this.thrown : undefined)
    const constructorName = this.#constructorName()
    const rawName = error === undefined ? undefined : this.#read(error, 'name').value
    const errorName = this.#text(rawName, 'errorName')
    const message = error === undefined ? null : this.#text(this.#read(error, 'message').value, 'message')
    const thrownValue = error === undefined ? this.#text(printableThrown(this.thrown), 'thrownValue') : null
    this.ancestors.add(this.thrown)
    const cause = this.#cause()
    const errors = this.#aggregate()
    // Summaries and causes have priority over the often-repetitive outer stack.
    const stack = error === undefined ? null : this.#text(this.#read(error, 'stack').value, 'stack')
    this.ancestors.delete(this.thrown)
    const result = Object.freeze({
      javascriptKind: javascriptExceptionKind(dom, type),
      nativeClass: nativeErrorClass(dom, type, typeof rawName === 'string' ? rawName : null),
      thrownType: this.thrown === null ? 'null' : typeof this.thrown,
      constructorName, errorName, message, stack, thrownValue, cause,
      errors: Object.freeze(errors), omittedBranches: this.#omittedBranches,
      truncated: Object.freeze(this.#truncated), unavailable: Object.freeze(this.#unavailable),
    })
    return result
  }

  #read(value: unknown, key: string): Read {
    try { return { value: Reflect.get(value as object, key), available: true } }
    catch { this.#unavailable.push(key); return { value: undefined, available: false } }
  }

  #constructorName(): string | null {
    const constructor = isObject(this.thrown) ? this.#read(this.thrown, 'constructor').value : undefined
    return typeof constructor === 'function' ? this.#text(this.#read(constructor, 'name').value, 'constructorName') : null
  }

  #text(value: unknown, field: string): string | null {
    if (value === undefined || value === null) return null
    let source: string
    try { source = String(value) } catch {
      this.#unavailable.push(field)
      source = '[unprintable thrown value]'
    }
    const capacity = Math.min(MAX_EXCEPTION_TEXT_BYTES, this.budget.bytes)
    const bounded = boundedDiagnosticText(source, capacity)
    let projected = bounded
    try { projected = boundedDiagnosticText(this.projector(bounded), capacity) }
    catch { this.#unavailable.push(field) }
    if (projected.length < source.length) this.#truncated.push(field)
    this.budget.bytes -= diagnosticBytes(projected) - 2
    return projected
  }

  #branch(value: unknown, field: string): DiagnosticExceptionProjection | null {
    const reason = this.#omissionReason(value)
    if (reason !== undefined) {
      this.#truncated.push(field + ':' + reason)
      this.#omittedBranches++
      return null
    }
    return capture(value, this.projector, this.budget, this.ancestors, this.depth + 1)
  }
  #omissionReason(value: unknown): string | undefined {
    if (this.ancestors.has(value)) return 'cycle'
    if (this.depth >= MAX_EXCEPTION_DEPTH) return 'depth'
    if (this.budget.nodes === 0 || this.budget.bytes < NODE_OVERHEAD_BYTES) return 'budget'
    return undefined
  }
  #cause(): DiagnosticExceptionProjection | null {
    if (!isObject(this.thrown)) return null
    const cause = this.#read(this.thrown, 'cause')
    return cause.value === undefined ? null : this.#branch(cause.value, 'cause')
  }

  #aggregate(): DiagnosticExceptionProjection[] {
    if (!isObject(this.thrown)) return []
    const aggregate = this.#read(this.thrown, 'errors').value
    if (aggregate === undefined) return []
    try {
      if (Array.isArray(aggregate)) return this.#arrayBranches(aggregate)
      this.#unavailable.push('errors:non-array')
    } catch { this.#unavailable.push('errors') }
    return []
  }

  #arrayBranches(aggregate: unknown[]): DiagnosticExceptionProjection[] {
    // Array prefixes avoid invoking arbitrary iterators or traversing huge aggregates.
    const errors: DiagnosticExceptionProjection[] = []
    const length = aggregate.length
    if (!Number.isSafeInteger(length) || length < 0) {
      this.#unavailable.push('errors:length')
      return errors
    }
    const count = Math.min(length, MAX_EXCEPTION_BRANCHES)
    for (let index = 0; index < count; index++) {
      const item = this.#read(aggregate, String(index))
      if (!item.available) { this.#omittedBranches++; continue }
      const child = this.#branch(item.value, 'errors')
      if (child !== null) errors.push(child)
    }
    if (length > count) { this.#omittedBranches += length - count; this.#truncated.push('errors:branches') }
    return errors
  }
}

function isObject(value: unknown): value is object {
  return (typeof value === 'object' && value !== null) || typeof value === 'function'
}
function printableThrown(value: unknown): unknown {
  if (value === null) return 'null'
  if (value === undefined) return 'undefined'
  return value
}
