import { catalogNameCollisionKey, isCatalogName } from '../../catalog/path-policy'
import type {
  RestorePlatform,
  SafeNameRestoreArtifact,
  SafeNameRestorePlan,
  SafeNameRestoreStep,
  SafeNameUnsupportedEntry,
} from '../model/safe-name-restore'
import type { ReceiveFileRecord, ReceiveTaskRecord } from '../store/types'
import { decideCollisionName, fitsPortableOutputNameBudget, isPortableOutputName } from './policy'

export const MAX_INLINE_RESTORE_STEPS = 10
export const MAX_INLINE_RESTORE_COMMAND_LENGTH = 2000
export const RESTORE_MANIFEST_FILENAME = 'windshare-restore-names.json'
export const RESTORE_POSIX_SCRIPT_FILENAME = 'windshare-restore-names.sh'
export const RESTORE_POSIX_FALLBACK_FILENAME = 'windshare-restore-names.txt'

const RESTORE_MANIFEST_VERSION = 1
const JSON_MEDIA_TYPE = 'application/json;charset=utf-8'
const TEXT_MEDIA_TYPE = 'text/plain;charset=utf-8'
const SAFE_UNQUOTED_POSIX_FILENAME = /^[A-Za-z0-9._-]+$/
// PowerShell treats ASCII apostrophe and four Unicode quote codepoints as single-quote delimiters;
// U+2028/U+2029 act as line terminators in some hosts.
const POWERSHELL_SINGLE_QUOTE_UNSAFE = /['\u2018\u2019\u201a\u201b\u2028\u2029]/g

export interface RestorePlatformNavigator {
  readonly userAgentData?: { readonly platform?: string }
  readonly platform?: string
  readonly userAgent?: string
}

export interface SafeNameRestorePlanInput {
  readonly task: Pick<ReceiveTaskRecord, 'artifact' | 'target' | 'lifecycle' | 'safeRenamedDirectories'>
  readonly files: readonly Pick<ReceiveFileRecord, 'path' | 'fsaPath' | 'status'>[]
  readonly platform: RestorePlatform
}

interface RawStep {
  readonly kind: 'file' | 'directory'
  readonly parentPhysicalPath: readonly string[]
  readonly parentLogicalPath: readonly string[]
  readonly fromName: string
  readonly toName: string
  readonly logicalPath: readonly string[]
}

export function detectRestorePlatform(
  navigatorLike: RestorePlatformNavigator | undefined = globalThis.navigator as RestorePlatformNavigator | undefined,
): RestorePlatform {
  const hint = navigatorLike?.userAgentData?.platform ||
    navigatorLike?.platform ||
    navigatorLike?.userAgent ||
    ''
  return /win/i.test(hint) ? 'windows' : 'posix'
}

export function buildSafeNameRestorePlan(input: SafeNameRestorePlanInput): SafeNameRestorePlan {
  const { task, files, platform } = input
  if (
    task.target.kind !== 'fsa' ||
    (task.lifecycle.kind !== 'saved' && task.lifecycle.kind !== 'partial')
  ) {
    return emptyPlan(platform, 'inside-result', '')
  }
  if (task.target.singleFile) {
    return buildSingleFilePlan(task, files, platform)
  }
  return buildDirectoryPlan(task, files, platform)
}

export function serializeRestoreManifestJson(plan: SafeNameRestorePlan): string {
  const payload = {
    version: RESTORE_MANIFEST_VERSION,
    workingDirectory: plan.workingDirectory,
    targetFolder: plan.targetFolderLabel,
    restorable: plan.restorable.map(step => ({
      kind: step.kind,
      parentPhysicalPath: [...step.parentPhysicalPath],
      parentLogicalPath: [...step.parentLogicalPath],
      fromName: step.fromName,
      toName: step.toName,
      logicalPath: [...step.logicalPath],
    })),
    unsupported: plan.unsupported.map(entry => ({
      kind: entry.kind,
      logicalPath: [...entry.logicalPath],
      physicalPath: [...entry.physicalPath],
      fromName: entry.fromName,
      toName: entry.toName,
      reason: entry.reason,
    })),
  }
  return `${JSON.stringify(payload, null, 2)}\n`
}

export function formatInlineRestoreCommand(
  plan: SafeNameRestorePlan,
  platform: RestorePlatform = plan.platform,
): string | null {
  if (plan.restorable.length === 0 || plan.restorable.length > MAX_INLINE_RESTORE_STEPS) {
    return null
  }
  const command = platform === 'windows'
    ? buildWindowsInlineCommand(plan)
    : buildPosixInlineCommand(plan)
  return command.length <= MAX_INLINE_RESTORE_COMMAND_LENGTH ? command : null
}

export function renderRestoreArtifact(
  plan: SafeNameRestorePlan,
  platform: RestorePlatform = plan.platform,
): SafeNameRestoreArtifact {
  if (platform === 'windows') {
    return Object.freeze({
      suggestedName: RESTORE_MANIFEST_FILENAME,
      fallbackName: null,
      mediaType: JSON_MEDIA_TYPE,
      content: serializeRestoreManifestJson(plan),
      runCommand: (fileName: string) => buildWindowsManifestCommand(fileName, plan.targetFolderLabel),
    })
  }
  return Object.freeze({
    suggestedName: RESTORE_POSIX_SCRIPT_FILENAME,
    fallbackName: RESTORE_POSIX_FALLBACK_FILENAME,
    mediaType: TEXT_MEDIA_TYPE,
    content: buildPosixScriptFile(plan),
    runCommand: (fileName: string) => formatPosixRunCommand(fileName),
  })
}

function buildSingleFilePlan(
  task: SafeNameRestorePlanInput['task'],
  files: SafeNameRestorePlanInput['files'],
  platform: RestorePlatform,
): SafeNameRestorePlan {
  if (task.target.kind !== 'fsa') return emptyPlan(platform, 'parent-folder', '')
  const targetFolderLabel = task.target.parentHandle.name
  const logicalRootName = decideCollisionName(task.artifact.name, 'file', task.target.collisionIndex)
  const fromName = task.target.reservedName
  const hasCompleted = files.some(file => file.status === 'completed')
  if (!hasCompleted || fromName === logicalRootName) {
    return emptyPlan(platform, 'parent-folder', targetFolderLabel)
  }
  if (!isCatalogName(fromName) || !isCatalogName(logicalRootName)) {
    return emptyPlan(platform, 'parent-folder', targetFolderLabel)
  }
  if (!isNameValidForPlatform(logicalRootName, platform)) {
    const unsupported: SafeNameUnsupportedEntry = Object.freeze({
      kind: 'file',
      logicalPath: Object.freeze([logicalRootName]),
      physicalPath: Object.freeze([fromName]),
      fromName,
      toName: logicalRootName,
      reason: 'invalid-for-os',
    })
    return Object.freeze({
      platform,
      workingDirectory: 'parent-folder',
      targetFolderLabel,
      restorable: Object.freeze([]),
      unsupported: Object.freeze([unsupported]),
      totalCount: 1,
    })
  }
  const step: SafeNameRestoreStep = Object.freeze({
    kind: 'file',
    parentPhysicalPath: Object.freeze([]),
    parentLogicalPath: Object.freeze([]),
    fromName,
    toName: logicalRootName,
    logicalPath: Object.freeze([logicalRootName]),
  })
  return Object.freeze({
    platform,
    workingDirectory: 'parent-folder',
    targetFolderLabel,
    restorable: Object.freeze([step]),
    unsupported: Object.freeze([]),
    totalCount: 1,
  })
}

function buildDirectoryPlan(
  task: SafeNameRestorePlanInput['task'],
  files: SafeNameRestorePlanInput['files'],
  platform: RestorePlatform,
): SafeNameRestorePlan {
  if (task.target.kind !== 'fsa') return emptyPlan(platform, 'inside-result', '')
  const logicalRootName = decideCollisionName(task.artifact.name, 'directory', task.target.collisionIndex)
  const physicalRootName = task.target.reservedName
  const rootRenamed = physicalRootName !== logicalRootName
  const workingDirectory = rootRenamed ? 'parent-folder' : 'inside-result'
  const targetFolderLabel = rootRenamed ? task.target.parentHandle.name : physicalRootName
  if (rootRenamed && (!isCatalogName(physicalRootName) || !isCatalogName(logicalRootName))) {
    return emptyPlan(platform, workingDirectory, targetFolderLabel)
  }

  const collector = createDirectoryPlanCollector(rootRenamed ? [physicalRootName] : [], rootRenamed ? [logicalRootName] : [])
  if (rootRenamed) {
    collector.addRoot(physicalRootName, logicalRootName)
  }
  for (const file of files) {
    if (file.status === 'completed' && file.fsaPath !== undefined) {
      collector.recordPath(file.path, file.fsaPath, 'file')
    }
  }
  for (const dir of task.safeRenamedDirectories ?? []) {
    collector.recordPath(dir.logicalPath, dir.physicalPath, 'directory')
  }

  const { restorable, unsupported } = classifyDirectorySteps(collector, platform)
  return Object.freeze({
    platform,
    workingDirectory,
    targetFolderLabel,
    restorable,
    unsupported,
    totalCount: restorable.length + unsupported.length,
  })
}

function createDirectoryPlanCollector(
  physicalPrefix: readonly string[],
  logicalPrefix: readonly string[],
) {
  const siblingsByParent = new Map<string, Set<string>>()
  const stepsByKey = new Map<string, RawStep>()

  const registerSibling = (parentLogical: readonly string[], name: string) => {
    const key = JSON.stringify(parentLogical)
    let names = siblingsByParent.get(key)
    if (names === undefined) {
      names = new Set<string>()
      siblingsByParent.set(key, names)
    }
    names.add(name)
  }

  const addStep = (step: RawStep) => {
    const key = `${JSON.stringify(step.parentPhysicalPath)}\0${step.fromName}`
    if (!stepsByKey.has(key)) stepsByKey.set(key, step)
  }

  return {
    siblingsByParent,
    stepsByKey,
    addRoot(physicalRootName: string, logicalRootName: string) {
      registerSibling([], logicalRootName)
      addStep({
        kind: 'directory',
        parentPhysicalPath: [],
        parentLogicalPath: [],
        fromName: physicalRootName,
        toName: logicalRootName,
        logicalPath: [logicalRootName],
      })
    },
    recordPath(logical: readonly string[], physical: readonly string[], leafKind: 'file' | 'directory') {
      if (logical.length === 0 || logical.length !== physical.length) return
      if (!allCatalogNames(logical) || !allCatalogNames(physical)) return
      const fullLogical = [...logicalPrefix, ...logical]
      const fullPhysical = [...physicalPrefix, ...physical]
      for (let index = logicalPrefix.length; index < fullLogical.length; index += 1) {
        const parentLogical = fullLogical.slice(0, index)
        const toName = fullLogical[index]!
        const fromName = fullPhysical[index]!
        registerSibling(parentLogical, toName)
        if (fromName !== toName) {
          addStep({
            kind: index === fullLogical.length - 1 ? leafKind : 'directory',
            parentPhysicalPath: fullPhysical.slice(0, index),
            parentLogicalPath: parentLogical,
            fromName,
            toName,
            logicalPath: fullLogical.slice(0, index + 1),
          })
        }
      }
    },
  }
}

function classifyDirectorySteps(
  collector: ReturnType<typeof createDirectoryPlanCollector>,
  platform: RestorePlatform,
): {
  readonly restorable: readonly SafeNameRestoreStep[]
  readonly unsupported: readonly SafeNameUnsupportedEntry[]
} {
  const ordered = [...collector.stepsByKey.values()].sort(compareStepsDeepestFirst)
  const restorable: SafeNameRestoreStep[] = []
  const unsupported: SafeNameUnsupportedEntry[] = []

  for (const step of ordered) {
    if (!isNameValidForPlatform(step.toName, platform)) {
      unsupported.push(freezeUnsupported(step, 'invalid-for-os'))
      continue
    }
    if (platform === 'windows' && hasSiblingCaseCollision(collector.siblingsByParent, step.parentLogicalPath, step.toName)) {
      unsupported.push(freezeUnsupported(step, 'case-collision'))
      continue
    }
    restorable.push(Object.freeze({
      kind: step.kind,
      parentPhysicalPath: Object.freeze([...step.parentPhysicalPath]),
      parentLogicalPath: Object.freeze([...step.parentLogicalPath]),
      fromName: step.fromName,
      toName: step.toName,
      logicalPath: Object.freeze([...step.logicalPath]),
    }))
  }

  return { restorable: Object.freeze(restorable), unsupported: Object.freeze(unsupported) }
}

function emptyPlan(
  platform: RestorePlatform,
  workingDirectory: SafeNameRestorePlan['workingDirectory'],
  targetFolderLabel: string,
): SafeNameRestorePlan {
  return Object.freeze({
    platform,
    workingDirectory,
    targetFolderLabel,
    restorable: Object.freeze([]),
    unsupported: Object.freeze([]),
    totalCount: 0,
  })
}

function allCatalogNames(segments: readonly string[]): boolean {
  return segments.every(segment => isCatalogName(segment))
}

function isNameValidForPlatform(name: string, platform: RestorePlatform): boolean {
  if (!isCatalogName(name)) return false
  return platform === 'windows'
    ? isPortableOutputName(name)
    : fitsPortableOutputNameBudget(name)
}

function hasSiblingCaseCollision(
  siblingsByParent: ReadonlyMap<string, ReadonlySet<string>>,
  parentLogicalPath: readonly string[],
  toName: string,
): boolean {
  const siblings = siblingsByParent.get(JSON.stringify(parentLogicalPath))
  if (siblings === undefined || siblings.size < 2) return false
  const targetFold = catalogNameCollisionKey(toName)
  for (const sibling of siblings) {
    if (sibling !== toName && catalogNameCollisionKey(sibling) === targetFold) {
      return true
    }
  }
  return false
}

function compareStepsDeepestFirst(left: RawStep, right: RawStep): number {
  const depthDiff = right.parentPhysicalPath.length - left.parentPhysicalPath.length
  if (depthDiff !== 0) return depthDiff
  if (left.kind !== right.kind) return left.kind === 'file' ? -1 : 1
  return left.logicalPath.join('/').localeCompare(right.logicalPath.join('/'))
}

function freezeUnsupported(
  step: RawStep,
  reason: SafeNameUnsupportedEntry['reason'],
): SafeNameUnsupportedEntry {
  return Object.freeze({
    kind: step.kind,
    logicalPath: Object.freeze([...step.logicalPath]),
    physicalPath: Object.freeze([...step.parentPhysicalPath, step.fromName]),
    fromName: step.fromName,
    toName: step.toName,
    reason,
  })
}

function escapeJsonForPowerShellSingleQuote(json: string): string {
  return json.replace(POWERSHELL_SINGLE_QUOTE_UNSAFE, ch =>
    `\\u${ch.charCodeAt(0).toString(16).padStart(4, '0')}`)
}

function buildWindowsInlineCommand(plan: SafeNameRestorePlan): string {
  const inlineData = {
    targetFolder: plan.targetFolderLabel,
    restorable: plan.restorable.map(step => {
      const sameParents = step.parentPhysicalPath.length === step.parentLogicalPath.length &&
        step.parentPhysicalPath.every((seg, i) => seg === step.parentLogicalPath[i])
      return {
        ...(step.parentPhysicalPath.length > 0 ? { parentPhysicalPath: [...step.parentPhysicalPath] } : {}),
        ...(!sameParents && step.parentLogicalPath.length > 0 ? { parentLogicalPath: [...step.parentLogicalPath] } : {}),
        fromName: step.fromName,
        toName: step.toName,
      }
    }),
  }
  const literal = escapeJsonForPowerShellSingleQuote(JSON.stringify(inlineData))
  return buildPowerShellExecutor(`$m = '${literal}' | ConvertFrom-Json`, null)
}

function buildWindowsManifestCommand(fileName: string, targetFolderLabel: string): string {
  const cfgLiteral = escapeJsonForPowerShellSingleQuote(JSON.stringify({
    file: fileName,
    target: targetFolderLabel,
  }))
  const sourceInit = [
    `$c = '${cfgLiteral}' | ConvertFrom-Json`,
    '$f = [IO.Path]::Combine($r, [string]$c.file)',
    'if (-not (Test-Path -LiteralPath $f -PathType Leaf)) { Write-Host "Manifest not found: $($c.file). Confirm current directory is \'$($c.target)\'."; return }',
    '$m = Get-Content -LiteralPath $f -Raw -Encoding UTF8 | ConvertFrom-Json',
  ].join('; ')
  return buildPowerShellExecutor(sourceInit, '$c.file')
}

/**
 * Single PowerShell 5.1 & 7 executor contract shared by inline commands and manifest-backed runs.
 * Uses only plaintext JSON + ConvertFrom-Json, $PWD.ProviderPath, [IO.Path]::Combine, and -LiteralPath.
 */
function buildPowerShellExecutor(sourceInit: string, cleanupFileExpr: string | null): string {
  const statements = [
    '$r=$PWD.ProviderPath;$ok=$sk=$cf=$ms=$fl=0;$b=@{}',
    '$pc={param($a,$b)[IO.Path]::Combine($a,[string]$b)};$td={param($x)Test-Path -LiteralPath $x -PathType Container};$tp={param($x)Test-Path -LiteralPath $x}',
    sourceInit,
    'foreach($s in @($m.restorable)){' +
      '$pp=@();if($s.parentPhysicalPath){$pp=@($s.parentPhysicalPath)};' +
      '$pl=$pp;if($s.parentLogicalPath){$pl=@($s.parentLogicalPath)};' +
      '$fr=[string]$s.fromName;$to=[string]$s.toName;$k=(@($pp)+@($fr))-join"/";' +
      'if($b.ContainsKey($k)){$fl++;Write-Host "SKIP(blocked): $k";continue}' +
      '$d=$r;$st=0;' +
      'for($i=0;$i -lt $pp.Count;$i++){' +
        '$p1=&$pc $d $pp[$i];$p2=&$pc $d $pl[$i];$e1=&$td $p1;' +
        'if($pp[$i] -ceq $pl[$i]){if($e1){$d=$p1}else{$st=2;break}}' +
        'else{$e2=&$td $p2;if($e1 -and $e2){$st=1;break}elseif($e1){$d=$p1}elseif($e2){$d=$p2}else{$st=2;break}}' +
      '}' +
      'if($st -eq 1){$cf++;Write-Host "CONFLICT: $k";continue}' +
      'if($st -eq 2){$ms++;Write-Host "MISSING: $k";continue}' +
      '$src=&$pc $d $fr;$dst=&$pc $d $to;$hs=&$tp $src;$hd=&$tp $dst;' +
      'if($hs -and -not $hd){' +
        'try{Rename-Item -LiteralPath $src -NewName $to -ErrorAction Stop;$ok++}' +
        'catch{$fl++;Write-Host "FAILED: $src -> $to ($($_.Exception.Message))";for($j=1;$j -le $pp.Count;$j++){$b[($pp[0..($j-1)]-join"/")]=1}}' +
      '}elseif(-not $hs -and $hd){$sk++}' +
      'elseif($hs -and $hd){$cf++;Write-Host "CONFLICT: $dst"}' +
      'else{$ms++;Write-Host "MISSING: $src"}' +
    '}',
    'if($ok -eq 0 -and $sk -eq 0 -and $ms -gt 0){Write-Host "Confirm current directory is \'$($m.targetFolder)\'."}',
    'Write-Host "Restored $ok, already restored $sk, conflicts $cf, missing $ms, failed $fl"',
  ]
  if (cleanupFileExpr !== null) {
    statements.push(
      `if($cf -eq 0 -and $ms -eq 0 -and $fl -eq 0){Write-Host "All filenames restored. You can safely delete $(${cleanupFileExpr})."}`,
    )
  }
  return `& { ${statements.join('; ')} }`
}

function quotePosix(value: string): string {
  return `'${value.replaceAll("'", "'\\''")}'`
}

function formatPosixRunCommand(fileName: string): string {
  const pathArg = `./${fileName}`
  return SAFE_UNQUOTED_POSIX_FILENAME.test(fileName)
    ? `sh ${pathArg}`
    : `sh ${quotePosix(pathArg)}`
}

function buildPosixStepCalls(plan: SafeNameRestorePlan, separator: string): string {
  return plan.restorable.map(step => {
    const parts: string[] = ['_d=$_b;_st=0;_pk="";_anc=""']
    for (let i = 0; i < step.parentPhysicalPath.length; i += 1) {
      const phys = step.parentPhysicalPath[i]!
      const log = step.parentLogicalPath[i]!
      parts.push(phys === log ? `p ${quotePosix(phys)}` : `p ${quotePosix(phys)} ${quotePosix(log)}`)
    }
    parts.push(`r ${quotePosix(step.fromName)} ${quotePosix(step.toName)}`)
    return parts.join(';')
  }).join(separator)
}

function buildPosixHelpers(inline: boolean): string {
  const sep = inline ? '; ' : '\n'
  const lines = [
    '_nl=$(printf \'\\nx\');_nl=${_nl%x};ok=0;sk=0;cf=0;ms=0;fl=0;_blk=""',
    'is_b(){ _r=$_blk;while [ -n "$_r" ];do _c=${_r%%"$_nl"*};_r=${_r#*"$_nl"};[ "$_c" = "$1" ]&&return 0;done;return 1;}',
    'p(){ _pk="${_pk:+$_pk/}$1";_anc="${_anc}${_pk}${_nl}";[ $_st = 0 ]||return 0;_l=${2-$1};_p1="$_d/$1";_p2="$_d/$_l";if [ "$1" = "$_l" ];then ! [ -L "$_p1" ] && [ -d "$_p1" ]&&_d="$_p1"||_st=2;else _e1=0;! [ -L "$_p1" ] && [ -d "$_p1" ]&&_e1=1;_e2=0;! [ -L "$_p2" ] && [ -d "$_p2" ]&&_e2=1;if [ $_e1 = 1 ]&&[ $_e2 = 1 ];then _st=1;elif [ $_e1 = 1 ];then _d="$_p1";elif [ $_e2 = 1 ];then _d="$_p2";else _st=2;fi;fi;}',
    'r(){ _k="${_pk:+$_pk/}$1";if is_b "$_k";then fl=$((fl+1));printf \'SKIP(blocked): %s\\n\' "$_k";return 0;fi;if [ $_st = 1 ];then cf=$((cf+1));printf \'CONFLICT: %s\\n\' "$_k";return 0;fi;if [ $_st = 2 ];then ms=$((ms+1));printf \'MISSING: %s\\n\' "$_k";return 0;fi;_src="$_d/$1";_dst="$_d/$2";_hs=0;[ -e "$_src" ] || [ -L "$_src" ]&&_hs=1;_hd=0;[ -e "$_dst" ] || [ -L "$_dst" ]&&_hd=1;if [ $_hs = 1 ]&&[ $_hd = 0 ];then if mv -- "$_src" "$_dst";then ok=$((ok+1));else fl=$((fl+1));printf \'FAILED: %s -> %s\\n\' "$_src" "$_dst";_blk="${_blk}${_anc}";fi;elif [ $_hs = 0 ]&&[ $_hd = 1 ];then sk=$((sk+1));elif [ $_hs = 1 ]&&[ $_hd = 1 ];then cf=$((cf+1));printf \'CONFLICT: %s\\n\' "$_dst";else ms=$((ms+1));printf \'MISSING: %s\\n\' "$_src";fi;}',
  ]
  return lines.join(sep)
}

function buildPosixInlineCommand(plan: SafeNameRestorePlan): string {
  const parts = [
    '_b=$PWD',
    buildPosixHelpers(true),
    buildPosixStepCalls(plan, ';'),
    `[ $ok = 0 ]&&[ $sk = 0 ]&&[ $ms -gt 0 ]&&printf 'Confirm current directory is %s.\\n' ${quotePosix(plan.targetFolderLabel)}`,
    'printf \'Restored %d, already restored %d, conflicts %d, missing %d, failed %d\\n\' "$ok" "$sk" "$cf" "$ms" "$fl"',
  ]
  return `(${parts.join(';')})`
}

function buildPosixScriptFile(plan: SafeNameRestorePlan): string {
  const lines = [
    '#!/bin/sh',
    '# WindShare safe-name restoration script (plaintext, idempotent, never overwrites)',
    '_b=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)',
    buildPosixHelpers(false),
    buildPosixStepCalls(plan, '\n'),
    `[ $ok = 0 ]&&[ $sk = 0 ]&&[ $ms -gt 0 ]&&printf 'Confirm current directory is %s.\\n' ${quotePosix(plan.targetFolderLabel)}`,
    'printf \'Restored %d, already restored %d, conflicts %d, missing %d, failed %d\\n\' "$ok" "$sk" "$cf" "$ms" "$fl"',
    '[ $cf = 0 ]&&[ $ms = 0 ]&&[ $fl = 0 ]&&printf \'All filenames restored. You can safely delete %s.\\n\' "$(basename -- "$0")"',
  ]
  return `${lines.join('\n')}\n`
}
