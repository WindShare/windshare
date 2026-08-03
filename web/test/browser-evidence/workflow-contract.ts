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
} from './workflow-contract/workflow-authority.ts'

export {
  BROWSER_PROCESS_INTEGRATION_TARGET,
  GENERATED_SEMANTIC_PROCESS_TARGET,
  PLATFORM_ENTRYPOINTS,
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
} from './workflow-contract/workflow-authority.ts'

export function parseWorkflowSet(sources: WorkflowSources): WorkflowSet {
  return Object.freeze({
    ci: parseWorkflowYaml(sources.ci),
    browserFull: parseWorkflowYaml(sources.browserFull),
  })
}

export function validateRepositoryContracts(
  workflowSources: WorkflowSources,
  localSources: LocalContractSources,
): void {
  const workflows = parseWorkflowSet(workflowSources)
  validateCIWorkflow(workflows.ci)
  validateBrowserFullWorkflow(workflows.browserFull)
  validateMakefileContract(localSources.makefile, localSources.fullBrowserOperationPlan)
  validateLocalEntrypointContract(localSources.packageManifest, localSources.platformScripts)
}
