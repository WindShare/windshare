import type { PlaywrightTestConfig } from '@playwright/test'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

import { PLAYWRIGHT_BROWSER_PROJECTS } from './playwright.projects.ts'

export const PLAYWRIGHT_SUITES = ['main', 'pion'] as const
export const PLAYWRIGHT_SUITE_PARTITIONS = ['base', 'focused', 'remainder'] as const

export type PlaywrightSuite = typeof PLAYWRIGHT_SUITES[number]
export type PlaywrightSuitePartition = typeof PLAYWRIGHT_SUITE_PARTITIONS[number]

export interface PlaywrightSuiteDeclarations {
  readonly testDir: string
  readonly testMatch: string[]
  readonly testIgnore?: string[]
  readonly projects: NonNullable<PlaywrightTestConfig['projects']>
}

interface PlaywrightSuiteDefinition {
  readonly testDir: string
  readonly testMatch: readonly string[]
  readonly focusedSpec: string
}

const WEB_ROOT = dirname(fileURLToPath(import.meta.url))

const SUITE_DEFINITIONS: Readonly<Record<PlaywrightSuite, PlaywrightSuiteDefinition>> =
  Object.freeze({
    main: Object.freeze({
      testDir: WEB_ROOT,
      testMatch: Object.freeze([
        'test/browser/**/*.spec.ts',
        'e2e/**/*.spec.ts',
      ]),
      focusedSpec: 'e2e/v2-real-hot-switch.spec.ts',
    }),
    pion: Object.freeze({
      testDir: join(WEB_ROOT, 'test', 'transport', 'webrtc'),
      testMatch: Object.freeze(['browser.spec.ts', 'pion-interop.spec.ts']),
      focusedSpec: 'pion-interop.spec.ts',
    }),
  })

/**
 * Production and collection must start from one source-of-truth test graph.
 * Keeping this factory pure prevents discovery from inheriting servers, ports,
 * evidence paths, or any other authority needed only while tests execute.
 */
export function createPlaywrightSuiteDeclarations(
  suite: PlaywrightSuite,
  partition: PlaywrightSuitePartition = 'base',
  additionalTestMatch: readonly string[] = [],
): PlaywrightSuiteDeclarations {
  const definition = SUITE_DEFINITIONS[suite]
  const baseTestMatch = [...definition.testMatch, ...additionalTestMatch]
  if (partition === 'focused') {
    return Object.freeze({
      testDir: definition.testDir,
      testMatch: [definition.focusedSpec],
      projects: PLAYWRIGHT_BROWSER_PROJECTS,
    })
  }
  if (partition === 'remainder') {
    return Object.freeze({
      testDir: definition.testDir,
      testMatch: baseTestMatch,
      testIgnore: [definition.focusedSpec],
      projects: PLAYWRIGHT_BROWSER_PROJECTS,
    })
  }
  return Object.freeze({
    testDir: definition.testDir,
    testMatch: baseTestMatch,
    projects: PLAYWRIGHT_BROWSER_PROJECTS,
  })
}

export function focusedPlaywrightSpec(suite: PlaywrightSuite): string {
  return SUITE_DEFINITIONS[suite].focusedSpec
}

export function playwrightDiscoveryProjectName(
  suite: PlaywrightSuite,
  partition: PlaywrightSuitePartition,
  browser: string,
): string {
  return `discovery-${suite}-${partition}-${browser}`
}

export function playwrightDiscoveryProjectPattern(
  suite: PlaywrightSuite,
  partition: PlaywrightSuitePartition,
): string {
  return playwrightDiscoveryProjectName(suite, partition, '*')
}

export function createPlaywrightDiscoveryProjects(): NonNullable<
  PlaywrightTestConfig['projects']
> {
  return PLAYWRIGHT_SUITES.flatMap((suite) =>
    PLAYWRIGHT_SUITE_PARTITIONS.flatMap((partition) => {
      const declarations = createPlaywrightSuiteDeclarations(suite, partition)
      return declarations.projects.map((project) => {
        if (project.name === undefined) {
          throw new Error('Playwright discovery requires named browser projects')
        }
        return {
          name: playwrightDiscoveryProjectName(suite, partition, project.name),
          testDir: declarations.testDir,
          testMatch: declarations.testMatch,
          ...(declarations.testIgnore === undefined
            ? {}
            : { testIgnore: declarations.testIgnore }),
        }
      })
    }))
}
