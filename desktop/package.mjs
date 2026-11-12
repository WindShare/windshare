import { copyFile, mkdir, mkdtemp, readFile, writeFile } from 'node:fs/promises';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { spawnSync } from 'node:child_process';

const desktop = dirname(fileURLToPath(import.meta.url));
const root = dirname(desktop);
const profile = process.argv[2] ?? 'release';
if (!['debug', 'release'].includes(profile)) throw new Error('Expected debug or release profile');
const executable = process.platform === 'win32' ? 'windshare.exe' : 'windshare';
const library = {
  win32: 'windshare_host.dll', darwin: 'libwindshare_host.dylib', linux: 'libwindshare_host.so',
}[process.platform];
if (!library) throw new Error('Unsupported desktop platform');
const output = join(root, 'dist', 'desktop');
await mkdir(output, { recursive: true });
const staging = await mkdtemp(join(output, 'windshare-'));
const target = process.env.CARGO_TARGET_DIR
  ? resolve(process.env.CARGO_TARGET_DIR) : join(desktop, 'target');
for (const name of [executable, library, 'windshare_host.h']) {
  await copyFile(join(target, profile, name), join(staging, name));
}
await copyFile(join(root, 'LICENSE'), join(staging, 'LICENSE'));
await writeFile(join(staging, 'README.txt'),
  'WindShare desktop: keep the executable and Go host library together.\n' +
  'Run windshare to select real files and folders, or drop a selection into the window.\n' +
  'Uses saved wind relay/frontend configuration. Closing the window stops and joins shares.\n');
const check = spawnSync(join(staging, executable), ['--host-check'], { cwd: staging, timeout: 60_000 });
if (check.error || check.status !== 0) throw check.error ?? new Error('Packaged host check failed');
const archive = join(output, `windshare-${process.platform}-${process.arch}-${profile}.tar.gz`);
const tar = spawnSync('tar', ['-czf', archive, '-C', staging, '.'], { stdio: 'inherit' });
if (tar.error || tar.status !== 0) throw tar.error ?? new Error('Desktop archive failed');
const { createHash } = await import('node:crypto');
const hash = createHash('sha256').update(await readFile(archive)).digest('hex');
await writeFile(archive + '.sha256', hash + '\n');
console.log(JSON.stringify({ directory: staging, archive, sha256: hash }));
