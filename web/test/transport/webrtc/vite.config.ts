import react from '@vitejs/plugin-react'
import { defineConfig } from 'vite'

const PION_INTEROP_TARGET = 'http://127.0.0.1:17849'

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
