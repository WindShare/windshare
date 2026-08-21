/// <reference types="vite/client" />

import { createHash } from 'node:crypto'
import { readFileSync } from 'node:fs'
import { describe, expect, it } from 'vitest'
import rawTemplate from '../../src/output/file-system-access/compatible-name/restoration/windows-v1.ps1?raw'

const WINDOWS_TEMPLATE_URL = new URL(
  '../../src/output/file-system-access/compatible-name/restoration/windows-v1.ps1',
  import.meta.url,
)
const WINDOWS_TEMPLATE_SHA256 =
  '5f405744ae8b39a15403bbe67d51a930f65c1189ea8082594d5ed3a9fdb394c0'
const templateBytes = readFileSync(WINDOWS_TEMPLATE_URL)
const template = templateBytes.toString('utf8')

describe('Windows compatible-name restoration template', () => {
  it('pins the reviewed production asset byte for byte', () => {
    expect(rawTemplate).toBe(template)
    expect(createHash('sha256').update(templateBytes).digest('hex'))
      .toBe(WINDOWS_TEMPLATE_SHA256)
    expect(template).toContain(
      "$script:WindShareRestorationTemplateId = 'windows-powershell-v1'",
    )
    expect(template).toContain(
      "$script:WindShareSidecarVersion = 'windshare-name-restoration/v1'",
    )
  })

  it('owns one exact Unicode MoveFileExW flags=0 primitive', () => {
    expect(template).toContain(
      'EntryPoint = "MoveFileExW"',
    )
    expect(template).toMatch(
      /NativeMethods\]::MoveFileExW\([\s\S]*?\[uint32\]0\s*\)/u,
    )
    expect(template).not.toMatch(
      /MOVEFILE_(?:REPLACE_EXISTING|COPY_ALLOWED)|Rename-Item|Move-Item/u,
    )
    expect(template).toContain(
      'function Invoke-WindShareNoReplaceMove',
    )
    expect(template).toContain(
      "if ($MyInvocation.InvocationName -ne '.')",
    )
  })

  it('documents the one-process command that runs under the default Windows policy', () => {
    expect(template).toContain(
      'powershell.exe -NoProfile -ExecutionPolicy Bypass -File <script> ' +
      '-SidecarPath <adjacent-sidecar>',
    )
  })

  it('keeps every path-state branch explicit and non-destructive', () => {
    expect(template).toContain(
      'if ($sourcePresent -and -not $targetPresent)',
    )
    expect(template).toContain(
      'if (-not $sourcePresent -and $targetPresent)',
    )
    expect(template).toContain(
      'if ($sourcePresent -and $targetPresent)',
    )
    expect(template).toContain(
      "throw \"WindShare restoration is missing both names for '$($Record.LogicalPath)'.\"",
    )
    expect(template).not.toMatch(/Remove-Item|DeleteFile|RemoveDirectory/u)
  })

  it('validates complete checkpoints before deepest-first execution', () => {
    expect(template).toContain(
      "StartsWith(\"F`t\", [StringComparison]::Ordinal)",
    )
    expect(template).toContain(
      'WindShare sidecar mapping ordinal $ordinal is not contiguous',
    )
    expect(template).toContain(
      'WindShare sidecar contains duplicate logical path',
    )
    expect(template).toContain(
      'WindShare sidecar logical path',
    )
    expect(template).toContain(
      '@{ Expression = { $_.Depth }; Descending = $true }',
    )
    expect(template).toContain(
      "if ($checkpoint.State -ceq 'active')",
    )
  })
})
