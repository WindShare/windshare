import { existsSync, readFileSync, statSync } from 'node:fs'
import { dirname, normalize, resolve } from 'node:path'

import { describe, expect, it } from 'vitest'

const SOURCE_ROOT = resolve(import.meta.dirname, '../../src')
describe('v2 production entry boundary', () => {
  it('reaches the v2 session, relay, transfer, and preview implementations', () => {
    const graph = productionGraph(resolve(SOURCE_ROOT, 'main.tsx'))
    expect([...graph].some((file) => file.endsWith(normalize('session/v2-runtime.ts')))).toBe(true)
    expect([...graph].some((file) => file.endsWith(normalize('transport/relay/v2-receiver.ts')))).toBe(true)
    expect([...graph].some((file) => file.endsWith(normalize('transfer/v2-job.ts')))).toBe(true)
    expect([...graph].some((file) => file.endsWith(normalize('preview/v2-preview.ts')))).toBe(true)
    expect([...graph].some((file) => file.endsWith(normalize('preview/mp4-range.ts')))).toBe(true)
  })

  it('opens the picker-backed output authority before any production transfer job can discover', () => {
    const gateway = readFileSync(resolve(SOURCE_ROOT, 'ui/v2-gateway.ts'), 'utf8')
    const controller = readFileSync(resolve(SOURCE_ROOT, 'ui/v2-controller.ts'), 'utf8')
    expect(gateway).toContain('new TransferJob(')
    expect(gateway).not.toContain('V2TransferJob')
    expect(controller).not.toContain('openSelection(')
    expect(controller.indexOf('acquireBrowserV2Output')).toBeGreaterThanOrEqual(0)
    expect(controller.indexOf('acquireBrowserV2Output')).toBeLessThan(controller.indexOf('joined.transferJob'))
    expect(controller).toContain("phase: 'acquiring-output'")
  })

  it('keeps retired terminal-selection names out of the reachable production graph', () => {
    const retired = /V2TransferJob|V2OutputSelection|openSelection\(|resumeIntentText/u
    for (const file of productionGraph(resolve(SOURCE_ROOT, 'main.tsx'))) {
      expect(readFileSync(file, 'utf8')).not.toMatch(retired)
    }
  })
})

function productionGraph(entry: string): ReadonlySet<string> {
  const visited = new Set<string>()
  const pending = [entry]
  while (pending.length > 0) {
    const file = pending.pop()
    if (file === undefined || visited.has(file)) continue
    visited.add(file)
    const source = readFileSync(file, 'utf8')
    for (const specifier of relativeSpecifiers(source)) {
      const dependency = resolveSource(dirname(file), specifier)
      if (dependency !== undefined && dependency.startsWith(SOURCE_ROOT)) pending.push(dependency)
    }
  }
  return visited
}

function relativeSpecifiers(source: string): readonly string[] {
  const matches = source.matchAll(/(?:from\s*|import\s*)['"](\.[^'"]+)['"]/gu)
  return [...matches].map((match) => match[1]).filter((value): value is string => value !== undefined)
}

function resolveSource(parent: string, specifier: string): string | undefined {
  const base = resolve(parent, specifier)
  for (const candidate of [
    base,
    `${base}.ts`,
    `${base}.tsx`,
    resolve(base, 'index.ts'),
    resolve(base, 'index.tsx'),
  ]) {
    if (existsSync(candidate) && statSync(candidate).isFile()) return candidate
  }
  return undefined
}
