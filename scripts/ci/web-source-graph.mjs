import { existsSync, readFileSync, statSync } from 'node:fs'
import { dirname, resolve, sep } from 'node:path'

export function productionGraph(entry, sourceRoot) {
  const visited = new Set()
  const traversed = new Set()
  const unresolved = []
  const pending = [{ file: entry, loadMode: 'source' }]
  while (pending.length > 0) {
    const current = pending.pop()
    if (current === undefined) continue
    const { file, loadMode } = current
    visited.add(file)
    if (loadMode === 'opaque-resource' || traversed.has(file)) continue
    traversed.add(file)
    const source = readFileSync(file, 'utf8')
    for (const specifier of relativeSpecifiers(source)) {
      const dependency = resolveSource(dirname(file), specifier)
      if (dependency === undefined) {
        unresolved.push(Object.freeze({ importer: file, specifier }))
      } else if (dependency.file === sourceRoot || dependency.file.startsWith(`${sourceRoot}${sep}`)) {
        pending.push(dependency)
      }
    }
  }
  return Object.freeze({ dependencies: visited, unresolved: Object.freeze(unresolved) })
}

export function resolveSource(parent, specifier) {
  const parsed = parseRelativeSpecifier(specifier)
  const base = resolve(parent, parsed.path)
  for (const candidate of [
    base,
    `${base}.ts`,
    `${base}.tsx`,
    resolve(base, 'index.ts'),
    resolve(base, 'index.tsx'),
  ]) {
    if (existsSync(candidate) && statSync(candidate).isFile()) {
      return Object.freeze({ file: candidate, loadMode: parsed.loadMode })
    }
  }
  return undefined
}

function relativeSpecifiers(source) {
  const specifiers = []
  for (const match of source.matchAll(/(?:from\s*|import\s*)['"](\.[^'"]+)['"]/gu)) {
    if (match[1] !== undefined) specifiers.push(match[1])
  }
  for (const match of source.matchAll(/import\s*\(\s*['"](\.[^'"]+)['"]\s*\)/gu)) {
    if (match[1] !== undefined) specifiers.push(match[1])
  }
  return specifiers
}

function parseRelativeSpecifier(specifier) {
  const queryIndex = specifier.indexOf('?')
  const fragmentIndex = specifier.indexOf('#')
  const qualifierIndexes = [queryIndex, fragmentIndex].filter(index => index >= 0)
  const pathEnd = qualifierIndexes.length === 0 ? specifier.length : Math.min(...qualifierIndexes)
  const queryEnd = fragmentIndex > queryIndex ? fragmentIndex : specifier.length
  const query = queryIndex === pathEnd
    ? specifier.slice(queryIndex + 1, queryEnd)
    : ''

  // Vite turns a ?raw dependency into data. Treating those bytes as source could
  // manufacture graph edges from the contents of a shipped script or document.
  const loadMode = new URLSearchParams(query).has('raw') ? 'opaque-resource' : 'source'
  return Object.freeze({ path: specifier.slice(0, pathEnd), loadMode })
}
