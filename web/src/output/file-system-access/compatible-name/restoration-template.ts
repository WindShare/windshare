/// <reference types="vite/client" />

import windowsPowerShellV1Source from './restoration/windows-v1.ps1?raw'
import type { CompatibleNameSidecarMappingV1 } from './sidecar-codec'

export const WINDOWS_POWERSHELL_V1_RESTORATION_TEMPLATE_ID =
  'windows-powershell-v1' as const

export type RestorationPathState = 'rename' | 'already-restored' | 'conflict' | 'missing'

export interface RestorationTemplate {
  readonly id: typeof WINDOWS_POWERSHELL_V1_RESTORATION_TEMPLATE_ID
  readonly scriptFileExtension: '.ps1'
  readonly source: string
}

export interface RestorationTemplateProvider {
  select(platform: string): RestorationTemplate
}

export class RestorationTemplateUnavailableError extends Error {
  readonly platform: string

  constructor(platform: string) {
    super(`no proven compatible-name restoration template is available for platform '${platform}'`)
    this.name = 'RestorationTemplateUnavailableError'
    this.platform = platform
  }
}

const WINDOWS_POWERSHELL_V1_TEMPLATE: RestorationTemplate = Object.freeze({
  id: WINDOWS_POWERSHELL_V1_RESTORATION_TEMPLATE_ID,
  scriptFileExtension: '.ps1',
  source: windowsPowerShellV1Source,
})

/**
 * This provider consumes an already-derived host fact. It cannot manufacture the
 * native lookup rejection that alone authorizes compatible-name handling.
 */
export const provenRestorationTemplateProvider: RestorationTemplateProvider = Object.freeze({
  select(platform: string): RestorationTemplate {
    if (platform === 'windows') return WINDOWS_POWERSHELL_V1_TEMPLATE
    throw new RestorationTemplateUnavailableError(platform)
  },
})

export function restorationPathState(
  compatibleSourcePresent: boolean,
  originalTargetPresent: boolean,
): RestorationPathState {
  if (compatibleSourcePresent) return originalTargetPresent ? 'conflict' : 'rename'
  return originalTargetPresent ? 'already-restored' : 'missing'
}

export function orderRestorationMappings(
  mappings: readonly CompatibleNameSidecarMappingV1[],
): readonly CompatibleNameSidecarMappingV1[] {
  return Object.freeze([...mappings].sort((left, right) =>
    right.logicalPath.length - left.logicalPath.length || right.ordinal - left.ordinal))
}

export interface RestorationAncestorPresence {
  readonly logicalPresent: boolean
  readonly physicalPresent: boolean
}

export type RestorationAncestorObserver = (
  logicalPath: readonly string[],
  mapping: CompatibleNameSidecarMappingV1 | undefined,
) => RestorationAncestorPresence

export interface RestorationAncestorStep {
  readonly logicalPath: readonly string[]
  readonly selectedComponent: string
  readonly selectedName: 'logical' | 'physical'
}

export type RestorationAncestorRebase =
  | Readonly<{
    state: 'resolved'
    parentComponents: readonly string[]
    steps: readonly RestorationAncestorStep[]
  }>
  | Readonly<{
    state: 'conflict' | 'missing'
    logicalPath: readonly string[]
  }>

/**
 * Re-evaluating every mapped ancestor lets a rerun cross an interruption where
 * some parents already use logical names while deeper parents remain physical.
 */
export function rebaseRestorationAncestors(
  record: CompatibleNameSidecarMappingV1,
  mappings: readonly CompatibleNameSidecarMappingV1[],
  observe: RestorationAncestorObserver,
): RestorationAncestorRebase {
  const mappingsByPath = new Map(mappings.map(mapping => [
    restorationPathKey(mapping.logicalPath),
    mapping,
  ]))
  const parentComponents: string[] = []
  const steps: RestorationAncestorStep[] = []

  for (let depth = 1; depth < record.logicalPath.length; depth += 1) {
    const logicalPath = Object.freeze(record.logicalPath.slice(0, depth))
    const logicalComponent = logicalPath.at(-1)!
    const mapping = mappingsByPath.get(restorationPathKey(logicalPath))
    const presence = observe(logicalPath, mapping)
    const decision = decideRestorationAncestor(
      logicalPath,
      logicalComponent,
      mapping,
      presence,
    )
    if (decision.state !== 'resolved') {
      return decision
    }
    parentComponents.push(decision.step.selectedComponent)
    steps.push(decision.step)
  }

  return Object.freeze({
    state: 'resolved',
    parentComponents: Object.freeze(parentComponents),
    steps: Object.freeze(steps),
  })
}

type RestorationAncestorDecision =
  | Readonly<{ state: 'resolved'; step: RestorationAncestorStep }>
  | Readonly<{ state: 'conflict' | 'missing'; logicalPath: readonly string[] }>

function decideRestorationAncestor(
  logicalPath: readonly string[],
  logicalComponent: string,
  mapping: CompatibleNameSidecarMappingV1 | undefined,
  presence: RestorationAncestorPresence,
): RestorationAncestorDecision {
  if (mapping === undefined) {
    if (!presence.logicalPresent) return Object.freeze({ state: 'missing', logicalPath })
    return resolvedAncestorStep(logicalPath, logicalComponent, 'logical')
  }
  if (mapping.entryKind !== 'directory') {
    throw new TypeError('restoration ancestor mapping must name a directory')
  }
  const state = restorationPathState(presence.physicalPresent, presence.logicalPresent)
  if (state === 'conflict' || state === 'missing') {
    return Object.freeze({ state, logicalPath })
  }
  if (state === 'rename') {
    return resolvedAncestorStep(logicalPath, mapping.physicalComponent, 'physical')
  }
  return resolvedAncestorStep(logicalPath, logicalComponent, 'logical')
}

function resolvedAncestorStep(
  logicalPath: readonly string[],
  selectedComponent: string,
  selectedName: RestorationAncestorStep['selectedName'],
): RestorationAncestorDecision {
  return Object.freeze({
    state: 'resolved',
    step: Object.freeze({ logicalPath, selectedComponent, selectedName }),
  })
}

function restorationPathKey(path: readonly string[]): string {
  return path.map(component => component.toUpperCase()).join('/')
}
