import { readFileSync } from 'node:fs'
import type { Plugin } from 'vite'

const networkDirectory = new URL('../../deploy/network/', import.meta.url)

export function networkAssets(): Plugin {
  return {
    name: 'windshare-network-export',
    generateBundle() {
      const release = JSON.parse(readFileSync(new URL('release.json', networkDirectory), 'utf8')) as { snapshot: unknown }
      if (release.snapshot === null) return
      const source = readFileSync(new URL('windshare-network.json', networkDirectory), 'utf8')
      if (JSON.stringify(JSON.parse(source)) !== JSON.stringify(release.snapshot)) throw new Error('Directory mirror and embedded snapshot differ; run network-sign export before building.')
      this.emitFile({ type: 'asset', fileName: '.well-known/windshare-network', source })
    },
  }
}
