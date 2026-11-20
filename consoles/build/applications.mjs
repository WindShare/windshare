import { resolve } from 'node:path'

const RELAY_DEV_PORT = 5174
const NETWORK_DEV_PORT = 5175

// Single source of truth shared by Vite builds and embedded-bundle verification.
export const CONSOLE_APPLICATIONS = {
  relay: {
    crate: 'console',
    pages: { '/': 'index.html', '/device/complete': 'device.html' },
    port: RELAY_DEV_PORT,
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
    port: NETWORK_DEV_PORT,
    proxy: () => ({ '/v1/': { target: process.env.AUTHORITY_BACKEND || 'http://127.0.0.1:8787', changeOrigin: false } }),
  },
}

export const CONSOLE_APP_NAMES = Object.freeze(Object.keys(CONSOLE_APPLICATIONS))

export function consoleDistDirectory(workspace, appName) {
  const app = CONSOLE_APPLICATIONS[appName]
  if (!app) {
    throw new Error(`Unknown console application: ${appName}`)
  }
  return resolve(workspace, '../relay-rust', app.crate, 'console-dist')
}

export function selectConsoleApps(target = 'all') {
  if (!target || target === 'all') {
    return [...CONSOLE_APP_NAMES]
  }
  if (Object.hasOwn(CONSOLE_APPLICATIONS, target)) {
    return [target]
  }
  throw new Error(`Select one of ${CONSOLE_APP_NAMES.join(', ')}, or all; got: ${target}`)
}
