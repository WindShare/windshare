import js from '@eslint/js'
import globals from 'globals'
import reactHooks from 'eslint-plugin-react-hooks'
import reactRefresh from 'eslint-plugin-react-refresh'
import sonarjs from 'eslint-plugin-sonarjs'
import tseslint from 'typescript-eslint'
import { defineConfig, globalIgnores } from 'eslint/config'

const DEFAULT_COGNITIVE_COMPLEXITY = 15
const TSX_COGNITIVE_COMPLEXITY = 18
const TEST_COGNITIVE_COMPLEXITY = 25

export default defineConfig([
  globalIgnores([
    'dist',
    'coverage',
    'test-results',
    'playwright-report',
    'blob-report',
  ]),
  {
    files: ['**/*.{ts,tsx}'],
    extends: [
      js.configs.recommended,
      tseslint.configs.recommended,
      sonarjs.configs.recommended,
    ],
    languageOptions: {
      ecmaVersion: 2023,
    },
    rules: {
      'sonarjs/cognitive-complexity': ['error', DEFAULT_COGNITIVE_COMPLEXITY],
    },
  },
  {
    files: ['src/**/*.{ts,tsx}'],
    extends: [reactHooks.configs.flat.recommended, reactRefresh.configs.vite],
    languageOptions: {
      globals: globals.browser,
    },
  },
  {
    // Config files and Vitest fixtures execute in Node, even when they import
    // browser-facing modules for type or vector conformance checks.
    files: ['*.config.ts', 'test/**/*.{ts,tsx}', 'e2e/**/*.{ts,tsx}'],
    languageOptions: {
      globals: globals.node,
    },
  },
  {
    // Playwright callbacks may reference browser globals while their test module
    // itself still executes in Node.
    files: [
      'test/browser/**/*.spec.ts',
      'e2e/**/*.spec.ts',
      'e2e/**/*.probe.ts',
    ],
    languageOptions: {
      globals: { ...globals.node, ...globals.browser },
    },
  },
  {
    files: ['**/*.ts'],
    rules: {
      'max-lines-per-function': [
        'error',
        { max: 150, skipBlankLines: true, skipComments: true },
      ],
    },
  },
  {
    files: ['**/*.tsx'],
    rules: {
      'max-lines-per-function': [
        'error',
        { max: 250, skipBlankLines: true, skipComments: true },
      ],
      // JSX adds control-flow surface that would otherwise force components into
      // fragments with no independent semantic responsibility.
      'sonarjs/cognitive-complexity': ['error', TSX_COGNITIVE_COMPLEXITY],
    },
  },
  {
    files: ['**/*.{test,spec}.{ts,tsx}', 'test/**/*.{ts,tsx}'],
    rules: {
      'max-lines-per-function': [
        'error',
        { max: 350, skipBlankLines: true, skipComments: true },
      ],
      'sonarjs/cognitive-complexity': ['error', TEST_COGNITIVE_COMPLEXITY],
    },
  },
])
