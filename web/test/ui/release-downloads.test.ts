import { describe, expect, it } from 'vitest'
import {
  collectReleaseDownloads,
  parseReleaseDownloadAsset,
  resolveReleaseDownloads,
} from '../../build/release-downloads'
import {
  detectVisitorEnvironment,
  formatAssetLabel,
  formatDownloadAction,
  selectRecommendedAsset,
} from '../../src/ui/portal/console/release-downloads'
import { en } from '../../src/ui/portal/locales/en'
import { zh } from '../../src/ui/portal/locales/zh'

const RELEASE_ASSETS_V013 = [
  'SHA256SUMS-linux-amd64',
  'SHA256SUMS-windows-amd64',
  'windshare-network.json',
  'windshare-v0.1.3-linux-amd64.zip',
  'windshare-v0.1.3-source.zip',
  'windshare-v0.1.3-windows-amd64.zip',
] as const

describe('build release downloads manifest', () => {
  it('omits the manifest for default and self-hosted builds', () => {
    expect(resolveReleaseDownloads({})).toBeUndefined()
    expect(resolveReleaseDownloads({ WIND_RELEASE_VERSION: '  ', WIND_RELEASE_DOWNLOADS_DIR: '' })).toBeUndefined()
  })

  it('fails closed on partial or malformed release configuration', () => {
    expect(() => resolveReleaseDownloads({ WIND_RELEASE_VERSION: 'v0.1.3' })).toThrow(/must both be set/)
    expect(() => resolveReleaseDownloads({ WIND_RELEASE_DOWNLOADS_DIR: 'fixtures/downloads' })).toThrow(/must both be set/)
    expect(() => collectReleaseDownloads('latest', RELEASE_ASSETS_V013)).toThrow(/Invalid release version/)
    expect(() => collectReleaseDownloads('v0.1.3', ['windshare-v0.1.3-source.zip', 'windshare-network.json']))
      .toThrow(/No client download assets/)
  })

  it('collects only published client binaries with immutable versioned /downloads/ routes', () => {
    const manifest = resolveReleaseDownloads(
      { WIND_RELEASE_VERSION: 'v0.1.3', WIND_RELEASE_DOWNLOADS_DIR: 'fixtures/downloads' },
      () => RELEASE_ASSETS_V013,
    )
    expect(manifest).toEqual({
      version: 'v0.1.3',
      assets: [
        {
          kind: 'cli',
          platform: 'windows',
          arch: 'amd64',
          filename: 'windshare-v0.1.3-windows-amd64.zip',
          href: '/downloads/v0.1.3/windshare-v0.1.3-windows-amd64.zip',
        },
        {
          kind: 'cli',
          platform: 'linux',
          arch: 'amd64',
          filename: 'windshare-v0.1.3-linux-amd64.zip',
          href: '/downloads/v0.1.3/windshare-v0.1.3-linux-amd64.zip',
        },
      ],
    })
  })

  it('ranks desktop packages before CLI archives and prefers native installers over zips', () => {
    const manifest = collectReleaseDownloads('v0.2.0', [
      'windshare-v0.2.0-windows-amd64.zip',
      'windshare-desktop-v0.2.0-windows-x64.zip',
      'windshare-desktop-v0.2.0-windows-amd64.msi',
      'windshare-desktop-v0.2.0-darwin-arm64.dmg',
      'windshare-v0.2.0-linux-amd64.zip',
    ])
    expect(manifest.assets.map(asset => [asset.kind, asset.platform, asset.arch, asset.filename])).toEqual([
      ['desktop', 'windows', 'amd64', 'windshare-desktop-v0.2.0-windows-amd64.msi'],
      ['desktop', 'windows', 'amd64', 'windshare-desktop-v0.2.0-windows-x64.zip'],
      ['desktop', 'macos', 'arm64', 'windshare-desktop-v0.2.0-darwin-arm64.dmg'],
      ['cli', 'windows', 'amd64', 'windshare-v0.2.0-windows-amd64.zip'],
      ['cli', 'linux', 'amd64', 'windshare-v0.2.0-linux-amd64.zip'],
    ])
  })

  it('rejects non-client artifacts and unsafe filenames', () => {
    expect(parseReleaseDownloadAsset('v0.1.3', 'windshare-v0.1.3-source.zip')).toBeUndefined()
    expect(parseReleaseDownloadAsset('v0.1.3', 'windshare-v0.1.3-windows-amd64.zip.sha256')).toBeUndefined()
    expect(parseReleaseDownloadAsset('v0.1.3', '../windshare-v0.1.3-windows-amd64.zip')).toBeUndefined()
    expect(parseReleaseDownloadAsset('v0.1.3', 'windshare-v0.1.4-windows-amd64.zip')).toBeUndefined()
  })
})

describe('portal release download recommendation', () => {
  it('detects desktop OS without misclassifying mobile browsers as Linux or macOS', () => {
    expect(detectVisitorEnvironment({
      userAgent: 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36',
      platform: 'Win32',
    })).toEqual({ platform: 'windows', arch: 'amd64' })
    expect(detectVisitorEnvironment({
      userAgent: 'Mozilla/5.0 (X11; Linux aarch64; rv:136.0) Gecko/20100101 Firefox/136.0',
      platform: 'Linux aarch64',
    })).toEqual({ platform: 'linux', arch: 'arm64' })
    expect(detectVisitorEnvironment({
      userAgent: 'Mozilla/5.0 (Linux; Android 15; Pixel 9) AppleWebKit/537.36 Mobile',
      platform: 'Linux armv8l',
    })).toEqual({ platform: undefined, arch: undefined })
    expect(detectVisitorEnvironment({
      userAgent: 'Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 Mobile',
      platform: 'iPhone',
    })).toEqual({ platform: undefined, arch: undefined })
  })

  it('recommends matching desktop first, falls back to CLI when allowed, and avoids cross-OS or cross-arch guesses', () => {
    const cliOnly = collectReleaseDownloads('v0.1.3', RELEASE_ASSETS_V013)
    const winVisitor = { platform: 'windows', arch: 'amd64' } as const
    const macVisitor = { platform: 'macos', arch: 'arm64' } as const
    const linuxArmVisitor = { platform: 'linux', arch: 'arm64' } as const
    const linuxUnknownArchVisitor = { platform: 'linux', arch: undefined } as const

    expect(selectRecommendedAsset(cliOnly, winVisitor, {
      preferredKind: 'desktop',
      allowOtherKind: true,
    })?.href).toBe('/downloads/v0.1.3/windshare-v0.1.3-windows-amd64.zip')
    expect(selectRecommendedAsset(cliOnly, macVisitor, {
      preferredKind: 'desktop',
      allowOtherKind: true,
    })).toBeUndefined()
    expect(selectRecommendedAsset(cliOnly, linuxArmVisitor, {
      preferredKind: 'desktop',
      allowOtherKind: true,
    })).toBeUndefined()
    expect(selectRecommendedAsset(cliOnly, linuxArmVisitor, {
      preferredKind: 'cli',
      allowOtherKind: false,
    })).toBeUndefined()
    expect(selectRecommendedAsset(cliOnly, linuxUnknownArchVisitor, {
      preferredKind: 'desktop',
      allowOtherKind: true,
    })?.filename).toBe('windshare-v0.1.3-linux-amd64.zip')

    const withDesktop = collectReleaseDownloads('v0.2.0', [
      'windshare-v0.2.0-windows-amd64.zip',
      'windshare-desktop-v0.2.0-windows-amd64.msi',
      'windshare-desktop-v0.2.0-linux-amd64.AppImage',
      'windshare-v0.2.0-linux-arm64.zip',
    ])
    expect(selectRecommendedAsset(withDesktop, winVisitor, {
      preferredKind: 'desktop',
      allowOtherKind: true,
    })?.filename).toBe('windshare-desktop-v0.2.0-windows-amd64.msi')
    expect(selectRecommendedAsset(withDesktop, winVisitor, {
      preferredKind: 'cli',
      allowOtherKind: false,
    })?.filename).toBe('windshare-v0.2.0-windows-amd64.zip')
    expect(selectRecommendedAsset(withDesktop, linuxArmVisitor, {
      preferredKind: 'desktop',
      allowOtherKind: true,
    })?.filename).toBe('windshare-v0.2.0-linux-arm64.zip')
    expect(selectRecommendedAsset(withDesktop, linuxArmVisitor, {
      preferredKind: 'desktop',
      allowOtherKind: false,
    })).toBeUndefined()
  })

  it('formats distinct labels for desktop and CLI packages', () => {
    const manifest = collectReleaseDownloads('v0.2.0', [
      'windshare-v0.2.0-windows-amd64.zip',
      'windshare-desktop-v0.2.0-windows-amd64.msi',
    ])
    const [desktop, cli] = manifest.assets
    expect(desktop && formatDownloadAction(zh, desktop)).toBe('下载 Windows 版')
    expect(cli && formatDownloadAction(zh, cli)).toBe('下载 Windows CLI')
    expect(desktop && formatAssetLabel(en, desktop)).toBe('Windows')
    expect(cli && formatAssetLabel(en, cli)).toBe('Windows CLI')
  })
})
