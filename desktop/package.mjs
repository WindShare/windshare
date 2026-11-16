import { copyFile, mkdir, mkdtemp, readFile, writeFile } from 'node:fs/promises';
import { basename, dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { spawnSync } from 'node:child_process';
import { buildDesktop } from './build.mjs';
import { identity, writeIdentity } from './install/identity.mjs';

const desktop = dirname(fileURLToPath(import.meta.url));
const root = dirname(desktop);
const profile = process.argv[2] ?? 'release';
if (!['debug', 'release'].includes(profile)) throw new Error('Expected debug or release profile');
const cargo = process.argv.length > 3 ? process.argv.slice(3) : ['rustup', 'run', 'stable', 'cargo'];
const configuration = identity({
  channel: process.env.WINDSHARE_INSTALL_CHANNEL ?? 'development',
  publisher: process.env.WINDSHARE_PACKAGE_PUBLISHER,
  version: process.env.WINDSHARE_PACKAGE_VERSION,
  architecture: process.arch === 'arm64' ? 'arm64' : 'x64',
});
const artifacts = await buildDesktop([
  ...cargo, 'build', '--workspace', '--manifest-path', join(desktop, 'Cargo.toml'), '--locked',
  '--jobs', process.env.CARGO_BUILD_JOBS ?? '2', ...(profile === 'release' ? ['--release'] : []),
]);
const executable = basename(artifacts.executable);
const output = join(root, 'dist', 'desktop');
await mkdir(output, { recursive: true });
const staging = await mkdtemp(join(output, 'windshare-'));
for (const source of [artifacts.executable, artifacts.library, ...(artifacts.shell ? [artifacts.shell] : [])]) {
  await copyFile(source, join(staging, basename(source)));
}
await copyFile(join(root, 'LICENSE'), join(staging, 'LICENSE'));
await writeFile(join(staging, 'README.txt'),
  'WindShare desktop: keep the executable and Go host library together.\n' +
  'Run windshare to select real files and folders, or drop a selection into the window.\n' +
  'Uses saved wind relay/frontend settings, with in-app manual setup when needed.\n' +
  'Closing asks before stopping unfinished shares, then waits for shutdown.\n' +
  'Windows 11: powershell.exe -File install.ps1 (requires a Windows-trusted signing certificate).\n' +
  'Development package: powershell.exe -File install.ps1 -DevelopmentRegistration (Developer Mode required).\n' +
  'Remove with the installed uninstall.ps1. Updates/removal wait for app and shell release.\n');
if (process.platform === 'win32') {
  if (!artifacts.shell) throw new Error('Cargo did not report the Explorer DLL');
  const identityDirectory = join(staging, 'identity');
  await writeIdentity(identityDirectory, configuration);
  const assetDirectory = join(staging, 'Assets');
  const assets = spawnSync(cargo[0], [...cargo.slice(1), 'run', '--manifest-path', join(desktop, 'Cargo.toml'),
    '--locked', '-p', 'windshare-shell', '--example', 'package_assets', '--', assetDirectory], { stdio: 'inherit' });
  if (assets.error || assets.status !== 0) throw assets.error ?? new Error('Icon rendering failed');
  await copyFile(join(assetDirectory, 'windshare.ico'), join(staging, 'windshare.ico'));
  for (const script of ['install.ps1', 'uninstall.ps1', 'lifetime.ps1']) {
    await copyFile(join(desktop, 'install', script), join(staging, script));
  }
  const identityBuild = spawnSync('powershell.exe', ['-NoProfile', '-ExecutionPolicy', 'Bypass',
    '-File', join(desktop, 'install', 'build-identity.ps1'), '-Directory', staging], { stdio: 'inherit' });
  if (identityBuild.error || identityBuild.status !== 0) throw identityBuild.error ?? new Error('Identity package build failed');
}
const check = spawnSync(join(staging, executable), ['--host-check'], { cwd: staging, timeout: 60_000 });
if (check.error || check.status !== 0) throw check.error ?? new Error('Packaged host check failed');
const archive = join(output, `windshare-${process.platform}-${process.arch}-${profile}.tar.gz`);
const tar = spawnSync('tar', ['-czf', archive, '-C', staging, '.'], { stdio: 'inherit' });
if (tar.error || tar.status !== 0) throw tar.error ?? new Error('Desktop archive failed');
const { createHash } = await import('node:crypto');
const hash = createHash('sha256').update(await readFile(archive)).digest('hex');
await writeFile(archive + '.sha256', hash + '\n');
console.log(JSON.stringify({ directory: staging, archive, sha256: hash }));
