import { spawnSync } from 'node:child_process';
import { copyFile } from 'node:fs/promises';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const MAX_CARGO_OUTPUT_BYTES = 64 * 1024 * 1024;
const hostLibrary = {
  win32: 'windshare_host.dll', darwin: 'libwindshare_host.dylib', linux: 'libwindshare_host.so',
}[process.platform];

export function desktopArtifacts(messages) {
  const artifacts = messages.filter(message =>
    message.reason === 'compiler-artifact' && !message.profile.test);
  const executable = artifacts.find(message =>
    message.target.name === 'windshare' && message.target.kind.includes('bin'))?.executable;
  const host = artifacts.find(message =>
    message.target.name === 'windshare_host' && message.target.kind.includes('lib'));
  const output = messages.find(message =>
    message.reason === 'build-script-executed' && message.package_id === host?.package_id)?.out_dir;
  if (!executable || !output) throw new Error('Cargo did not report the desktop and native host artifacts');
  return { executable, hostDirectory: output };
}

export async function buildDesktop(command) {
  if (!hostLibrary) throw new Error('Unsupported desktop platform');
  if (!command.length) throw new Error('Expected a Cargo build command');
  const build = spawnSync(command[0], [...command.slice(1), '--message-format=json-render-diagnostics'], {
    stdio: ['inherit', 'pipe', 'inherit'], encoding: 'utf8', maxBuffer: MAX_CARGO_OUTPUT_BYTES,
  });
  if (build.error) throw build.error;
  const messages = build.stdout.split(/\r?\n/u).filter(Boolean).map(line => JSON.parse(line));
  for (const message of messages) {
    if (message.reason === 'compiler-message' && message.message.rendered) {
      process.stderr.write(message.message.rendered);
    }
  }
  if (build.status !== 0) throw new Error(`Cargo build failed: ${build.status ?? build.signal}`);
  const { executable, hostDirectory } = desktopArtifacts(messages);
  // Cargo's intermediate layout is private; its messages bind the library from
  // this build to the executable, including fresh builds and custom target dirs.
  const library = join(dirname(executable), hostLibrary);
  await copyFile(join(hostDirectory, hostLibrary), library);
  console.log(JSON.stringify({ event: 'desktop.host_staged', executable, library, hostDirectory }));
  return { executable, library };
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const args = process.argv.slice(2);
  const separator = args.indexOf('--');
  const command = separator < 0 ? args : args.slice(0, separator);
  const { executable } = await buildDesktop(command);
  if (separator >= 0) {
    const run = spawnSync(executable, args.slice(separator + 1), { stdio: 'inherit' });
    if (run.error) throw run.error;
    process.exitCode = run.status ?? 1;
  }
}
