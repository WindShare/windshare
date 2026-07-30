import { lstat } from 'node:fs/promises'
import { isAbsolute, resolve, win32 } from 'node:path'

import { childEvidenceEnvironment } from '../child-evidence.ts'
import { executeWindowsJob } from './windows-job-client.ts'
import { sampleProcessEnvironment } from './sample-environment.ts'
import type {
  BrowserSampleContainmentBackend,
  BrowserSampleContainmentRequest,
  ContainedSampleCommand,
} from './containment.ts'

export function createWindowsJobContainmentBackend(helperPath: string): BrowserSampleContainmentBackend {
  if (!isAbsolute(helperPath) || resolve(helperPath) !== helperPath) {
    throw new Error('Windows Job helper path must be absolute and canonical')
  }
  const canonicalHelperPath = helperPath
  return Object.freeze({
    kind: 'windows-job' as const,
    preflight: () => preflightWindowsJob(canonicalHelperPath),
    execute: (request: BrowserSampleContainmentRequest) =>
      executeWindowsJobContainment(canonicalHelperPath, request),
  })
}

async function preflightWindowsJob(helperPath: string): Promise<void> {
  if (!isAbsolute(helperPath)) throw new Error('Windows Job helper path must be absolute')
  const status = await lstat(helperPath)
  if (!status.isFile() || status.isSymbolicLink()) {
    throw new Error('Windows Job helper must be a regular file')
  }
}

async function executeWindowsJobContainment(
  helperPath: string,
  request: BrowserSampleContainmentRequest,
) {
  const command = mapWindowsChildAttachmentPaths(
    request.command,
    win32.join(request.sampleDirectory, 'attachments'),
    request.childAttachmentStagingRoot,
  )
  const environment = sampleProcessEnvironment(
    command.environment,
    childEvidenceEnvironment(request.childContext),
  )
  return executeWindowsJob({
    helperPath,
    operationId: request.operationId,
    command: Object.freeze({ ...command, environment }),
    inheritedEnvironment: Object.freeze({}),
    injectedEnvironment: Object.freeze({}),
    deadlineMs: request.deadlineMs,
    terminationGraceMs: request.terminationGraceMs,
    ...(request.terminationSignal === undefined
      ? {}
      : { terminationSignal: request.terminationSignal }),
    stdout: request.stdout,
    stderr: request.stderr,
  })
}

function mapWindowsChildAttachmentPaths(
  command: ContainedSampleCommand,
  finalAttachmentRoot: string,
  stagingAttachmentRoot: string,
): ContainedSampleCommand {
  const mapValue = (value: string): string =>
    mapRootConfinedWindowsPath(value, finalAttachmentRoot, stagingAttachmentRoot)
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

function mapRootConfinedWindowsPath(value: string, sourceRoot: string, destinationRoot: string): string {
  if (!win32.isAbsolute(value)) return value
  const relative = win32.relative(sourceRoot, value)
  if (relative === '') return destinationRoot
  if (relative === '..' || relative.startsWith(`..${win32.sep}`) || win32.isAbsolute(relative)) {
    return value
  }
  return win32.join(destinationRoot, relative)
}
