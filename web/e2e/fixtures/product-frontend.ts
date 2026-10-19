import { once } from 'node:events'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { createServer } from 'vite'

const REPOSITORY_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '../../..')

export async function startProductFrontend(output: string) {
  const vite = await createServer({
    root: join(REPOSITORY_ROOT, 'web'), cacheDir: join(output, 'vite-cache'), logLevel: 'error',
    server: { host: '127.0.0.1', port: 0, strictPort: true, hmr: false, watch: null },
  })
  try {
    const listener = vite.httpServer
    if (!listener) throw new Error('Vite listener unavailable')
    const listening = once(listener, 'listening')
    listener.listen(0, '127.0.0.1')
    await listening
    const address = listener.address()
    if (!address || typeof address === 'string') throw new Error('Vite address unavailable')
    return { vite, url: `http://127.0.0.1:${address.port}` }
  } catch (error) {
    await vite.close()
    throw error
  }
}
