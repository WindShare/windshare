import type { WorkflowMapping } from './yaml-document.ts'

export type WorkflowSet = Readonly<{
  ci: WorkflowMapping
  currentCommit: WorkflowMapping
  stability: WorkflowMapping
  releaseReadiness: WorkflowMapping
  browserFull: WorkflowMapping
}>

export type WorkflowSources = Readonly<{
  ci: string
  currentCommit: string
  stability: string
  releaseReadiness: string
  browserFull: string
}>

export type LocalContractSources = Readonly<{
  makefile: string
  packageManifest: string
  platformScripts: Readonly<Record<string, string>>
  fullBrowserOperationPlan: readonly string[]
}>

export const GENERATED_SEMANTIC_PROCESS_TARGET = 'test:browser:generated-semantic:process'
export const GENERATED_SEMANTIC_PROCESS_ENTRY =
  '../scripts/ci/browsergate/tests/process/generated-semantic.tests.mjs'
export const GENERATED_SEMANTIC_PROCESS_COMMAND = `node ${GENERATED_SEMANTIC_PROCESS_ENTRY}`
export const BROWSER_PROCESS_INTEGRATION_TARGET = 'test:browser:process:integration'
export const BROWSER_PROCESS_INTEGRATION_COMMAND = [
  'vitest run',
  'test/browser-evidence/native-directory-publisher.test.ts',
  'test/browser-evidence/process-runner.test.ts',
  'test/browser-evidence/test-process-owner-client.test.ts',
].join(' ')

export const PLATFORM_ENTRYPOINTS = Object.freeze([
  'browser-contract',
  'browser-generated',
  'browser-local',
  'browser-network',
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

export const PR_GATES = Object.freeze([
  'vet',
  'core-release',
  'race',
  'vectors',
  'coverage',
  'integration',
  'e2e',
  'web',
  'browser-contract',
  'browser-generated',
  'browser-process',
  'hygiene',
  'lint',
  'sloc',
])

export const FULL_GATES = Object.freeze([
  'vet',
  'core-release',
  'race',
  'vectors',
  'coverage',
  'integration',
  'e2e',
  'web',
  'browser-contract',
  'browser-generated',
  'browser-process',
  'hygiene',
  'lint',
  'sloc',
  'browser',
])
