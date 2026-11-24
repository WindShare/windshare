import { describe, expect, it } from 'vitest'
import {
  MAX_INLINE_RESTORE_COMMAND_LENGTH,
  MAX_INLINE_RESTORE_STEPS,
  RESTORE_MANIFEST_FILENAME,
  RESTORE_POSIX_FALLBACK_FILENAME,
  RESTORE_POSIX_SCRIPT_FILENAME,
  buildSafeNameRestorePlan,
  detectRestorePlatform,
  formatInlineRestoreCommand,
  renderRestoreArtifact,
  serializeRestoreManifestJson,
  type SafeNameRestorePlanInput,
} from '../../src/output/names/restore-plan'
import type { ReceiveFileRecord, ReceiveTaskRecord } from '../../src/output/store/types'
import { initialReceiveState, transitionReceiveState, type ReceiveLifecycleState } from '../../src/transfer/state'

function makeLifecycle(kind: ReceiveLifecycleState['kind']): ReceiveLifecycleState {
  const base = initialReceiveState('task-1')
  switch (kind) {
    case 'preparing':
      return base
    case 'downloading':
      return transitionReceiveState(base, { kind: 'downloading', phase: 'receiving' })
    case 'paused':
      return transitionReceiveState(base, {
        kind: 'paused', reason: 'user', completedFiles: 1n, completedBytes: 8n,
        retainedBytes: 8n, discoveredFiles: 2n, discoveredBytes: 16n, discoveryComplete: true,
      })
    case 'saved':
      return transitionReceiveState(base, { kind: 'saved', savedFiles: 1n, savedBytes: 8n })
    case 'partial':
      return transitionReceiveState(base, { kind: 'partial', reason: 'stopped', savedFiles: 1n, failedFiles: 0n })
    case 'failed':
      return transitionReceiveState(base, { kind: 'failed', reason: 'cancelled' })
    default:
      return base
  }
}

function makeFSATask(overrides: Readonly<{
  artifactName?: string
  reservedName?: string
  parentName?: string
  collisionIndex?: number
  singleFile?: boolean
  lifecycleKind?: ReceiveLifecycleState['kind']
  safeRenamedDirectories?: ReceiveTaskRecord['safeRenamedDirectories']
}> = {}): SafeNameRestorePlanInput['task'] {
  const singleFile = overrides.singleFile ?? false
  const parentHandle = { name: overrides.parentName ?? 'Downloads' } as unknown as FileSystemDirectoryHandle
  const target = singleFile
    ? {
        kind: 'fsa' as const,
        parentHandle,
        reservedName: overrides.reservedName ?? overrides.artifactName ?? 'app.dll',
        collisionIndex: overrides.collisionIndex ?? 0,
        singleFile: true as const,
        reservedAtMs: 1_000,
        reservedFile: {
          handle: {} as FileSystemFileHandle,
          fingerprint: { size: 0, lastModified: 1_000 },
        },
      }
    : {
        kind: 'fsa' as const,
        parentHandle,
        reservedName: overrides.reservedName ?? overrides.artifactName ?? 'Project',
        collisionIndex: overrides.collisionIndex ?? 0,
        singleFile: false as const,
        reservedAtMs: 1_000,
      }
  return {
    artifact: {
      kind: 'directory-tree',
      name: overrides.artifactName ?? (singleFile ? 'app.dll' : 'Project'),
      ...(singleFile ? { singleFileId: 'f1' } : {}),
    },
    target,
    lifecycle: makeLifecycle(overrides.lifecycleKind ?? 'saved'),
    ...(overrides.safeRenamedDirectories === undefined
      ? {}
      : { safeRenamedDirectories: overrides.safeRenamedDirectories }),
  }
}

function makeFile(
  path: readonly string[],
  fsaPath: readonly string[],
  status: ReceiveFileRecord['status'] = 'completed',
): Pick<ReceiveFileRecord, 'path' | 'fsaPath' | 'status'> {
  return { path, fsaPath, status }
}

describe('detectRestorePlatform', () => {
  it('detects Windows vs POSIX platforms from navigator hints', () => {
    expect(detectRestorePlatform({ userAgentData: { platform: 'Windows' } })).toBe('windows')
    expect(detectRestorePlatform({ platform: 'Win32' })).toBe('windows')
    expect(detectRestorePlatform({ userAgent: 'Mozilla/5.0 (Windows NT 10.0; Win64; x64)' })).toBe('windows')
    expect(detectRestorePlatform({ userAgentData: { platform: 'macOS' } })).toBe('posix')
    expect(detectRestorePlatform({ platform: 'Linux x86_64' })).toBe('posix')
    expect(detectRestorePlatform({})).toBe('posix')
  })
})

describe('buildSafeNameRestorePlan', () => {
  it('orders nested renames deepest-first, separates root vs child CON/CON scopes, and includes empty renamed directories', () => {
    const task = makeFSATask({
      artifactName: 'CON',
      reservedName: 'entry-CON.windshare-11111111',
      parentName: 'Downloads',
      safeRenamedDirectories: [
        {
          logicalPath: ['CON', 'empty.cfg'],
          physicalPath: ['entry-CON.windshare-22222222', 'empty-cfg.windshare-33333333'],
        },
      ],
    })
    const files = [
      makeFile(
        ['CON', 'sub.cfg', 'lib.dll'],
        ['entry-CON.windshare-22222222', 'sub-cfg.windshare-44444444', 'lib-dll.windshare-55555555'],
      ),
    ]

    const posixPlan = buildSafeNameRestorePlan({ task, files, platform: 'posix' })
    expect(posixPlan.workingDirectory).toBe('parent-folder')
    expect(posixPlan.targetFolderLabel).toBe('Downloads')
    expect(posixPlan.unsupported).toEqual([])
    expect(posixPlan.restorable.map(step => ({
      kind: step.kind,
      parentPhysical: step.parentPhysicalPath,
      parentLogical: step.parentLogicalPath,
      from: step.fromName,
      to: step.toName,
    }))).toEqual([
      {
        kind: 'file',
        parentPhysical: ['entry-CON.windshare-11111111', 'entry-CON.windshare-22222222', 'sub-cfg.windshare-44444444'],
        parentLogical: ['CON', 'CON', 'sub.cfg'],
        from: 'lib-dll.windshare-55555555',
        to: 'lib.dll',
      },
      {
        kind: 'directory',
        parentPhysical: ['entry-CON.windshare-11111111', 'entry-CON.windshare-22222222'],
        parentLogical: ['CON', 'CON'],
        from: 'empty-cfg.windshare-33333333',
        to: 'empty.cfg',
      },
      {
        kind: 'directory',
        parentPhysical: ['entry-CON.windshare-11111111', 'entry-CON.windshare-22222222'],
        parentLogical: ['CON', 'CON'],
        from: 'sub-cfg.windshare-44444444',
        to: 'sub.cfg',
      },
      {
        kind: 'directory',
        parentPhysical: ['entry-CON.windshare-11111111'],
        parentLogical: ['CON'],
        from: 'entry-CON.windshare-22222222',
        to: 'CON',
      },
      {
        kind: 'directory',
        parentPhysical: [],
        parentLogical: [],
        from: 'entry-CON.windshare-11111111',
        to: 'CON',
      },
    ])

    // On Windows, both CON directories are unsupported, while nested .cfg/.dll entries stay restorable inside their physical parents.
    const winPlan = buildSafeNameRestorePlan({ task, files, platform: 'windows' })
    expect(winPlan.restorable.map(step => step.toName)).toEqual(['lib.dll', 'empty.cfg', 'sub.cfg'])
    expect(winPlan.unsupported.map(entry => ({ logicalPath: entry.logicalPath, reason: entry.reason }))).toEqual([
      { logicalPath: ['CON', 'CON'], reason: 'invalid-for-os' },
      { logicalPath: ['CON'], reason: 'invalid-for-os' },
    ])
    expect(winPlan.totalCount).toBe(5)
  })

  it('distinguishes inside-result, renamed root, single-file, and collisionIndex suffixes', () => {
    const insideDir = buildSafeNameRestorePlan({
      task: makeFSATask({ artifactName: 'my-repo', reservedName: 'my-repo (1)', collisionIndex: 1 }),
      files: [makeFile(['pyvenv.cfg'], ['pyvenv-cfg.windshare-aaaaaaaa'])],
      platform: 'windows',
    })
    expect(insideDir.workingDirectory).toBe('inside-result')
    expect(insideDir.targetFolderLabel).toBe('my-repo (1)')
    expect(insideDir.restorable).toHaveLength(1)
    expect(insideDir.restorable[0]!.parentPhysicalPath).toEqual([])

    const renamedRoot = buildSafeNameRestorePlan({
      task: makeFSATask({ artifactName: 'tool.cfg', reservedName: 'tool-cfg.windshare-bbbbbbbb', collisionIndex: 1, parentName: 'SaveHere' }),
      files: [makeFile(['core.dll'], ['core-dll.windshare-cccccccc'])],
      platform: 'windows',
    })
    expect(renamedRoot.workingDirectory).toBe('parent-folder')
    expect(renamedRoot.targetFolderLabel).toBe('SaveHere')
    expect(renamedRoot.restorable.map(s => [s.fromName, s.toName])).toEqual([
      ['core-dll.windshare-cccccccc', 'core.dll'],
      ['tool-cfg.windshare-bbbbbbbb', 'tool.cfg (1)'],
    ])

    const singleFileSafeRenamed = buildSafeNameRestorePlan({
      task: makeFSATask({
        singleFile: true,
        artifactName: 'plugin.dll',
        reservedName: 'plugin-dll.windshare-dddddddd',
        collisionIndex: 2,
        parentName: 'Bin',
      }),
      files: [makeFile(['plugin.dll'], ['plugin-dll.windshare-dddddddd'])],
      platform: 'windows',
    })
    expect(singleFileSafeRenamed.workingDirectory).toBe('parent-folder')
    expect(singleFileSafeRenamed.targetFolderLabel).toBe('Bin')
    expect(singleFileSafeRenamed.restorable).toEqual([
      {
        kind: 'file',
        parentPhysicalPath: [],
        parentLogicalPath: [],
        fromName: 'plugin-dll.windshare-dddddddd',
        toName: 'plugin (2).dll',
        logicalPath: ['plugin (2).dll'],
      },
    ])

    // Normal single-file collisionIndex > 0 is NOT a safe rename and produces an empty plan.
    const singleFileNormalCollision = buildSafeNameRestorePlan({
      task: makeFSATask({
        singleFile: true,
        artifactName: 'report.pdf',
        reservedName: 'report (1).pdf',
        collisionIndex: 1,
      }),
      files: [makeFile(['report.pdf'], ['report (1).pdf'])],
      platform: 'windows',
    })
    expect(singleFileNormalCollision.totalCount).toBe(0)
  })

  it('rejects non-terminal lifecycles and ignores unfinished files', () => {
    for (const lifecycleKind of ['preparing', 'downloading', 'paused', 'failed'] as const) {
      const plan = buildSafeNameRestorePlan({
        task: makeFSATask({ lifecycleKind }),
        files: [makeFile(['a.dll'], ['a-dll.windshare-12345678'], 'completed')],
        platform: 'windows',
      })
      expect(plan.totalCount).toBe(0)
      expect(plan.restorable).toEqual([])
      expect(plan.unsupported).toEqual([])
    }

    const partialPlan = buildSafeNameRestorePlan({
      task: makeFSATask({ lifecycleKind: 'partial' }),
      files: [
        makeFile(['done.dll'], ['done-dll.windshare-11111111'], 'completed'),
        makeFile(['unfinished.dll'], ['unfinished-dll.windshare-22222222'], 'receiving'),
      ],
      platform: 'windows',
    })
    expect(partialPlan.restorable.map(s => s.toName)).toEqual(['done.dll'])
  })

  it('classifies OS-invalid names and Windows-only case collisions while allowing case-differing siblings on POSIX', () => {
    const task = makeFSATask({ artifactName: 'Bundle', reservedName: 'Bundle' })
    const files = [
      makeFile(['lib.dll'], ['lib-dll.windshare-11111111']),
      makeFile(['pyvenv.cfg'], ['pyvenv-cfg.windshare-22222222']),
      makeFile(['app.lnk'], ['app-lnk.windshare-33333333']),
      makeFile(['app.local'], ['app-local.windshare-44444444']),
      makeFile(['CON'], ['entry-CON.windshare-55555555']),
      makeFile(['a:b'], ['a-b.windshare-66666666']),
      makeFile(['foo.'], ['foo.windshare-77777777']),
      makeFile(['foo', 'inside.txt'], ['foo', 'inside.txt']),
      makeFile(['FOO'], ['FOO.windshare-88888888']),
      makeFile(['Case.cfg'], ['Case-cfg.windshare-99999999']),
      makeFile(['case.cfg'], ['case-cfg.windshare-aaaaaaaa']),
      makeFile(['..'], ['bad.windshare-bbbbbbbb']),
    ]

    const winPlan = buildSafeNameRestorePlan({ task, files, platform: 'windows' })
    expect(winPlan.restorable.map(s => s.toName)).toEqual(['app.lnk', 'app.local', 'lib.dll', 'pyvenv.cfg'])
    expect(winPlan.unsupported.map(u => [u.toName, u.reason])).toEqual([
      ['a:b', 'invalid-for-os'],
      ['case.cfg', 'case-collision'],
      ['Case.cfg', 'case-collision'],
      ['CON', 'invalid-for-os'],
      ['FOO', 'case-collision'],
      ['foo.', 'invalid-for-os'],
    ])
    // Invalid catalog name '..' must never enter either list.
    expect([...winPlan.restorable, ...winPlan.unsupported].some(entry => entry.toName === '..')).toBe(false)

    const posixPlan = buildSafeNameRestorePlan({ task, files, platform: 'posix' })
    expect(posixPlan.unsupported).toEqual([])
    expect(posixPlan.restorable.map(s => s.toName)).toContain('CON')
    expect(posixPlan.restorable.map(s => s.toName)).toContain('Case.cfg')
    expect(posixPlan.restorable.map(s => s.toName)).toContain('case.cfg')
    expect(posixPlan.restorable.map(s => s.toName)).toContain('FOO')
    expect(posixPlan.restorable.map(s => s.toName)).not.toContain('..')
  })
})

describe('plaintext command and artifact generation contracts', () => {
  const adversarialNames = [
    "quote'$(whoami)`id`;echo&1.cfg",
    'smart\u2018\u2019\u201a\u201b[id].dll',
    '-rf.cfg',
    '中文 空格 🚀.cfg',
  ]

  it('generates PS 5.1-compatible, zero-Base64, injection-safe Windows inline commands and JSON manifests', () => {
    const task = makeFSATask({ artifactName: "Proj'[id]’", reservedName: "Proj'[id]’" })
    const files = adversarialNames.map((name, idx) =>
      makeFile([name], [`safe-${idx}.windshare-0000000${idx}`]),
    )
    const plan = buildSafeNameRestorePlan({ task, files, platform: 'windows' })
    const inline = formatInlineRestoreCommand(plan, 'windows')
    expect(inline).not.toBeNull()
    expect(inline!.length).toBeLessThanOrEqual(MAX_INLINE_RESTORE_COMMAND_LENGTH)

    const artifact = renderRestoreArtifact(plan, 'windows')
    expect(artifact.suggestedName).toBe(RESTORE_MANIFEST_FILENAME)
    expect(artifact.fallbackName).toBeNull()
    const manifestCmd = artifact.runCommand(RESTORE_MANIFEST_FILENAME)

    for (const command of [inline!, manifestCmd]) {
      expect(command).toContain('$PWD.ProviderPath')
      expect(command).toContain('[IO.Path]::Combine')
      expect(command).toContain('-LiteralPath')
      expect(command).toContain('-PathType Container')
      expect(command).toContain('ConvertFrom-Json')
      expect(command).not.toMatch(/FromBase64String|scriptblock|ExecutionPolicy|-enc\b|-EncodedCommand/i)
    }
    expect(manifestCmd).toContain('Get-Content -LiteralPath $f -Raw -Encoding UTF8 | ConvertFrom-Json')
    expect(manifestCmd).toContain('Manifest not found')
    expect(manifestCmd).toContain('You can safely delete')

    // Every PowerShell single-quote delimiter inside the JSON literal must be escaped as \uXXXX.
    expect(inline!).toContain('\\u0027')
    expect(inline!).toContain('\\u2018')
    expect(inline!).toContain('\\u2019')
    expect(inline!).toContain('\\u201a')
    expect(inline!).toContain('\\u201b')
    expect(inline!).toContain('中文 空格 🚀.cfg')
    expect(manifestCmd).toContain('\\u0027')
    expect(manifestCmd).toContain('\\u2019')

    const parsedManifest = JSON.parse(serializeRestoreManifestJson(plan))
    expect(parsedManifest.version).toBe(1)
    expect(parsedManifest.targetFolder).toBe("Proj'[id]’")
    expect(parsedManifest.restorable).toHaveLength(adversarialNames.length)
  })

  it('returns null inline command when step count exceeds 10 or serialized length exceeds 2000 chars', () => {
    const task = makeFSATask()
    const elevenFiles = Array.from({ length: MAX_INLINE_RESTORE_STEPS + 1 }, (_, i) =>
      makeFile([`f${i}.dll`], [`f${i}-dll.windshare-${String(i).padStart(8, '0')}`]),
    )
    const bigPlan = buildSafeNameRestorePlan({ task, files: elevenFiles, platform: 'windows' })
    expect(formatInlineRestoreCommand(bigPlan, 'windows')).toBeNull()
    expect(formatInlineRestoreCommand(bigPlan, 'posix')).toBeNull()

    const longDir = 'segment-'.repeat(20)
    const longFiles = Array.from({ length: 8 }, (_, i) =>
      makeFile([longDir, `very-long-library-name-${i}.dll`], [longDir, `very-long-library-name-${i}-dll.windshare-0000000${i}`]),
    )
    const longPlan = buildSafeNameRestorePlan({ task, files: longFiles, platform: 'windows' })
    expect(longPlan.restorable.length).toBeLessThanOrEqual(MAX_INLINE_RESTORE_STEPS)
    expect(formatInlineRestoreCommand(longPlan, 'windows')).toBeNull()
  })

  it('generates strict single-quoted POSIX inline commands and scripts with symlink-safe directory checks', () => {
    const task = makeFSATask({ artifactName: 'My Repo', reservedName: 'My Repo' })
    const files = adversarialNames.map((name, idx) =>
      makeFile([name], [`safe-${idx}.windshare-0000000${idx}`]),
    )
    const plan = buildSafeNameRestorePlan({ task, files, platform: 'posix' })
    const inline = formatInlineRestoreCommand(plan, 'posix')
    expect(inline).not.toBeNull()
    expect(inline!.length).toBeLessThanOrEqual(MAX_INLINE_RESTORE_COMMAND_LENGTH)

    const artifact = renderRestoreArtifact(plan, 'posix')
    expect(artifact.suggestedName).toBe(RESTORE_POSIX_SCRIPT_FILENAME)
    expect(artifact.fallbackName).toBe(RESTORE_POSIX_FALLBACK_FILENAME)
    expect(artifact.runCommand(RESTORE_POSIX_SCRIPT_FILENAME)).toBe('sh ./windshare-restore-names.sh')
    expect(artifact.runCommand('windshare-restore-names (1).txt')).toBe("sh './windshare-restore-names (1).txt'")

    for (const script of [inline!, artifact.content]) {
      expect(script).toContain('! [ -L "$_p1" ] && [ -d "$_p1" ]')
      expect(script).toContain('[ -e "$_dst" ] || [ -L "$_dst" ]')
      expect(script).toContain('mv -- "$_src" "$_dst"')
      expect(script).toContain("'quote'\\''$(whoami)`id`;echo&1.cfg'")
      expect(script).toContain("'-rf.cfg'")
      expect(script).not.toMatch(/base64/i)
    }
    expect(artifact.content).toContain('$(dirname -- "$0")')
    expect(artifact.content).toContain('All filenames restored. You can safely delete')
  })
})
