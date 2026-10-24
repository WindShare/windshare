import fs from 'node:fs'
import path from 'node:path'
import ts from 'typescript'
import { TRANSFER_BOUNDARY } from './policies.mjs'

const normalized = value => value.replaceAll('\\', '/')
const inside = (file, directory) => {
  const source = file.replace(/\?(raw|url)$/, '')
  return source === directory || source.startsWith(directory + '/')
}
const STYLE_EXTENSION = '.css'

export function moduleReferences(source) {
  const references = []
  function visit(node) {
    if ((ts.isImportDeclaration(node) || ts.isExportDeclaration(node)) && node.moduleSpecifier) {
      references.push(node.moduleSpecifier)
    } else if (ts.isImportTypeNode(node) && ts.isLiteralTypeNode(node.argument)) {
      references.push(node.argument.literal)
    } else if (ts.isImportEqualsDeclaration(node) && ts.isExternalModuleReference(node.moduleReference)) {
      references.push(node.moduleReference.expression)
    } else if (ts.isCallExpression(node) &&
        (node.expression.kind === ts.SyntaxKind.ImportKeyword ||
         ts.isIdentifier(node.expression) && node.expression.text === 'require')) {
      references.push(node.arguments[0])
    }
    ts.forEachChild(node, visit)
  }
  visit(source)
  return references.filter(value => value !== undefined).map(value =>
    ts.isStringLiteralLike(value) ? value.text : null)
}

export function loadSourceGraph(sourceRoot, options) {
  const root = normalized(path.resolve(sourceRoot))
  const cache = ts.createModuleResolutionCache(path.dirname(root), value => value, options)
  const graph = new Map()
  function walk(directory) {
    for (const entry of fs.readdirSync(directory, { withFileTypes: true })) {
      const file = path.join(directory, entry.name)
      if (entry.isDirectory()) walk(file)
      else if (/\.[cm]?[jt]sx?$/.test(entry.name)) {
        const source = ts.createSourceFile(file, fs.readFileSync(file, 'utf8'), ts.ScriptTarget.Latest, true)
        const edges = moduleReferences(source).flatMap(specifier => {
          if (specifier === null) return ['<computed import>']
          const asset = /\?(raw|url)$/.exec(specifier)
          const sourceSpecifier = asset === null ? specifier : specifier.slice(0, asset.index)
          const resolved = ts.resolveModuleName(sourceSpecifier, file, options, ts.sys, cache).resolvedModule
          const bundledAsset = asset !== null || path.extname(sourceSpecifier) === STYLE_EXTENSION
          const assetFile = bundledAsset && sourceSpecifier.startsWith('.')
            ? path.resolve(path.dirname(file), sourceSpecifier) : undefined
          const dependencyFile = resolved?.resolvedFileName ??
            (assetFile !== undefined && fs.existsSync(assetFile) && fs.statSync(assetFile).isFile() ? assetFile : undefined)
          if (dependencyFile === undefined) return [specifier.startsWith('.') || specifier.startsWith('/')
            ? '<unresolved import: ' + specifier + '>' : 'package:' + specifier]
          const dependency = normalized(path.resolve(dependencyFile))
          // Raw assets remain visible to path policies, without executing their imports.
          if (inside(dependency, root)) return [dependency.slice(root.length + 1) + (asset?.[0] ?? '')]
          return specifier.startsWith('.') ? [] : ['package:' + specifier]
        })
        graph.set(normalized(path.resolve(file)).slice(root.length + 1), edges)
      }
    }
  }
  walk(sourceRoot)
  return graph
}

/** Resolve the entire path so an innocent-looking barrel cannot hide a forbidden implementation. */
export function findBoundaryViolations(graph, policy = TRANSFER_BOUNDARY) {
  const violations = []
  for (const root of graph.keys()) {
    if (!policy.roots.some(directory => inside(root, directory))) continue
    const visited = new Set([root])
    const queue = [[root]]
    for (let index = 0; index < queue.length; index += 1) {
      const chain = queue[index]
      for (const dependency of graph.get(chain.at(-1)) ?? []) {
        if (visited.has(dependency)) continue
        visited.add(dependency)
        const next = [...chain, dependency]
        if (dependency.startsWith('<') || policy.forbidden.some(directory => inside(dependency, directory))) {
          violations.push(next)
        } else if (!policy.stopAt?.some(directory => inside(dependency, directory))) {
          queue.push(next)
        }
      }
    }
  }
  return violations
}
