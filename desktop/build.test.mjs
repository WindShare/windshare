import assert from 'node:assert/strict';
import test from 'node:test';
import { desktopArtifacts } from './build.mjs';
import { identity } from './install/identity.mjs';

test('development and release installations have distinct identities', () => {
  const development = identity({ version: '0.1.0.1' });
  const release = identity({ channel: 'release', publisher: 'CN=Official Publisher', version: '1.0.0.0' });
  assert.notEqual(development.name, release.name);
  assert.notEqual(development.clsid, release.clsid);
  assert.notEqual(development.publisher, release.publisher);
  for (const options of [{ channel: 'release' }, { channel: 'release', publisher: 'CN=Official Publisher' },
    { channel: 'release', version: '1.0.0.0' }, { version: '1.0.65536.0' }, { architecture: 'ia32' }]) {
    assert.throws(() => identity(options));
  }
});

test('Explorer DLL selection follows matching Cargo artifact, excluding tests and import libraries', () => {
  const executable = '/custom/debug/windshare.exe';
  const shell = '/custom/debug/windshare_shell.dll';
  assert.equal(desktopArtifacts([
    hostBuild('/custom/host/out'), hostArtifact,
    compilerArtifact('desktop', 'windshare', 'bin', executable),
    { ...compilerArtifact('shell', 'windshare_shell', 'cdylib', null, true), filenames: ['/tests/shell.dll'] },
    { ...compilerArtifact('other', 'unrelated', 'cdylib', null), filenames: ['/wrong/shell.dll'] },
    { ...compilerArtifact('shell', 'windshare_shell', 'cdylib', null), filenames: [
      '/custom/debug/windshare_shell.dll.lib', '/custom/debug/windshare_shell.rlib', shell,
    ] },
  ]).shell, shell);
});

const hostPackage = 'path+file:///checkout/desktop/crates/windshare-host#0.1.0';
const compilerArtifact = (packageId, name, kind, executable, isTest = false) => ({
  reason: 'compiler-artifact', package_id: packageId,
  target: { name, kind: [kind] }, profile: { test: isTest }, executable,
});
const hostArtifact = compilerArtifact(hostPackage, 'windshare_host', 'lib', null);
const hostBuild = outDir => ({
  reason: 'build-script-executed', package_id: hostPackage, out_dir: outDir,
});

for (const fresh of [false, true]) {
  test(`native artifacts follow Cargo messages with fresh=${fresh}`, () => {
    const executable = '/custom-target/x86_64-pc-windows-msvc/debug/windshare.exe';
    const hostDirectory = '/independent-build/windshare-host/hash/out';
    const messages = [
      hostBuild(hostDirectory),
      { ...hostArtifact, fresh },
      compilerArtifact('desktop', 'windshare', 'bin', '/tests/windshare.exe', true),
      { ...compilerArtifact('desktop', 'windshare', 'bin', executable), fresh },
      { reason: 'build-script-executed', package_id: 'unrelated', out_dir: '/stale/out' },
    ];
    assert.deepEqual(desktopArtifacts(messages), { executable, hostDirectory });
  });
}

test('missing host build output cannot reuse another package output', () => {
  assert.throws(() => desktopArtifacts([
    hostArtifact,
    compilerArtifact('desktop', 'windshare', 'bin', '/target/debug/windshare.exe'),
    { reason: 'build-script-executed', package_id: 'unrelated', out_dir: '/stale/out' },
  ]), /did not report/u);
});

test('a test executable cannot be packaged as the desktop application', () => {
  assert.throws(() => desktopArtifacts([
    hostBuild('/build/host/out'), hostArtifact,
    compilerArtifact('desktop', 'windshare', 'bin', '/tests/windshare.exe', true),
  ]), /did not report/u);
});
