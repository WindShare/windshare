import { isAbsolute, resolve } from 'node:path'

export const GENERATED_SEMANTIC_FILENAME = 'final-semantic-reducer.js'
const GENERATED_SEMANTIC_DEBUG_INFORMATION_MODE = 'none'

export function createGeneratedSemanticBuildConfig({
  webRoot,
  semanticEntry,
  isolatedCacheRoot,
}) {
  requireAbsolutePath(webRoot, 'generated semantic web root')
  requireAbsolutePath(semanticEntry, 'generated semantic entry')
  requireAbsolutePath(isolatedCacheRoot, 'generated semantic cache root')
  return {
    root: webRoot,
    configFile: false,
    envDir: false,
    mode: 'production',
    publicDir: false,
    cacheDir: isolatedCacheRoot,
    clearScreen: false,
    logLevel: 'silent',
    build: {
      target: 'es2023',
      ssr: semanticEntry,
      write: false,
      minify: false,
      sourcemap: false,
      rolldownOptions: {
        tsconfig: false,
        // Rolldown's default embeds host-specific module paths in source-region
        // comments, so determinism requires disabling that debug-only surface.
        experimental: {
          attachDebugInfo: GENERATED_SEMANTIC_DEBUG_INFORMATION_MODE,
        },
        output: {
          format: 'es',
          entryFileNames: GENERATED_SEMANTIC_FILENAME,
        },
      },
    },
  }
}

function requireAbsolutePath(value, label) {
  if (typeof value !== 'string' || !isAbsolute(value) || resolve(value) !== value) {
    throw new Error(`${label} must be absolute and canonical`)
  }
}
