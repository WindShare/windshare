import { lstat } from 'node:fs/promises'
import { isAbsolute, join, relative, resolve, sep } from 'node:path'

import { childEvidenceEnvironment } from '../child-evidence.ts'
import type {
  BrowserSampleContainmentBackend,
  BrowserSampleContainmentPreflight,
  BrowserSampleContainmentRequest,
  BrowserSampleContainmentTrace,
  ContainedSampleCommand,
} from './containment.ts'
import { BrowserSampleContainmentError } from './containment.ts'
import { createOwnedEventChannel } from './owned-process-channel.mjs'
import {
  executeTestProcessOwner,
  testProcessOwnerFailureEvidence,
  type TestProcessOwnerArtifact,
  type TestProcessOwnerExecution,
  type TestProcessOwnerOutput,
  type TestProcessOwnerTransportError,
} from './test-process-owner-client.mjs'
import { sampleProcessEnvironment } from './sample-environment.ts'

const MAXIMUM_CONTAINMENT_TRACE_RECORDS = 16

export function createTestProcessOwnerContainmentBackend(
  owner: TestProcessOwnerArtifact,
  platform: NodeJS.Platform = process.platform,
): BrowserSampleContainmentBackend {
  const authority = canonicalOwnerArtifact(owner)
  if (platform !== 'linux' && platform !== 'win32') {
    throw new Error(`test process owner containment is unsupported on ${platform}`)
  }
  return Object.freeze({
    kind: 'test-process-owner' as const,
    preflight: (request: BrowserSampleContainmentPreflight) =>
      preflightOwner(authority, request),
    execute: (request: BrowserSampleContainmentRequest) =>
      executeOwnedSample(authority, platform, request),
  })
}

async function preflightOwner(
  owner: TestProcessOwnerArtifact,
  request: BrowserSampleContainmentPreflight,
): Promise<void> {
  await Promise.all([
    requireRegularNoFollowPath(owner.path, 'test process owner'),
    requireRegularNoFollowPath(request.topologyProfilePath, 'topology profile'),
    requireRegularNoFollowPath(request.topologyResolutionPath, 'topology resolution'),
    ...request.readOnlyInputRoots.map((path, index) =>
      requireExistingNoFollowPath(path, `read-only input ${index}`)),
  ])
}

async function executeOwnedSample(
  owner: TestProcessOwnerArtifact,
  platform: 'linux' | 'win32',
  request: BrowserSampleContainmentRequest,
) {
  const traces = createOwnedEventChannel<BrowserSampleContainmentTrace>(
    MAXIMUM_CONTAINMENT_TRACE_RECORDS,
    'test process owner containment traces',
  )
  let execution: TestProcessOwnerExecution | undefined
  try {
    const command = mapChildAttachmentPaths(
      request.command,
      join(request.sampleDirectory, 'attachments'),
      request.childAttachmentStagingRoot,
    )
    if (request.operationId !== request.childContext.operationId) {
      throw new Error('sample containment operation identity differs from its child evidence context')
    }
    const environment = sampleProcessEnvironment(
      command.environment,
      childEvidenceEnvironment(request.childContext),
    )
    execution = await executeTestProcessOwner({
      owner,
      runId: request.childContext.runId,
      operationId: request.operationId,
      scenario: request.childContext.scenario,
      command: {
        executable: command.executable,
        arguments: command.arguments,
        cwd: command.cwd ?? process.cwd(),
        ...(command.stdin === undefined ? {} : { stdin: command.stdin }),
      },
      environment,
      deadlineMs: request.deadlineMs,
      terminationGraceMs: request.terminationGraceMs,
      ...(request.terminationSignal === undefined
        ? {}
        : { terminationSignal: request.terminationSignal }),
      platform,
      capture: Object.freeze({
        stdoutBytes: request.capture.stdoutBytes,
        stderrBytes: request.capture.stderrBytes,
      }),
    })
    if (!execution.treeEmpty || execution.cleanupOutcome !== 'completed') {
      throw new Error('test process owner did not prove completed cleanup of an empty tree')
    }
    if (execution.ownerFailure !== undefined) {
      throw new Error(
        `test process owner reported ${execution.ownerFailure.code}: ${execution.ownerFailure.message}`,
      )
    }
    traces.append(terminalTrace(
      request,
      platform,
      'test-process-owner-settled',
      execution,
    ))
    traces.finish()
    return Object.freeze({
      ...execution,
      terminationReason: execution.ownershipEvidence.terminationReason,
      traces: traces.view.snapshot(),
    })
  } catch (cause) {
    const preserved = execution ?? preservedSettlement(cause)
    const output = execution?.output ?? preservedOutput(cause)
    traces.append(terminalTrace(
      request,
      platform,
      'test-process-owner-failed',
      preserved,
      cause,
    ))
    traces.finish()
    throw new BrowserSampleContainmentError(
      'test process owner containment failed',
      traces.view.snapshot(),
      output,
      cause,
    )
  }
}

function preservedSettlement(cause: unknown): TestProcessOwnerExecution | undefined {
  return testProcessOwnerFailureEvidence(cause)?.settlement
}

function preservedOutput(cause: unknown): TestProcessOwnerOutput | undefined {
  const evidence = testProcessOwnerFailureEvidence(cause)
  if (evidence?.kind !== 'transport-failed') return undefined
  // The inert brand check above proves this is the constructor-minted transport
  // error before we inspect its bounded output snapshot.
  return (cause as TestProcessOwnerTransportError).output
}

function terminalTrace(
  request: BrowserSampleContainmentRequest,
  platform: 'linux' | 'win32',
  milestone: 'test-process-owner-settled' | 'test-process-owner-failed',
  execution: TestProcessOwnerExecution | undefined,
  failure?: unknown,
): BrowserSampleContainmentTrace {
  const context = Object.freeze({
    runId: request.childContext.runId,
    operationId: request.operationId,
    scenario: request.childContext.scenario,
    backend: execution?.ownershipEvidence.backend ??
      (platform === 'win32' ? 'windows_job' : 'linux_subreaper'),
    terminal: execution?.processEvidence.terminal ?? 'unavailable',
    treeEmpty: execution?.treeEmpty ?? false,
    cleanupOutcome: execution?.cleanupOutcome ?? 'unavailable',
    terminationReason: execution?.ownershipEvidence.terminationReason ?? 'unavailable',
    ...(execution?.ownerFailure === undefined
      ? {}
      : {
          ownerFailureCode: execution.ownerFailure.code,
          ownerFailureMessage: execution.ownerFailure.message,
        }),
    ...(failure === undefined ? {} : { failure: boundedFailure(failure) }),
  })
  return Object.freeze({
    milestone,
    outcome: milestone === 'test-process-owner-settled' ? 'succeeded' : 'failed',
    context,
  })
}

function boundedFailure(value: unknown): string {
  const message = value instanceof Error ? value.message : String(value)
  return message.length <= 512 ? message : message.slice(0, 512)
}

function mapChildAttachmentPaths(
  command: ContainedSampleCommand,
  finalAttachmentRoot: string,
  stagingAttachmentRoot: string,
): ContainedSampleCommand {
  const mapValue = (value: string): string =>
    mapRootConfinedPath(value, finalAttachmentRoot, stagingAttachmentRoot)
  return Object.freeze({
    executable: command.executable,
    arguments: Object.freeze(command.arguments.map(mapValue)),
    ...(command.cwd === undefined ? {} : { cwd: command.cwd }),
    ...(command.environment === undefined
      ? {}
      : {
          environment: Object.freeze(Object.fromEntries(
            Object.entries(command.environment).map(([name, value]) => [name, mapValue(value)]),
          )),
        }),
    ...(command.stdin === undefined ? {} : { stdin: command.stdin }),
  })
}

function mapRootConfinedPath(value: string, sourceRoot: string, destinationRoot: string): string {
  if (!isAbsolute(value)) return value
  const confined = relative(sourceRoot, value)
  if (confined === '') return destinationRoot
  if (confined === '..' || confined.startsWith(`..${sep}`) || isAbsolute(confined)) return value
  return join(destinationRoot, confined)
}

function canonicalOwnerArtifact(owner: TestProcessOwnerArtifact): TestProcessOwnerArtifact {
  if (!isAbsolute(owner.path) || resolve(owner.path) !== owner.path) {
    throw new Error('test process owner path must be absolute and canonical')
  }
  if (Object.keys(owner).length !== 1) {
    throw new Error('test process owner artifact must contain only its dynamic fixture path')
  }
  return Object.freeze({ path: owner.path })
}

async function requireRegularNoFollowPath(path: string, label: string): Promise<void> {
  const metadata = await lstat(path)
  if (!metadata.isFile() || metadata.isSymbolicLink()) {
    throw new Error(`${label} must be a regular no-follow path`)
  }
}

async function requireExistingNoFollowPath(path: string, label: string): Promise<void> {
  const metadata = await lstat(path)
  if (metadata.isSymbolicLink()) throw new Error(`${label} must not be a symbolic link`)
}
