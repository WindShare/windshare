import type { BrowserSampleContainmentBackend } from './containment.ts'
import {
  createLinuxProcessOwnerContainmentBackend,
} from './linux-process-owner-backend.ts'
import type { LinuxProcessOwnerArtifact } from './linux-process-owner-client.ts'
import { createWindowsJobContainmentBackend } from './windows-job-backend.ts'

export interface ProductionContainmentBackendOptions {
  readonly linuxProcessOwner?: LinuxProcessOwnerArtifact
  readonly windowsJobHelperPath?: string
}

export function createProductionContainmentBackend(
  options: ProductionContainmentBackendOptions,
): BrowserSampleContainmentBackend {
  if (process.platform === 'linux') {
    if (options.windowsJobHelperPath !== undefined) {
      throw new Error('Linux samples must not receive a Windows Job helper')
    }
    if (options.linuxProcessOwner === undefined) {
      throw new Error('production Linux samples require an authenticated subreaper artifact')
    }
    return createLinuxProcessOwnerContainmentBackend(options.linuxProcessOwner)
  }
  if (process.platform === 'win32') {
    if (options.linuxProcessOwner !== undefined) {
      throw new Error('Windows samples must not receive a Linux subreaper artifact')
    }
    if (options.windowsJobHelperPath === undefined) {
      throw new Error('production Windows samples require an absolute Windows Job helper path')
    }
    return createWindowsJobContainmentBackend(options.windowsJobHelperPath)
  }
  throw new Error(`production sample ownership is unsupported on ${process.platform}`)
}
