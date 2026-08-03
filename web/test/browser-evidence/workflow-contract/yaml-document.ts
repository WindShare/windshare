import assert from 'node:assert/strict'

import { parseDocument } from 'yaml'

export type WorkflowValue = string | number | boolean | null | WorkflowValue[] | WorkflowMapping
export type WorkflowMapping = Map<string, WorkflowValue>

export const WORKFLOW_ALIAS_EXPANSION_LIMIT = 100

export class WorkflowYamlError extends Error {
  constructor(message: string, options?: ErrorOptions) {
    super(message, options)
    this.name = 'WorkflowYamlError'
  }
}

export function parseWorkflowYaml(source: string): WorkflowMapping {
  const document = parseDocument(source, {
    merge: false,
    strict: true,
    stringKeys: true,
    uniqueKeys: true,
    version: '1.2',
  })
  if (document.errors.length > 0) {
    const details = document.errors
      .map((error) => `${error.code ?? 'YAML_ERROR'}: ${firstLine(error.message)}`)
      .join('; ')
    throw new WorkflowYamlError(`workflow YAML parse failed: ${details}`)
  }

  let value: unknown
  try {
    value = document.toJS({
      mapAsMap: true,
      maxAliasCount: WORKFLOW_ALIAS_EXPANSION_LIMIT,
    })
  } catch (cause) {
    throw new WorkflowYamlError(
      `workflow YAML alias expansion failed: ${errorMessage(cause)}`,
      { cause },
    )
  }
  if (!(value instanceof Map)) throw new WorkflowYamlError('workflow YAML root must be a mapping')
  return value as WorkflowMapping
}

export function requiredField(mapping: WorkflowMapping, key: string, label: string): WorkflowValue {
  assert(mapping.has(key), `${label}.${key} is missing`)
  return mapping.get(key) as WorkflowValue
}

export function requiredMappingField(mapping: WorkflowMapping, key: string, label: string): WorkflowMapping {
  return requireMapping(requiredField(mapping, key, label), `${label}.${key}`)
}

export function requiredSequenceField(mapping: WorkflowMapping, key: string, label: string): WorkflowValue[] {
  const value = requiredField(mapping, key, label)
  assert(Array.isArray(value), `${label}.${key} must be a sequence`)
  return value
}

export function requiredStringField(mapping: WorkflowMapping, key: string, label: string): string {
  return requireString(requiredField(mapping, key, label), `${label}.${key}`)
}

export function optionalStringField(mapping: WorkflowMapping, key: string): string | undefined {
  if (!mapping.has(key)) return undefined
  return requireString(mapping.get(key) as WorkflowValue, key)
}

export function requiredBooleanField(mapping: WorkflowMapping, key: string, label: string): boolean {
  const value = requiredField(mapping, key, label)
  assert(typeof value === 'boolean', `${label}.${key} must be a boolean`)
  return value
}

export function positiveIntegerField(mapping: WorkflowMapping, key: string, label: string): number {
  const value = requiredField(mapping, key, label)
  assert(typeof value === 'number' && Number.isSafeInteger(value) && value > 0, `${label}.${key} must be positive`)
  return value
}

export function stringListField(mapping: WorkflowMapping, key: string, label: string): string[] {
  return requiredSequenceField(mapping, key, label)
    .map((value, index) => requireString(value, `${label}.${key}[${index}]`))
}

export function requireMapping(value: WorkflowValue, label: string): WorkflowMapping {
  assert(value instanceof Map, `${label} must be a mapping`)
  return value
}

export function requireString(value: WorkflowValue, label: string): string {
  assert(typeof value === 'string', `${label} must be a string`)
  return value
}

export function escapeRegExp(value: string): string {
  return value.replace(/[.*+?^${}()|[\]\\]/gu, '\\$&')
}

export function semanticStrings(root: WorkflowValue | WorkflowMapping): string[] {
  const strings: string[] = []
  const pending: unknown[] = [root]
  const seen = new WeakSet<object>()
  while (pending.length > 0) {
    const value = pending.pop()
    if (typeof value === 'string') {
      strings.push(value)
    } else if (Array.isArray(value)) {
      if (seen.has(value)) continue
      seen.add(value)
      pending.push(...value)
    } else if (value instanceof Map) {
      if (seen.has(value)) continue
      seen.add(value)
      for (const [key, entry] of value.entries()) pending.push(key, entry)
    }
  }
  return strings
}

export function containsMappingKey(root: WorkflowValue | WorkflowMapping, expectedKey: string): boolean {
  const pending: unknown[] = [root]
  const seen = new WeakSet<object>()
  while (pending.length > 0) {
    const value = pending.pop()
    if (Array.isArray(value)) {
      if (seen.has(value)) continue
      seen.add(value)
      pending.push(...value)
    } else if (value instanceof Map) {
      if (seen.has(value)) continue
      seen.add(value)
      for (const [key, entry] of value.entries()) {
        if (typeof key === 'string' && key.toLowerCase() === expectedKey.toLowerCase()) return true
        pending.push(entry)
      }
    }
  }
  return false
}

function firstLine(message: string): string {
  return message.split(/\r?\n/u, 1)[0] ?? message
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error)
}
