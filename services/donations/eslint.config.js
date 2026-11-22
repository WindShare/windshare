import js from '@eslint/js'
import globals from 'globals'
import sonarjs from 'eslint-plugin-sonarjs'
import tseslint from 'typescript-eslint'
import { defineConfig, globalIgnores } from 'eslint/config'

const DEFAULT_COGNITIVE_COMPLEXITY = 15
const TEST_COGNITIVE_COMPLEXITY = 25
const DEFAULT_CYCLOMATIC_COMPLEXITY = 15
const TEST_CYCLOMATIC_COMPLEXITY = 25
const MAX_NESTING_DEPTH = 4
const TEST_MAX_NESTING_DEPTH = 5
const MAX_FUNCTION_PARAMS = 5
const MAX_FUNCTION_LINES = 150
const MAX_TEST_FUNCTION_LINES = 350

export default defineConfig([
  globalIgnores([
    '**/.tmp/**',
    '.wrangler',
    'dist',
    'coverage',
    'src/admin/console-dist',
  ]),
  {
    files: ['**/*.{js,mjs}'],
    extends: [
      js.configs.recommended,
      sonarjs.configs.recommended,
    ],
    languageOptions: {
      ecmaVersion: 2023,
      globals: globals.node,
    },
    rules: {
      'complexity': ['error', DEFAULT_CYCLOMATIC_COMPLEXITY],
      'max-depth': ['error', MAX_NESTING_DEPTH],
      'max-lines-per-function': [
        'error',
        { max: MAX_FUNCTION_LINES, skipBlankLines: true, skipComments: true },
      ],
      'sonarjs/cognitive-complexity': ['error', DEFAULT_COGNITIVE_COMPLEXITY],
    },
  },
  {
    files: ['**/*.ts'],
    extends: [
      js.configs.recommended,
      tseslint.configs.recommended,
      sonarjs.configs.recommended,
    ],
    languageOptions: {
      ecmaVersion: 2023,
    },
    rules: {
      '@typescript-eslint/consistent-type-imports': ['error', { prefer: 'type-imports' }],
      'complexity': ['error', DEFAULT_CYCLOMATIC_COMPLEXITY],
      'eqeqeq': ['error', 'always'],
      'max-depth': ['error', MAX_NESTING_DEPTH],
      'max-lines-per-function': [
        'error',
        { max: MAX_FUNCTION_LINES, skipBlankLines: true, skipComments: true },
      ],
      'max-params': ['error', MAX_FUNCTION_PARAMS],
      'sonarjs/cognitive-complexity': ['error', DEFAULT_COGNITIVE_COMPLEXITY],
    },
  },
  {
    // Pure domain, sanitization, verification, and env helpers must stay testable
    // in plain Node without Durable Object or edge entrypoint runtime coupling.
    files: ['src/env.ts', 'src/model.ts', 'src/sanitize.ts', 'src/verify.ts'],
    rules: {
      'no-restricted-imports': ['error', {
        patterns: [{
          group: ['cloudflare:*', './wall-do*', './worker*', './admin/**'],
          message: 'Pure domain modules must not depend on Cloudflare Workers runtime, Durable Objects, or HTTP routes.',
        }],
      }],
    },
  },
  {
    // Single-threaded Durable Object owns SQLite state only; outbound provider
    // HTTP calls and edge routing belong in the stateless Worker entrypoint.
    files: ['src/wall-do.ts'],
    rules: {
      'no-restricted-imports': ['error', {
        patterns: [{
          group: ['./worker*', './admin/**'],
          message: 'DonationWall Durable Object must stay decoupled from HTTP routing and admin asset serving.',
        }],
      }],
    },
  },
  {
    files: ['admin-ui/src/**/*.ts'],
    languageOptions: {
      globals: globals.browser,
    },
    rules: {
      'no-restricted-imports': ['error', {
        patterns: [{
          group: ['cloudflare:*', 'node:*', '../../src/!(model)*'],
          message: 'Browser admin UI may only import pure domain types from src/model.ts.',
        }],
      }],
    },
  },
  {
    files: ['vitest.config.ts', 'test/**/*.ts', '**/*.{test,spec}.ts'],
    languageOptions: {
      globals: globals.node,
    },
    rules: {
      'complexity': ['error', TEST_CYCLOMATIC_COMPLEXITY],
      'max-depth': ['error', TEST_MAX_NESTING_DEPTH],
      'max-lines-per-function': [
        'error',
        { max: MAX_TEST_FUNCTION_LINES, skipBlankLines: true, skipComments: true },
      ],
      'max-params': 'off',
      'sonarjs/cognitive-complexity': ['error', TEST_COGNITIVE_COMPLEXITY],
    },
  },
])
