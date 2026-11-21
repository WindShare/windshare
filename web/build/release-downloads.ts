import { readdirSync } from 'node:fs'

export type ReleaseDownloadKind = 'desktop' | 'cli'
export type ReleaseDownloadPlatform = 'windows' | 'macos' | 'linux'
export type ReleaseDownloadArch = 'amd64' | 'arm64'

export interface ReleaseDownloadAsset {
  readonly kind: ReleaseDownloadKind
  readonly platform: ReleaseDownloadPlatform
  readonly arch: ReleaseDownloadArch
  readonly filename: string
  readonly href: string
}

export interface ReleaseDownloadsManifest {
  readonly version: string
  readonly assets: readonly ReleaseDownloadAsset[]
}

export interface ReleaseDownloadsEnv {
  readonly WIND_RELEASE_VERSION?: string
  readonly WIND_RELEASE_DOWNLOADS_DIR?: string
}

const DOWNLOADS_ROUTE_PREFIX = '/downloads'
const RELEASE_VERSION_PATTERN = /^v\d+\.\d+\.\d+$/
const SAFE_ASSET_NAME_PATTERN = /^[a-zA-Z0-9][a-zA-Z0-9._-]{0,179}$/
const DESKTOP_ARCHIVE_EXTENSIONS = new Set([
  'msi', 'exe', 'msix', 'dmg', 'appimage', 'deb', 'rpm', 'zip', 'tar.gz',
])
const CLI_ARCHIVE_EXTENSIONS = new Set(['zip', 'tar.gz'])

const PLATFORM_ALIASES: Readonly<Record<string, ReleaseDownloadPlatform>> = {
  windows: 'windows',
  darwin: 'macos',
  macos: 'macos',
  linux: 'linux',
}

const ARCH_ALIASES: Readonly<Record<string, ReleaseDownloadArch>> = {
  amd64: 'amd64',
  x64: 'amd64',
  x86_64: 'amd64',
  arm64: 'arm64',
  aarch64: 'arm64',
}

const KIND_PRIORITY: Readonly<Record<ReleaseDownloadKind, number>> = { desktop: 0, cli: 1 }
const PLATFORM_PRIORITY: Readonly<Record<ReleaseDownloadPlatform, number>> = { windows: 0, macos: 1, linux: 2 }
const ARCH_PRIORITY: Readonly<Record<ReleaseDownloadArch, number>> = { amd64: 0, arm64: 1 }
const EXTENSION_PRIORITY: Readonly<Record<string, number>> = {
  msi: 0, exe: 1, msix: 2, dmg: 3, appimage: 4, deb: 5, rpm: 6, zip: 7, 'tar.gz': 8,
}

export function resolveReleaseDownloads(
  env: ReleaseDownloadsEnv,
  listFiles: (directory: string) => readonly string[] = listRegularFiles,
): ReleaseDownloadsManifest | undefined {
  const version = normalizeEnvValue(env.WIND_RELEASE_VERSION)
  const directory = normalizeEnvValue(env.WIND_RELEASE_DOWNLOADS_DIR)
  // Default and self-hosted builds omit official release inputs so the portal
  // links to GitHub Releases instead of assuming same-origin /downloads/ hosting.
  if (version === undefined && directory === undefined) return undefined
  if (version === undefined || directory === undefined) {
    throw new Error('WIND_RELEASE_VERSION and WIND_RELEASE_DOWNLOADS_DIR must both be set.')
  }
  return collectReleaseDownloads(version, listFiles(directory))
}

export function collectReleaseDownloads(
  version: string,
  filenames: readonly string[],
): ReleaseDownloadsManifest {
  if (!RELEASE_VERSION_PATTERN.test(version)) {
    throw new Error(`Invalid release version for downloads manifest: ${version}`)
  }
  const assets = filenames
    .map(filename => parseReleaseDownloadAsset(version, filename))
    .filter((asset): asset is ReleaseDownloadAsset => asset !== undefined)
    .sort(compareReleaseAssets)
  if (assets.length === 0) {
    throw new Error(`No client download assets found for release ${version}.`)
  }
  return { version, assets }
}

export function parseReleaseDownloadAsset(
  version: string,
  filename: string,
): ReleaseDownloadAsset | undefined {
  if (!SAFE_ASSET_NAME_PATTERN.test(filename) || filename === '.' || filename === '..') {
    return undefined
  }
  const desktopPrefix = `windshare-desktop-${version}-`
  const cliPrefix = `windshare-${version}-`
  let kind: ReleaseDownloadKind
  let remainder: string
  if (filename.startsWith(desktopPrefix)) {
    kind = 'desktop'
    remainder = filename.slice(desktopPrefix.length)
  } else if (filename.startsWith(cliPrefix)) {
    kind = 'cli'
    remainder = filename.slice(cliPrefix.length)
  } else {
    return undefined
  }
  const target = parseTargetAndExtension(remainder, kind)
  if (target === undefined) return undefined
  return {
    kind,
    platform: target.platform,
    arch: target.arch,
    filename,
    href: `${DOWNLOADS_ROUTE_PREFIX}/${version}/${filename}`,
  }
}

function parseTargetAndExtension(
  remainder: string,
  kind: ReleaseDownloadKind,
): { readonly platform: ReleaseDownloadPlatform; readonly arch: ReleaseDownloadArch } | undefined {
  const normalized = remainder.toLowerCase()
  const extension = extractArchiveExtension(normalized)
  if (extension === undefined) return undefined
  const allowedExtensions = kind === 'desktop' ? DESKTOP_ARCHIVE_EXTENSIONS : CLI_ARCHIVE_EXTENSIONS
  if (!allowedExtensions.has(extension)) return undefined
  const stem = normalized.slice(0, normalized.length - extension.length - 1)
  const parts = stem.split('-')
  if (parts.length !== 2) return undefined
  const [rawPlatform, rawArch] = parts
  const platform = rawPlatform === undefined ? undefined : PLATFORM_ALIASES[rawPlatform]
  const arch = rawArch === undefined ? undefined : ARCH_ALIASES[rawArch]
  if (platform === undefined || arch === undefined) return undefined
  return { platform, arch }
}

function extractArchiveExtension(lowercaseName: string): string | undefined {
  if (lowercaseName.endsWith('.tar.gz')) return 'tar.gz'
  const dotIndex = lowercaseName.lastIndexOf('.')
  if (dotIndex <= 0 || dotIndex === lowercaseName.length - 1) return undefined
  return lowercaseName.slice(dotIndex + 1)
}

function compareReleaseAssets(left: ReleaseDownloadAsset, right: ReleaseDownloadAsset): number {
  return (KIND_PRIORITY[left.kind] - KIND_PRIORITY[right.kind])
    || (PLATFORM_PRIORITY[left.platform] - PLATFORM_PRIORITY[right.platform])
    || (ARCH_PRIORITY[left.arch] - ARCH_PRIORITY[right.arch])
    || (extensionRank(left.filename) - extensionRank(right.filename))
    || left.filename.localeCompare(right.filename)
}

function extensionRank(filename: string): number {
  const extension = extractArchiveExtension(filename.toLowerCase())
  if (extension === undefined) return Number.MAX_SAFE_INTEGER
  return EXTENSION_PRIORITY[extension] ?? Number.MAX_SAFE_INTEGER
}

function normalizeEnvValue(value: string | undefined): string | undefined {
  const trimmed = value?.trim()
  return trimmed === undefined || trimmed.length === 0 ? undefined : trimmed
}

function listRegularFiles(directory: string): readonly string[] {
  return readdirSync(directory, { withFileTypes: true })
    .filter(entry => entry.isFile())
    .map(entry => entry.name)
}
