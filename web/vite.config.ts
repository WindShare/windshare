import react from '@vitejs/plugin-react'
import { defineConfig } from 'vitest/config'

const UNIT_TEST_PATTERN = 'test/**/*.test.{ts,tsx}'

export default defineConfig({
  plugins: [react()],
  test: {
    // A single worker and explicit cleanup make order or leaked globals unable to
    // turn a passing unit suite into a runner-dependent result.
    include: [UNIT_TEST_PATTERN],
    environment: 'node',
    fileParallelism: false,
    maxWorkers: 1,
    clearMocks: true,
    restoreMocks: true,
    unstubEnvs: true,
    unstubGlobals: true,
  },
})
