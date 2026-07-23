import { createHash } from 'node:crypto'
import { readdir, readFile, stat } from 'node:fs/promises'
import { gzipSync } from 'node:zlib'
import { join, relative, resolve } from 'node:path'

const webRoot = resolve(import.meta.dirname, '..')
const distRoot = join(webRoot, 'dist')
const noticePath = join(distRoot, 'third-party-notices.txt')
const indexPath = join(distRoot, 'index.html')
const requiredNoticeTokens = [
  '@noble/curves 2.2.0',
  '@noble/hashes 2.2.0',
  'Copyright (c) 2022 Paul Miller',
  'Permission is hereby granted, free of charge',
  'The above copyright notice and this permission notice shall be included',
  'THE SOFTWARE IS PROVIDED',
]

const [notice, indexHtml, files] = await Promise.all([
  readFile(noticePath, 'utf8'),
  readFile(indexPath, 'utf8'),
  listFiles(distRoot),
])
for (const token of requiredNoticeTokens) {
  if (!notice.includes(token)) throw new Error(`Production notice is missing: ${token}`)
}

const assets = []
for (const path of files) {
  if (path === noticePath || path === indexPath) continue
  const bytes = await readFile(path)
  const name = relative(distRoot, path).replaceAll('\\', '/')
  assets.push({
    name,
    kind: name.endsWith('.js') ? 'javascript' : name.endsWith('.css') ? 'css' : 'other',
    entry: indexHtml.includes(`/${name}`),
    rawBytes: bytes.byteLength,
    gzipBytes: gzipSync(bytes).byteLength,
  })
}
assets.sort((left, right) => right.rawBytes - left.rawBytes || left.name.localeCompare(right.name))
if (!assets.some((asset) => asset.kind === 'javascript')) {
  throw new Error('Production build emitted no JavaScript asset')
}

console.log(JSON.stringify({
  schema: 1,
  generatedAt: new Date().toISOString(),
  runtime: { node: process.version, platform: process.platform, architecture: process.arch },
  notice: {
    path: 'third-party-notices.txt',
    bytes: Buffer.byteLength(notice),
    sha256: createHash('sha256').update(notice).digest('hex'),
    packages: ['@noble/curves@2.2.0', '@noble/hashes@2.2.0'],
  },
  assets,
}, null, 2))

async function listFiles(directory) {
  const entries = await readdir(directory, { withFileTypes: true })
  const nested = await Promise.all(entries.map(async (entry) => {
    const path = join(directory, entry.name)
    if (entry.isDirectory()) return listFiles(path)
    if (!entry.isFile() || !(await stat(path)).isFile()) return []
    return [path]
  }))
  return nested.flat()
}
