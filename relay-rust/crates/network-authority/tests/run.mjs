import { execFileSync } from 'node:child_process';
import { readdirSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

const cwd = fileURLToPath(new URL('../', import.meta.url));
const build = 'build/test-release';
execFileSync('worker-build', ['--dev', '--out-dir', build, '--locked', '--features', 'test-release'], { cwd, stdio: 'inherit' });
const suites = readdirSync(new URL('./', import.meta.url)).filter(name => name.endsWith('.test.mjs') || name === 'worker.mjs');
execFileSync(process.execPath, ['--test', ...suites.map(name => 'tests/' + name)], {
  cwd, stdio: 'inherit', env: { ...process.env, AUTHORITY_TEST_BUILD: build },
});
