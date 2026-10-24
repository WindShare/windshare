import path from 'node:path'
import { fileURLToPath } from 'node:url'
import ts from 'typescript'
import { findBoundaryViolations, loadSourceGraph } from './graph.mjs'
import { BOUNDARY_POLICIES } from './policies.mjs'

const webRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')
const configPath = path.join(webRoot, 'tsconfig.app.json')
const config = ts.readConfigFile(configPath, ts.sys.readFile)
if (config.error !== undefined) throw new Error(ts.flattenDiagnosticMessageText(config.error.messageText, '\n'))
const parsed = ts.parseJsonConfigFileContent(config.config, ts.sys, webRoot)
if (parsed.errors.length !== 0) {
  throw new Error(parsed.errors.map(error => ts.flattenDiagnosticMessageText(error.messageText, '\n')).join('\n'))
}
const graph = loadSourceGraph(path.join(webRoot, 'src'), parsed.options)
for (const policy of BOUNDARY_POLICIES) {
  const violations = findBoundaryViolations(graph, policy)
  if (violations.length === 0) continue
  console.error('Dependency boundary: ' + policy.name)
  for (const chain of violations) console.error('  ' + chain.join(' -> '))
  process.exitCode = 1
}
if (process.exitCode !== 1) {
  console.log('Web dependency boundaries passed (' + BOUNDARY_POLICIES.length + ' policies, ' + graph.size + ' source modules).')
}
