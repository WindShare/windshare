import assert from 'node:assert/strict';
import test from 'node:test';
import { desktopArtifacts } from './build.mjs';

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
