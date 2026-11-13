import { copyFile, mkdir, mkdtemp, readFile, writeFile } from 'node:fs/promises';
import { basename, dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { spawnSync } from 'node:child_process';
import { buildDesktop } from './build.mjs';

const desktop = dirname(fileURLToPath(import.meta.url));
const root = dirname(desktop);
const profile = process.argv[2] ?? 'release';
if (!['debug', 'release'].includes(profile)) throw new Error('Expected debug or release profile');
const cargo = process.argv.length > 3 ? process.argv.slice(3) : ['rustup', 'run', 'stable', 'cargo'];
const artifacts = await buildDesktop([
  ...cargo, 'build', '--manifest-path', join(desktop, 'Cargo.toml'), '--locked',
  '--jobs', process.env.CARGO_BUILD_JOBS ?? '2', ...(profile === 'release' ? ['--release'] : []),
]);
const executable = basename(artifacts.executable);
const output = join(root, 'dist', 'desktop');
await mkdir(output, { recursive: true });
const staging = await mkdtemp(join(output, 'windshare-'));
for (const source of [artifacts.executable, artifacts.library]) {
  await copyFile(source, join(staging, basename(source)));
}
await copyFile(join(root, 'LICENSE'), join(staging, 'LICENSE'));
await writeFile(join(staging, 'README.txt'),
  'WindShare desktop: keep the executable and Go host library together.\n' +
  'Run windshare to select real files and folders, or drop a selection into the window.\n' +
  'Uses saved wind relay/frontend settings, with in-app manual setup when needed.\n' +
  'Closing the window stops and joins shares.\n');
const check = spawnSync(join(staging, executable), ['--host-check'], { cwd: staging, timeout: 60_000 });
if (check.error || check.status !== 0) throw check.error ?? new Error('Packaged host check failed');
const archive = join(output, `windshare-${process.platform}-${process.arch}-${profile}.tar.gz`);
const tar = spawnSync('tar', ['-czf', archive, '-C', staging, '.'], { stdio: 'inherit' });
if (tar.error || tar.status !== 0) throw tar.error ?? new Error('Desktop archive failed');
const { createHash } = await import('node:crypto');
const hash = createHash('sha256').update(await readFile(archive)).digest('hex');
await writeFile(archive + '.sha256', hash + '\n');
console.log(JSON.stringify({ directory: staging, archive, sha256: hash }));
