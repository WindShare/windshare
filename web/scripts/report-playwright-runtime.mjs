import { readFile } from 'node:fs/promises'
import { createRequire } from 'node:module'
import { dirname, join } from 'node:path'

const browserNames = new Set(['chromium', 'firefox', 'webkit'])
const rootRequire = createRequire(import.meta.url)
const playwrightTestEntry = rootRequire.resolve('@playwright/test')
const playwrightRequire = createRequire(playwrightTestEntry)
const playwrightCoreEntry = playwrightRequire.resolve('playwright-core')

const [playwrightTestPackage, playwrightCorePackage, browserCatalog] = await Promise.all([
  readJson(join(dirname(playwrightTestEntry), 'package.json')),
  readJson(join(dirname(playwrightCoreEntry), 'package.json')),
  readJson(join(dirname(playwrightCoreEntry), 'browsers.json')),
])
const browsers = browserCatalog.browsers
  .filter((browser) => browserNames.has(browser.name))
  .map((browser) => ({
    name: browser.name,
    revision: String(browser.revision),
    browserVersion: browser.browserVersion,
  }))
if (browsers.length !== browserNames.size) {
  throw new Error(`Expected exact Chromium, Firefox, and WebKit revisions; found ${browsers.length}`)
}

console.log(JSON.stringify({
  schema: 1,
  playwrightTestVersion: playwrightTestPackage.version,
  playwrightCoreVersion: playwrightCorePackage.version,
  browsers,
}))

async function readJson(path) {
  return JSON.parse(await readFile(path, 'utf8'))
}
