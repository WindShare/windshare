import { access, constants } from 'node:fs/promises'
import { basename, dirname, join } from 'node:path'

import { chromium, firefox, webkit } from '@playwright/test'

import { PLAYWRIGHT_BROWSER_NAMES } from '../playwright.projects.ts'

const browserTypes = Object.freeze({ chromium, firefox, webkit })
const missing = []
const manifest = PLAYWRIGHT_BROWSER_NAMES.map((browserName) => {
  const executable = browserTypes[browserName].executablePath()
  const networkExecutables = networkExecutablePaths(browserName, executable)
  const cacheRoots = [...new Set(networkExecutables.map(playwrightCacheRoot))]
  if (cacheRoots.length !== 1) {
    throw new Error(`${browserName} network processes span multiple Playwright cache roots`)
  }
  return Object.freeze({
    browserName,
    executable,
    networkExecutables,
    cacheRoot: cacheRoots[0],
  })
})

for (const browser of manifest) {
  const candidates = [
    ['browser', browser.executable],
    ...browser.networkExecutables.map((executable) => ['network candidate', executable]),
  ]
  for (const [role, executable] of candidates) {
    try {
      await access(executable, process.platform === 'win32' ? constants.F_OK : constants.X_OK)
    } catch {
      missing.push({ browserName: browser.browserName, executable, role })
    }
  }
}

if (missing.length > 0) {
  const details = missing
    .map(({ browserName, executable, role }) => `- ${browserName} ${role}: ${executable}`)
    .join('\n')
  throw new Error(
    `Playwright browser executables are missing:\n${details}\n` +
    'Install the pinned matrix with: pnpm -C web exec playwright install chromium firefox webkit',
  )
}

if (process.argv.includes('--network-manifest-json')) {
  process.stdout.write(JSON.stringify(manifest))
} else {
  process.stdout.write(
    `Playwright browser preflight: ${PLAYWRIGHT_BROWSER_NAMES.join(', ')} ready\n`,
  )
}

function playwrightCacheRoot(executable) {
  let directory = dirname(executable)
  while (dirname(directory) !== directory) {
    if (/^(?:chromium(?:_headless_shell)?|firefox|webkit)-[0-9]+$/.test(basename(directory))) {
      return dirname(directory)
    }
    directory = dirname(directory)
  }
  throw new Error(`Cannot derive Playwright cache root from: ${executable}`)
}

function networkExecutablePaths(browserName, executable) {
  if (process.platform !== 'win32') return [executable]
  if (browserName === 'webkit') {
    const installation = dirname(executable)
    return [
      executable,
      join(installation, 'WebKitNetworkProcess.exe'),
      join(installation, 'WebKitWebProcess.exe'),
    ]
  }
  if (browserName !== 'chromium') return [executable]

  // Headless Playwright launches the separately pinned shell, which is the Windows firewall identity.
  const installation = dirname(dirname(executable))
  const revision = /^chromium-(?<revision>[0-9]+)$/.exec(basename(installation))?.groups?.revision
  if (revision === undefined) {
    throw new Error(`Cannot derive Chromium headless-shell revision from: ${executable}`)
  }
  return [
    join(
      dirname(installation),
      `chromium_headless_shell-${revision}`,
      'chrome-headless-shell-win64',
      'chrome-headless-shell.exe',
    ),
  ]
}
