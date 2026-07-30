import { defineConfig } from 'vitest/config'

export default defineConfig({
  test: {
    include: ['test/browser-evidence/**/*.test.ts'],
    exclude: [
      'test/browser-evidence/artifact-guard-clean-bootstrap.integration.test.ts',
      'test/browser-evidence/native-directory-publisher.test.ts',
      'test/browser-evidence/native-process-group-backend.test.ts',
      'test/browser-evidence/process-runner.test.ts',
      'test/browser-evidence/windows-job-backend.test.ts',
      'test/browser-evidence/windows-job-client.test.ts',
    ],
    setupFiles: ['test/browser-evidence/contract-child-process-guard.ts'],
  },
})
