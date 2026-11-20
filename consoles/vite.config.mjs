import { fileURLToPath } from 'node:url'
import { resolve } from 'node:path'
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import { CONSOLE_APPLICATIONS, consoleDistDirectory } from './build/applications.mjs'
import { embeddedPages } from './build/embedded-pages.mjs'

const workspace = fileURLToPath(new URL('.', import.meta.url))

export default defineConfig(({ mode }) => {
  const app = CONSOLE_APPLICATIONS[mode]
  if (!app) throw new Error('Select relay or network with --mode.')
  const root = resolve(workspace, mode)
  return {
    root,
    publicDir: false,
    plugins: [react(), embeddedPages(app.pages)],
    server: { host: '127.0.0.1', port: app.port, strictPort: true, fs: { allow: [workspace] }, proxy: app.proxy() },
    build: {
      outDir: consoleDistDirectory(workspace, mode),
      emptyOutDir: true,
      license: { fileName: 'third-party-licenses.txt' },
      rolldownOptions: { input: [...new Set(Object.values(app.pages))].map(file => resolve(root, file)) },
    },
  }
})
