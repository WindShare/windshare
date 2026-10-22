import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'
import { createRequire, registerHooks, stripTypeScriptTypes } from 'node:module'

const webRequire = createRequire(new URL('../../../../web/package.json', import.meta.url))
registerHooks({
  resolve(specifier, context, next) {
    try { return next(specifier, context) } catch (error) {
      if (specifier.startsWith('.') || specifier.startsWith('/')) {
        const base = fileURLToPath(new URL(specifier, context.parentURL))
        for (const file of [base + '.ts', path.join(base, 'index.ts')]) {
          if (fs.existsSync(file)) return { url: pathToFileURL(file).href, shortCircuit: true }
        }
      } else if (!specifier.startsWith('node:')) {
        return { url: pathToFileURL(webRequire.resolve(specifier)).href, shortCircuit: true }
      }
      throw error
    }
  },
  load(url, context, next) {
    if (url.endsWith('.ts')) return {
      format: 'module', shortCircuit: true,
      source: stripTypeScriptTypes(fs.readFileSync(fileURLToPath(url), 'utf8'), { mode: 'transform' }),
    }
    return next(url, context)
  },
})
