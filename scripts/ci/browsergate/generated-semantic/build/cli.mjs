import { mkdtemp, readFile, readdir, rm } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join, resolve } from 'node:path'

import {
  assertPinnedNodeVersion,
  readPinnedNodeVersion,
} from '../../../node-version.mjs'
import { parseGeneratedSemanticArguments } from './arguments.mjs'
import {
  generatedSemanticArtifactSummary,
  validatedGeneratedSemanticBytes,
  validateGeneratedSemanticArtifact,
} from './artifact-policy.mjs'
import { createGeneratedSemanticEnvironment } from './environment.mjs'
import {
  GeneratedSemanticFailureError,
  createGeneratedSemanticFailure,
  failureFromCause,
  generatedSemanticCauseMessage,
} from './failure.mjs'
import {
  publishGeneratedSemanticArtifact,
  requireGeneratedSemanticPublicationResult,
} from './publisher.mjs'
import {
  createGeneratedSemanticResult,
  encodeGeneratedSemanticResult,
} from './result-contract.mjs'
import {
  assertGeneratedSemanticToolVersions,
  parseGeneratedSemanticToolAuthorization,
} from './tool-authorization.mjs'
import {
  assertGeneratedSemanticParentProcessIsolation,
  launchGeneratedSemanticWorker,
  requireSuccessfulGeneratedSemanticWorker,
} from './worker-launcher.mjs'
import { createGeneratedSemanticWorkerRequest } from './worker-protocol.mjs'

const REPOSITORY_ROOT = resolve(import.meta.dirname, '..', '..', '..', '..', '..')
const GENERATED_ROOT = resolve(import.meta.dirname, '..')
const WEB_ROOT = join(REPOSITORY_ROOT, 'web')
const TEMPORARY_PREFIX = 'windshare-final-semantic-reducer-'

export const GENERATED_SEMANTIC_PATHS = Object.freeze({
  repositoryRoot: REPOSITORY_ROOT,
  generatedRoot: GENERATED_ROOT,
  buildRoot: import.meta.dirname,
  committedPath: join(GENERATED_ROOT, 'final-semantic-reducer.js'),
  semanticEntry: join(WEB_ROOT, 'scripts', 'browser-evidence', 'final-semantic-reducer.ts'),
  toolLockPath: join(WEB_ROOT, 'pnpm-lock.yaml'),
  viteModulePath: join(WEB_ROOT, 'node_modules', 'vite', 'dist', 'node', 'index.js'),
  webRoot: WEB_ROOT,
  workerPath: join(import.meta.dirname, 'worker.mjs'),
})

const defaultDependencies = Object.freeze({
  readPinnedNodeVersion,
  assertPinnedNodeVersion,
  createTemporaryRoot: () => mkdtemp(join(tmpdir(), TEMPORARY_PREFIX)),
  removeTemporaryRoot: (path) => rm(path, { recursive: true, force: true }),
  readDirectoryEntries: (path) => readdir(path, { withFileTypes: true }),
  readCommittedArtifact: (path) => readFile(path),
  readToolAuthorization: async (path) =>
    parseGeneratedSemanticToolAuthorization(await readFile(path)),
  assertParentProcessIsolation: assertGeneratedSemanticParentProcessIsolation,
  createEnvironment: createGeneratedSemanticEnvironment,
  launchWorker: launchGeneratedSemanticWorker,
  requireWorkerSuccess: requireSuccessfulGeneratedSemanticWorker,
  publishArtifact: publishGeneratedSemanticArtifact,
})

export async function runGeneratedSemanticCli(arguments_, overrides = {}) {
  const parsed = parseGeneratedSemanticArguments(arguments_)
  if (!parsed.ok) {
    return createGeneratedSemanticResult({
      mode: null,
      outcome: 'failed',
      tools: null,
      artifact: null,
      failures: [parsed.failure],
    })
  }

  const dependencies = Object.freeze({ ...defaultDependencies, ...overrides })
  const failures = []
  let temporaryRoot = null
  let tools = null
  let artifact = null
  let successfulOutcome = 'current'
  let publicationArtifact = null

  try {
    dependencies.assertParentProcessIsolation(
      Object.hasOwn(overrides, 'parentPermissionModel')
        ? overrides.parentPermissionModel
        : process.permission,
    )
    const pinnedNodeVersion = dependencies.readPinnedNodeVersion(REPOSITORY_ROOT)
    dependencies.assertPinnedNodeVersion({
      actualVersion: overrides.actualNodeVersion ?? process.version,
      pinnedVersion: pinnedNodeVersion,
    })
    let authorizedTools
    try {
      authorizedTools = await dependencies.readToolAuthorization(
        GENERATED_SEMANTIC_PATHS.toolLockPath,
      )
    } catch (cause) {
      throw new GeneratedSemanticFailureError(
        'build',
        'tool-lock-invalid',
        generatedSemanticCauseMessage(cause, 'generated semantic tool lock is invalid'),
        { cause },
      )
    }

    try {
      temporaryRoot = await dependencies.createTemporaryRoot()
    } catch (cause) {
      throw new GeneratedSemanticFailureError(
        'build',
        'temporary-root-create-failed',
        generatedSemanticCauseMessage(cause, 'generated semantic temporary root creation failed'),
        { cause },
      )
    }

    const environment = dependencies.createEnvironment({
      platform: overrides.platform ?? process.platform,
      temporaryRoot,
      inheritedEnvironment: overrides.inheritedEnvironment ?? process.env,
    })
    const request = createGeneratedSemanticWorkerRequest({
      webRoot: WEB_ROOT,
      semanticEntry: GENERATED_SEMANTIC_PATHS.semanticEntry,
      isolatedCacheRoot: join(temporaryRoot, 'vite-cache'),
      viteModulePath: GENERATED_SEMANTIC_PATHS.viteModulePath,
    })
    const execution = await dependencies.launchWorker({
      nodeExecutable: overrides.nodeExecutable ?? process.execPath,
      workerPath: GENERATED_SEMANTIC_PATHS.workerPath,
      request,
      environment,
      workingDirectory: temporaryRoot,
    })
    const built = dependencies.requireWorkerSuccess(execution)
    let observedTools
    try {
      observedTools = assertGeneratedSemanticToolVersions(built.tools, authorizedTools)
    } catch (cause) {
      throw new GeneratedSemanticFailureError(
        'build',
        'tool-version-unauthorized',
        generatedSemanticCauseMessage(cause, 'generated semantic tool versions are not lock-authorized'),
        { cause },
      )
    }
    tools = Object.freeze({
      node: pinnedNodeVersion,
      ...observedTools,
    })

    let generatedRootEntries
    let buildDirectoryEntries
    try {
      [generatedRootEntries, buildDirectoryEntries] = await Promise.all([
        dependencies.readDirectoryEntries(GENERATED_ROOT),
        dependencies.readDirectoryEntries(import.meta.dirname),
      ])
    } catch (cause) {
      throw new GeneratedSemanticFailureError(
        'artifact-policy',
        'directory-surface-unreadable',
        generatedSemanticCauseMessage(cause, 'generated semantic directory surface is unreadable'),
        { cause },
      )
    }
    const validated = validateGeneratedSemanticArtifact({
      builds: built.builds,
      generatedRootEntries,
      buildDirectoryEntries,
    })
    artifact = generatedSemanticArtifactSummary(validated)

    let committed
    try {
      committed = await dependencies.readCommittedArtifact(GENERATED_SEMANTIC_PATHS.committedPath)
    } catch (cause) {
      throw new GeneratedSemanticFailureError(
        'stale-output',
        'committed-artifact-unreadable',
        generatedSemanticCauseMessage(cause, 'committed generated semantic artifact is unreadable'),
        { cause },
      )
    }
    const rebuilt = validatedGeneratedSemanticBytes(validated)
    const current = rebuilt.equals(Buffer.from(committed))
    if (!current && parsed.mode === 'verify') {
      throw new GeneratedSemanticFailureError(
        'stale-output',
        'committed-artifact-stale',
        'generated final semantic reducer is stale',
      )
    }
    if (!current && parsed.mode === 'write') {
      publicationArtifact = validated
    }
  } catch (cause) {
    failures.push(failureFromCause(
      cause,
      'build',
      'unexpected-build-failure',
      'generated semantic build failed unexpectedly',
    ))
  }

  if (temporaryRoot !== null) {
    const completedTemporaryRoot = temporaryRoot
    temporaryRoot = null
    try {
      await dependencies.removeTemporaryRoot(completedTemporaryRoot)
    } catch (cause) {
      failures.push(createGeneratedSemanticFailure(
        'cleanup',
        'temporary-root-remove-failed',
        generatedSemanticCauseMessage(cause, 'generated semantic temporary root cleanup failed'),
      ))
    }
  }

  if (publicationArtifact !== null && failures.length === 0) {
    let publication
    let publisherReturned = false
    try {
      publication = await dependencies.publishArtifact({
        artifact: publicationArtifact,
        destination: GENERATED_SEMANTIC_PATHS.committedPath,
      })
      publisherReturned = true
    } catch (cause) {
      failures.push(createGeneratedSemanticFailure(
        'publication',
        'publisher-failed',
        generatedSemanticCauseMessage(cause, 'generated semantic publisher failed'),
      ))
    }
    if (publisherReturned) {
      try {
        publication = requireGeneratedSemanticPublicationResult(publication)
        failures.push(...publication.failures)
        if (publication.ok) successfulOutcome = 'published'
      } catch (cause) {
        failures.push(createGeneratedSemanticFailure(
          'publication',
          'publisher-result-invalid',
          generatedSemanticCauseMessage(cause, 'generated semantic publisher result is invalid'),
        ))
      }
    }
  }

  return createGeneratedSemanticResult({
    mode: parsed.mode,
    outcome: failures.length === 0 ? successfulOutcome : 'failed',
    tools,
    artifact,
    failures,
  })
}

export async function executeGeneratedSemanticCli(arguments_, overrides = {}) {
  let result
  try {
    result = await runGeneratedSemanticCli(arguments_, overrides)
  } catch (cause) {
    const parsed = parseGeneratedSemanticArguments(arguments_)
    result = createGeneratedSemanticResult({
      mode: parsed.ok ? parsed.mode : null,
      outcome: 'failed',
      tools: null,
      artifact: null,
      failures: [createGeneratedSemanticFailure(
        parsed.ok ? 'build' : 'usage',
        parsed.ok ? 'cli-internal-failure' : 'invalid-arguments',
        generatedSemanticCauseMessage(cause, 'generated semantic CLI failed internally'),
      )],
    })
  }
  return Object.freeze({
    exitCode: result.outcome === 'failed' ? 1 : 0,
    record: encodeGeneratedSemanticResult(result),
    result,
  })
}
