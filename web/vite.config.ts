import { readFileSync } from 'node:fs'
import react from '@vitejs/plugin-react'
import { defineConfig } from 'vitest/config'

const UNIT_TEST_PATTERN = 'test/**/*.test.{ts,tsx}'
const MAX_UNIT_TEST_WORKERS = 2
const BUILD_REVISION_PATTERN = /^[0-9a-f]{7,64}$/
const LOCAL_DEVELOPMENT_MODE = 'windshare-local'
const LOCAL_RELAY_PROXY_TARGET = 'http://127.0.0.1:8484'
const RELAY_WEBSOCKET_PATH = '/v2/ws'
const RELAY_HEALTH_PATH = '/healthz'

interface PackageMetadata {
  readonly version: string
}

const packageMetadata = JSON.parse(
  readFileSync(new URL('./package.json', import.meta.url), 'utf8'),
) as PackageMetadata

export default defineConfig(({ command, mode }) => {
  const revision = process.env.WIND_BUILD_REVISION
  const buildMode = resolveBuildMode(command, mode)

  return {
    define: {
      __WIND_BUILD_VERSION__: JSON.stringify(packageMetadata.version),
      __WIND_BUILD_REVISION__: revision !== undefined &&
          BUILD_REVISION_PATTERN.test(revision)
        ? JSON.stringify(revision)
        : 'undefined',
      __WIND_BUILD_MODE__: JSON.stringify(buildMode),
    },
    plugins: [react()],
    ...(mode === LOCAL_DEVELOPMENT_MODE
      ? {
          server: {
            // The browser sees one origin while the production relay remains an
            // independently debuggable process with its own lifecycle and logs.
            proxy: {
              [RELAY_WEBSOCKET_PATH]: {
                target: LOCAL_RELAY_PROXY_TARGET,
                ws: true,
              },
              [RELAY_HEALTH_PATH]: {
                target: LOCAL_RELAY_PROXY_TARGET,
              },
            },
          },
        }
      : {}),
    test: {
      // File isolation contains globals; two workers overlap startup and analysis
      // while leaving resources for the Go lanes in ci-parallel.
      include: [UNIT_TEST_PATTERN],
      environment: 'node',
      isolate: true,
      fileParallelism: true,
      maxWorkers: MAX_UNIT_TEST_WORKERS,
      clearMocks: true,
      restoreMocks: true,
      unstubEnvs: true,
      unstubGlobals: true,
    },
  }
})

function resolveBuildMode(
  command: 'build' | 'serve',
  mode: string,
): 'development' | 'production' | 'test' {
  if (mode === 'test') return 'test'
  return command === 'build' ? 'production' : 'development'
}
