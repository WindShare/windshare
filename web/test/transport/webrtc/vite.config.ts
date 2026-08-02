import react from '@vitejs/plugin-react'
import { defineConfig } from 'vite'

const PION_INTEROP_TARGET = pionInteropTarget()

export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      '/d2-pion': {
        target: PION_INTEROP_TARGET,
        changeOrigin: false,
        rewrite: (path) => path.replace(/^\/d2-pion/u, ''),
      },
    },
  },
})

function pionInteropTarget(): string {
  const encoded = process.env.WINDSHARE_PION_HTTP_ADDRESS
  if (encoded === undefined) {
    throw new Error('WINDSHARE_PION_HTTP_ADDRESS must come from the owned Pion server fixture')
  }
  const parsed = new URL(`http://${encoded}`)
  const port = Number(parsed.port)
  if (
    parsed.hostname !== '127.0.0.1' || !Number.isSafeInteger(port) ||
    port < 1 || port > 65_535 || parsed.pathname !== '/'
  ) throw new Error('WINDSHARE_PION_HTTP_ADDRESS must identify an owned loopback listener')
  return parsed.origin
}
