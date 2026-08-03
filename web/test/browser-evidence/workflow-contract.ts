import assert from 'node:assert/strict'

import { parseWorkflowYaml } from './workflow-contract/yaml-document.ts'
import type {
  LocalContractSources,
  WorkflowSet,
  WorkflowSources,
} from './workflow-contract/workflow-policy.ts'
import {
  validateLocalEntrypointContract,
  validateMakefileContract,
} from './workflow-contract/local-authority.ts'
import {
  validateBrowserFullWorkflow,
  validateCIWorkflow,
  validateCurrentCommitWorkflow,
  validateReleaseWorkflow,
  validateStabilityWorkflow,
} from './workflow-contract/workflow-authority.ts'

export {
  BROWSER_PROCESS_INTEGRATION_COMMAND,
  BROWSER_PROCESS_INTEGRATION_TARGET,
  FULL_GATES,
  GENERATED_SEMANTIC_PROCESS_COMMAND,
  GENERATED_SEMANTIC_PROCESS_ENTRY,
  GENERATED_SEMANTIC_PROCESS_TARGET,
  PLATFORM_ENTRYPOINTS,
  PR_GATES,
} from './workflow-contract/workflow-policy.ts'
export type {
  LocalContractSources,
  WorkflowSet,
  WorkflowSources,
} from './workflow-contract/workflow-policy.ts'
export {
  WORKFLOW_ALIAS_EXPANSION_LIMIT,
  WorkflowYamlError,
  parseWorkflowYaml,
} from './workflow-contract/yaml-document.ts'
export type {
  WorkflowMapping,
  WorkflowValue,
} from './workflow-contract/yaml-document.ts'
export {
  validateLocalEntrypointContract,
  validateMakefileContract,
} from './workflow-contract/local-authority.ts'
export {
  validateBrowserFullWorkflow,
  validateCIWorkflow,
  validateCurrentCommitWorkflow,
  validateReleaseWorkflow,
  validateStabilityWorkflow,
} from './workflow-contract/workflow-authority.ts'

export function parseWorkflowSet(sources: WorkflowSources): WorkflowSet {
  return Object.freeze({
    ci: parseWorkflowYaml(sources.ci),
    currentCommit: parseWorkflowYaml(sources.currentCommit),
    stability: parseWorkflowYaml(sources.stability),
    releaseReadiness: parseWorkflowYaml(sources.releaseReadiness),
    browserFull: parseWorkflowYaml(sources.browserFull),
  })
}

export function validateRepositoryContracts(
  workflowSources: WorkflowSources,
  localSources: LocalContractSources,
): void {
  validateWorkflowSet(parseWorkflowSet(workflowSources))
  validateMakefileContract(localSources.makefile, localSources.fullBrowserOperationPlan)
  validateLocalEntrypointContract(localSources.packageManifest, localSources.platformScripts)
}

export function validateWorkflowSet(workflows: WorkflowSet): void {
  validateCIWorkflow(workflows.ci)
  validateCurrentCommitWorkflow(workflows.currentCommit)
  const stabilityCapacity = validateStabilityWorkflow(workflows.stability)
  const releaseRequirement = validateReleaseWorkflow(workflows.releaseReadiness)
  assert(
    stabilityCapacity >= releaseRequirement,
    `stability retention can preserve ${stabilityCapacity} runs; release requires ${releaseRequirement}`,
  )
  validateBrowserFullWorkflow(workflows.browserFull)
}
