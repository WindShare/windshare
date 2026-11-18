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
const MAX_FUNCTION_LINES = 150
const MAX_TSX_FUNCTION_LINES = 250
const MAX_TEST_FUNCTION_LINES = 350

export default defineConfig([
  globalIgnores([
    '**/.tmp/**',
    'dist',
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
      'sonarjs/cognitive-complexity': ['error', DEFAULT_COGNITIVE_COMPLEXITY],
      'max-lines-per-function': [
        'error',
        { max: MAX_FUNCTION_LINES, skipBlankLines: true, skipComments: true },
      ],
    },
  },
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
    files: ['relay/**/*.{ts,tsx}', 'network/**/*.{ts,tsx}', 'ui/**/*.{ts,tsx}'],
    extends: [reactHooks.configs.flat.recommended, reactRefresh.configs.vite],
    languageOptions: {
      globals: globals.browser,
    },
  },
  {
    files: ['ui/**/*.{ts,tsx}'],
    rules: {
      'no-restricted-imports': ['error', {
        patterns: [{
          group: ['../relay/**', '**/relay/**', '../network/**', '**/network/**'],
          message: 'Shared console UI primitives must stay independent of relay and network applications.',
        }],
      }],
    },
  },
  {
    files: ['relay/**/*.{ts,tsx}'],
    rules: {
      'no-restricted-imports': ['error', {
        patterns: [{
          group: ['../network/**', '**/network/**'],
          message: 'Relay console must not import from the network console.',
        }],
      }],
    },
  },
  {
    files: ['network/**/*.{ts,tsx}'],
    rules: {
      'no-restricted-imports': ['error', {
        patterns: [{
          group: ['../relay/**', '**/relay/**'],
          message: 'Network console must not import from the relay console.',
        }],
      }],
    },
  },
  {
    files: ['*.config.ts', 'build/**/*.{ts,tsx}', 'test/**/*.{ts,tsx}'],
    languageOptions: {
      globals: globals.node,
    },
  },
  {
    files: ['**/*.ts'],
    rules: {
      'max-lines-per-function': [
        'error',
        { max: MAX_FUNCTION_LINES, skipBlankLines: true, skipComments: true },
      ],
    },
  },
  {
    files: ['**/*.tsx'],
    rules: {
      'max-lines-per-function': [
        'error',
        { max: MAX_TSX_FUNCTION_LINES, skipBlankLines: true, skipComments: true },
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
        { max: MAX_TEST_FUNCTION_LINES, skipBlankLines: true, skipComments: true },
      ],
      'sonarjs/cognitive-complexity': ['error', TEST_COGNITIVE_COMPLEXITY],
    },
  },
])
