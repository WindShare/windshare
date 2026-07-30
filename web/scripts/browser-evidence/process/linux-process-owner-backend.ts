import { lstat } from 'node:fs/promises'
import { isAbsolute, join, relative, resolve, sep } from 'node:path'

import { childEvidenceEnvironment } from '../child-evidence.ts'
import { readStableRegularFileSnapshot } from '../filesystem/snapshot.ts'
import type {
  BrowserSampleContainmentBackend,
  BrowserSampleContainmentPreflight,
  BrowserSampleContainmentRequest,
  ContainedSampleCommand,
} from './containment.ts'
import {
  executeLinuxProcessOwner,
  type LinuxProcessOwnerArtifact,
} from './linux-process-owner-client.ts'
import { sampleProcessEnvironment } from './sample-environment.ts'

const MAXIMUM_LINUX_PROCESS_OWNER_BYTES = 512 * 1024 * 1024

export function createLinuxProcessOwnerContainmentBackend(
  helper: LinuxProcessOwnerArtifact,
): BrowserSampleContainmentBackend {
  const authority = canonicalLinuxProcessOwnerArtifact(helper)
  return Object.freeze({
    kind: 'linux-process-owner' as const,
    preflight: (request: BrowserSampleContainmentPreflight) =>
      preflightLinuxProcessOwner(authority, request),
    execute: (request: BrowserSampleContainmentRequest) =>
      executeLinuxProcessOwnerContainment(authority, request),
  })
}

async function preflightLinuxProcessOwner(
  helper: LinuxProcessOwnerArtifact,
  request: BrowserSampleContainmentPreflight,
): Promise<void> {
  const [snapshot] = await Promise.all([
    readStableRegularFileSnapshot(
      helper.path,
      MAXIMUM_LINUX_PROCESS_OWNER_BYTES,
      'Linux process owner helper',
    ),
    requireRegularNoFollowPath(request.topologyProfilePath, 'topology profile'),
    requireRegularNoFollowPath(request.topologyResolutionPath, 'topology resolution'),
    ...request.readOnlyInputRoots.map((path, index) =>
      requireExistingNoFollowPath(path, `read-only input ${index}`)),
  ])
  try {
    if (snapshot.bytes.byteLength !== helper.byteLength || snapshot.sha256 !== helper.sha256) {
      throw new Error('Linux process owner helper differs from its authenticated artifact')
    }
  } finally {
    snapshot.bytes.fill(0)
  }
}

async function executeLinuxProcessOwnerContainment(
  helper: LinuxProcessOwnerArtifact,
  request: BrowserSampleContainmentRequest,
) {
  const command = mapLinuxChildAttachmentPaths(
    request.command,
    join(request.sampleDirectory, 'attachments'),
    request.childAttachmentStagingRoot,
  )
  const environment = sampleProcessEnvironment(
    command.environment,
    childEvidenceEnvironment(request.childContext),
  )
  return executeLinuxProcessOwner({
    helper,
    operationId: request.operationId,
    command,
    environment,
    deadlineMs: request.deadlineMs,
    terminationGraceMs: request.terminationGraceMs,
    ...(request.terminationSignal === undefined
      ? {}
      : { terminationSignal: request.terminationSignal }),
    stdout: request.stdout,
    stderr: request.stderr,
    trace: request.trace,
  })
}

function mapLinuxChildAttachmentPaths(
  command: ContainedSampleCommand,
  finalAttachmentRoot: string,
  stagingAttachmentRoot: string,
): ContainedSampleCommand {
  const mapValue = (value: string): string =>
    mapRootConfinedLinuxPath(value, finalAttachmentRoot, stagingAttachmentRoot)
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
    ...(command.stdin === undefined
      ? {}
      : { stdin: command.stdin, stdinAuthority: command.stdinAuthority }),
  })
}

function mapRootConfinedLinuxPath(
  value: string,
  sourceRoot: string,
  destinationRoot: string,
): string {
  if (!isAbsolute(value)) return value
  const confined = relative(sourceRoot, value)
  if (confined === '') return destinationRoot
  if (confined === '..' || confined.startsWith(`..${sep}`) || isAbsolute(confined)) return value
  return join(destinationRoot, confined)
}

function canonicalLinuxProcessOwnerArtifact(
  helper: LinuxProcessOwnerArtifact,
): LinuxProcessOwnerArtifact {
  if (!isAbsolute(helper.path) || resolve(helper.path) !== helper.path) {
    throw new Error('Linux process owner helper path must be absolute and canonical')
  }
  if (!Number.isSafeInteger(helper.byteLength) || helper.byteLength < 1) {
    throw new Error('Linux process owner helper byte length is invalid')
  }
  if (!/^[0-9a-f]{64}$/u.test(helper.sha256)) {
    throw new Error('Linux process owner helper digest is invalid')
  }
  return Object.freeze({ ...helper })
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
