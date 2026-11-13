import { readFileSync } from 'node:fs'
import type { Plugin } from 'vite'

export function networkAssets(): Plugin {
  return {
    name: 'windshare-network-export',
    generateBundle() {
      const release = JSON.parse(readFileSync(new URL('../../deploy/network/release.json', import.meta.url), 'utf8')) as { snapshot: unknown }
      if (release.snapshot === null) return
      const source = readFileSync(new URL('../../deploy/network/windshare-network.json', import.meta.url), 'utf8')
      if (JSON.stringify(JSON.parse(source)) !== JSON.stringify(release.snapshot)) throw new Error('Directory mirror and embedded snapshot differ; run network-sign sign before building.')
      this.emitFile({ type: 'asset', fileName: '.well-known/windshare-network', source })
    },
  }
}
