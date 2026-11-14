import { spawnSync } from 'node:child_process';
import { copyFile, mkdir, writeFile } from 'node:fs/promises';
import { createRequire } from 'node:module';
import { basename, join } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { buildDesktop } from '../../desktop/build.mjs';

const root = fileURLToPath(new URL('../../', import.meta.url));
const web = join(root, 'web');
const output = join(root, '.windshare-dev', 'zero-config');
const network = join(root, 'deploy', 'network', 'development');
const desktop = join(output, 'desktop');
const webRequire = createRequire(join(web, 'package.json'));
const nativeEnv = {
  ...process.env, GOWORK: 'off', GOTOOLCHAIN: 'local',
  // Apply the same compile-time identity to the CLI and Cargo-owned Go library.
  GOFLAGS: [process.env.GOFLAGS, '-tags=windshare_development'].filter(Boolean).join(' '),
};

function run(command, args, options = {}) {
  const result = spawnSync(command, args, { cwd: root, env: nativeEnv, stdio: 'inherit', ...options });
  if (result.error) throw result.error;
  if (result.status !== 0) throw new Error(`${command} failed: ${result.status ?? result.signal}`);
}

console.log(JSON.stringify({ event: 'development.build_started', network, output }));
run('go', ['run', './cmd/network-sign', 'verify',
  '--release', join(network, 'release.json'),
  '--mirror', join(network, 'windshare-network.json'),
  '--history', join(network, 'history')]);
await mkdir(desktop, { recursive: true });
const config = join(output, 'config.json');
try {
  // Rebuilds retain settings and adjacent trust state, including anti-rollback history.
  await writeFile(config, '{"profiles":[]}\n', { flag: 'wx' });
} catch (error) {
  if (error.code !== 'EEXIST') throw error;
}
run('go', ['build', '-trimpath', '-o', join(output, process.platform === 'win32' ? 'wind.exe' : 'wind'), './cmd/wind']);
const artifacts = await buildDesktop([
  'rustup', 'run', 'stable', 'cargo', 'build', '--manifest-path', join(root, 'desktop', 'Cargo.toml'),
  '--target-dir', join(output, 'desktop-target'), '--locked', '--jobs', process.env.CARGO_BUILD_JOBS ?? '2',
], { cwd: root, env: nativeEnv });
for (const source of [artifacts.executable, artifacts.library]) {
  await copyFile(source, join(desktop, basename(source)));
}
run(join(desktop, basename(artifacts.executable)), ['--host-check'], {
  cwd: desktop, env: { ...nativeEnv, WINDSHARE_CONFIG: config }, timeout: 60_000,
});
run(process.execPath, [webRequire.resolve('typescript/bin/tsc'), '-b'], { cwd: web });
const { build } = await import(pathToFileURL(webRequire.resolve('vite')).href);
await build({
  root: web, mode: 'windshare-development',
  build: { outDir: join(output, 'web'), emptyOutDir: true },
});
console.log(JSON.stringify({ event: 'development.build_completed', output, config }));
