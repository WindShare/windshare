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

export interface VisitorEnvironment {
  readonly platform: ReleaseDownloadPlatform | undefined
  readonly arch: ReleaseDownloadArch | undefined
}

export interface VisitorEnvironmentInput {
  readonly userAgent?: string
  readonly platform?: string
  readonly userAgentDataPlatform?: string
}

export interface DownloadCopy {
  readonly downloadDesktopTarget: string
  readonly downloadCliTarget: string
  readonly assetCliLabel: string
}

declare global {
  const __WIND_RELEASE_DOWNLOADS__: ReleaseDownloadsManifest | undefined
}

const MOBILE_PATTERN = /android|iphone|ipad|ipod|mobile/i
const WINDOWS_PATTERN = /win/i
const MACOS_PATTERN = /mac|darwin/i
const LINUX_PATTERN = /linux|x11/i
const ARM64_PATTERN = /arm64|aarch64/i
const AMD64_PATTERN = /x86_64|x64|win64|wow64|amd64/i

const PLATFORM_LABELS: Readonly<Record<ReleaseDownloadPlatform, string>> = {
  windows: 'Windows',
  macos: 'macOS',
  linux: 'Linux',
}

const ARCH_LABELS: Readonly<Record<ReleaseDownloadArch, string>> = {
  amd64: 'x64',
  arm64: 'ARM64',
}

interface NavigatorWithUAData extends Navigator {
  readonly userAgentData?: { readonly platform?: string }
}

export function getReleaseDownloads(): ReleaseDownloadsManifest | undefined {
  return typeof __WIND_RELEASE_DOWNLOADS__ === 'undefined' ? undefined : __WIND_RELEASE_DOWNLOADS__
}

export function detectVisitorEnvironment(source?: VisitorEnvironmentInput): VisitorEnvironment {
  const input = source ?? readNavigatorEnvironment()
  const userAgent = input.userAgent ?? ''
  const platformText = `${input.userAgentDataPlatform ?? ''} ${input.platform ?? ''} ${userAgent}`
  // Mobile browsers often include "Linux" (Android) or "Mac OS X" (iOS) in userAgent;
  // bail out first so mobile visitors are never offered a desktop/CLI binary.
  if (MOBILE_PATTERN.test(platformText)) {
    return { platform: undefined, arch: undefined }
  }
  const platform = detectDesktopPlatform(platformText)
  return { platform, arch: detectArch(userAgent) }
}

export function selectRecommendedAsset(
  manifest: ReleaseDownloadsManifest,
  visitor: VisitorEnvironment,
  options: { readonly preferredKind: ReleaseDownloadKind; readonly allowOtherKind: boolean },
): ReleaseDownloadAsset | undefined {
  if (visitor.platform === undefined) return undefined
  const forPlatform = manifest.assets.filter(asset => asset.platform === visitor.platform)
  if (forPlatform.length === 0) return undefined
  const preferred = forPlatform.filter(asset => asset.kind === options.preferredKind)
  if (preferred.length > 0) return pickByArch(preferred, visitor.arch)
  if (!options.allowOtherKind) return undefined
  return pickByArch(forPlatform, visitor.arch)
}

export function formatDownloadAction(copy: DownloadCopy, asset: ReleaseDownloadAsset): string {
  const template = asset.kind === 'desktop' ? copy.downloadDesktopTarget : copy.downloadCliTarget
  return template.replace('{platform}', PLATFORM_LABELS[asset.platform])
}

export function formatAssetLabel(copy: DownloadCopy, asset: ReleaseDownloadAsset): string {
  const platform = PLATFORM_LABELS[asset.platform]
  return asset.kind === 'desktop' ? platform : copy.assetCliLabel.replace('{platform}', platform)
}

export function formatArchLabel(arch: ReleaseDownloadArch): string {
  return ARCH_LABELS[arch]
}

function readNavigatorEnvironment(): VisitorEnvironmentInput {
  if (typeof navigator === 'undefined') return {}
  const nav = navigator as NavigatorWithUAData
  const userAgentDataPlatform = nav.userAgentData?.platform
  return {
    userAgent: nav.userAgent,
    platform: nav.platform,
    ...(userAgentDataPlatform === undefined ? {} : { userAgentDataPlatform }),
  }
}

function detectDesktopPlatform(text: string): ReleaseDownloadPlatform | undefined {
  if (WINDOWS_PATTERN.test(text)) return 'windows'
  if (MACOS_PATTERN.test(text)) return 'macos'
  if (LINUX_PATTERN.test(text)) return 'linux'
  return undefined
}

function detectArch(userAgent: string): ReleaseDownloadArch | undefined {
  if (ARM64_PATTERN.test(userAgent)) return 'arm64'
  if (AMD64_PATTERN.test(userAgent)) return 'amd64'
  return undefined
}

function pickByArch(
  candidates: readonly ReleaseDownloadAsset[],
  arch: ReleaseDownloadArch | undefined,
): ReleaseDownloadAsset | undefined {
  if (arch !== undefined) {
    const exact = candidates.find(asset => asset.arch === arch)
    if (exact !== undefined) return exact
  }
  return candidates[0]
}
