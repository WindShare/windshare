import path from 'node:path'
import { fileURLToPath } from 'node:url'
import ts from 'typescript'
import { findBoundaryViolations, loadSourceGraph } from './graph.mjs'

const webRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')
const configPath = path.join(webRoot, 'tsconfig.app.json')
const config = ts.readConfigFile(configPath, ts.sys.readFile)
if (config.error !== undefined) throw new Error(ts.flattenDiagnosticMessageText(config.error.messageText, '\n'))
const parsed = ts.parseJsonConfigFileContent(config.config, ts.sys, webRoot)
if (parsed.errors.length !== 0) {
  throw new Error(parsed.errors.map(error => ts.flattenDiagnosticMessageText(error.messageText, '\n')).join('\n'))
}
const graph = loadSourceGraph(path.join(webRoot, 'src'), parsed.options)
const violations = findBoundaryViolations(graph)
if (violations.length !== 0) {
  console.error('Transfer must consume domain contracts; output adapters own storage implementations.')
  for (const chain of violations) console.error('  ' + chain.join(' -> '))
  process.exitCode = 1
} else {
  console.log('Transfer dependency boundaries passed (' + graph.size + ' source modules).')
}
