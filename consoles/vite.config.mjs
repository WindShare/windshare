import { fileURLToPath } from 'node:url'
import { resolve } from 'node:path'
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import { embeddedPages } from './build/embedded-pages.mjs'

const workspace = fileURLToPath(new URL('.', import.meta.url))
const applications = {
  relay: {
    crate: 'console',
    pages: { '/': 'index.html', '/device/complete': 'device.html' },
    port: 5174,
    proxy: () => {
      const target = process.env.RELAY_BACKEND || 'http://127.0.0.1:8484'
      // The published endpoint and verification link belong to the browser-visible origin.
      return {
        '/api/': { target, changeOrigin: false },
        '/.well-known/windshare': { target, changeOrigin: false },
        '/v2/ws': { target, changeOrigin: false, ws: true },
      }
    },
  },
  network: {
    crate: 'crates/network-authority',
    pages: { '/': 'index.html', '/console': 'index.html', '/report': 'report.html', '/device': 'device.html' },
    port: 5175,
    proxy: () => ({ '/v1/': { target: process.env.AUTHORITY_BACKEND || 'http://127.0.0.1:8787', changeOrigin: false } }),
  },
}

export default defineConfig(({ mode }) => {
  const app = applications[mode]
  if (!app) throw new Error('Select relay or network with --mode.')
  const root = resolve(workspace, mode)
  return {
    root,
    publicDir: false,
    plugins: [react(), embeddedPages(app.pages)],
    server: { host: '127.0.0.1', port: app.port, strictPort: true, fs: { allow: [workspace] }, proxy: app.proxy() },
    build: {
      outDir: resolve(workspace, '../relay-rust', app.crate, 'console-dist'),
      emptyOutDir: true,
      license: { fileName: 'third-party-licenses.txt' },
      rolldownOptions: { input: [...new Set(Object.values(app.pages))].map(file => resolve(root, file)) },
    },
  }
})
