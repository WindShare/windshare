import { describe, expect, it, vi } from 'vitest'

import {
  captureDiagnosticContextV1,
  type DiagnosticContextSources,
} from '../../../src/diagnostics/export/context'

describe('diagnostic context projection', () => {
  it('takes one independent allowlisted cut from every memory source', () => {
    const sources: DiagnosticContextSources = {
      controller: source({
        generation: 1n,
        phase: 'awaiting-key',
        status: 'private UI text',
      }),
      lifecycle: source({
        generation: 2n,
        state: 'resumable-receive',
        operationId: 'private',
      }),
      progress: source({
        generation: 3n,
        discovery: 'failed',
        discoveredFiles: 4n,
        discoveredBytes: 5n,
        writtenBytes: 6n,
        completedFiles: 7n,
        completedBytes: 8n,
        fileErrors: 9n,
        selectionErrors: 10n,
        failedDirectories: 11n,
        contentLanes: 0xffff_ffff,
        path: 'private',
      }),
      output: source({
        generation: 12n,
        planKind: 'workspace-then-publish',
        handle: {},
      }),
      protocol: source({
        generation: 0xffff_ffff_ffff_ffffn,
        message: 'private',
      }),
    }

    const context = captureDiagnosticContextV1(sources)

    expect(context).toEqual({
      controller: { generation: '1', phase: 'awaiting_key' },
      lifecycle: { generation: '2', state: 'resumable_receive' },
      progress: {
        generation: '3',
        discovery: 'failed',
        discovered_files: '4',
        discovered_bytes: '5',
        written_bytes: '6',
        completed_files: '7',
        completed_bytes: '8',
        file_errors: '9',
        selection_errors: '10',
        failed_directories: '11',
        content_lanes: 0xffff_ffff,
      },
      output: {
        generation: '12',
        plan_kind: 'workspace_then_publish',
      },
      protocol: { generation: '18446744073709551615' },
    })
    expect(JSON.stringify(context)).not.toContain('private')
    expect(Object.isFrozen(context)).toBe(true)
    expect(Object.isFrozen(context.progress)).toBe(true)
    for (const value of Object.values(sources)) {
      expect(value?.read).toHaveBeenCalledTimes(1)
    }
  })

  it('omits only a failing or invalid supplier', () => {
    const context = captureDiagnosticContextV1({
      controller: {
        read: () => {
          throw new Error('unavailable')
        },
      },
      lifecycle: source({
        generation: -1n,
        state: 'published',
      }),
      output: source({
        generation: 7n,
        planKind: 'direct-atomic',
      }),
    })

    expect(context).toEqual({
      output: { generation: '7', plan_kind: 'direct_atomic' },
    })
  })
})

function source<Snapshot>(snapshot: Snapshot & Record<string, unknown>) {
  return {
    read: vi.fn(() => snapshot),
  }
}
