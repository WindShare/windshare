import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import type { Plugin } from 'vite'

const DEVELOPMENT_NETWORK_MODE = 'windshare-development'

export function networkAssets(mode: string): Plugin {
  const directory = new URL(mode === DEVELOPMENT_NETWORK_MODE
    ? '../../deploy/network/development/' : '../../deploy/network/', import.meta.url)
  const releasePath = fileURLToPath(new URL('release.json', directory))
  return {
    name: 'windshare-network-export',
    // Resolve the browser trust anchor and mirror together so a build cannot
    // silently publish a directory belonging to another network identity.
    config: () => ({ resolve: { alias: { '@windshare/network-release': releasePath } } }),
    generateBundle() {
      const release = JSON.parse(readFileSync(releasePath, 'utf8')) as { snapshot: unknown }
      if (release.snapshot === null) return
      const source = readFileSync(new URL('windshare-network.json', directory), 'utf8')
      if (JSON.stringify(JSON.parse(source)) !== JSON.stringify(release.snapshot)) throw new Error('Directory mirror and embedded snapshot differ; run network-sign sign before building.')
      this.emitFile({ type: 'asset', fileName: '.well-known/windshare-network', source })
    },
  }
}
