import type { WorkflowMapping } from './yaml-document.ts'

export type WorkflowSet = Readonly<{
  ci: WorkflowMapping
  browserFull: WorkflowMapping
}>

export type WorkflowSources = Readonly<{
  ci: string
  browserFull: string
}>

export type LocalContractSources = Readonly<{
  makefile: string
  packageManifest: string
  platformScripts: Readonly<Record<string, string>>
  fullBrowserOperationPlan: readonly string[]
}>

export const GENERATED_SEMANTIC_PROCESS_TARGET = 'test:browser:generated-semantic:process'
export const BROWSER_PROCESS_INTEGRATION_TARGET = 'test:browser:process:integration'

export const PLATFORM_ENTRYPOINTS = Object.freeze([
  'browser-local',
  'browser-network',
  'browser-preflight',
  'browser-process',
  'browser-stability',
  'check',
  'core-release',
  'coverage',
  'e2e-go',
  'hygiene',
  'integration',
  'lint',
  'race',
  'sloc',
  'vectors',
  'vet',
  'web',
  'web-dependencies',
  'workflow-lint',
])
