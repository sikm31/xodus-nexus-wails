import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import { buildSaveSyncProbe, saveSyncProbeBuild } from './build-gamesave-sync-probe.mjs';

const options = { source: '/tmp/source', build: '/tmp/build with spaces', tests: '/tmp/tests', project: '/tmp/project' };
test('save sync probe uses actual pinned source and matching Wine UCRT', () => {
  const probe = saveSyncProbeBuild(options);
  assert.equal(probe.command, '/tmp/build with spaces/tools/winegcc/winegcc');
  for (const argument of ['-D_UCRT', '/tmp/source/dlls/xgameruntime', '/tmp/source/include/msvcrt', '-lwinecrt0', '-lucrtbase', '-lbcrypt']) assert.ok(probe.args.includes(argument));
  assert.equal(probe.output, '/tmp/tests/gamesave-sync-probe.exe');
  const fixture = fs.readFileSync(new URL('./gamesave-sync-probe.c', import.meta.url), 'utf8');
  assert.ok(fixture.includes('#include "xgamesave.c"'));
  assert.ok(fixture.includes('https://fixture.invalid'));
  assert.ok(fixture.includes('#define MoveFileExW test_move'));
});
test('save probe rejects a mixed CRT instead of accepting an untested binary', () => {
  const calls = [];
  buildSaveSyncProbe(options, (...args) => { calls.push(args); return 'DLL Name: ucrtbase.dll'; });
  assert.equal(calls.length, 4);
  assert.ok(calls[2][1].includes('/tmp/project/scripts/runtime-tests/gamesave-transfer-probe.c'));
  for (const headers of ['', 'DLL Name: msvcrt.dll', 'DLL Name: ucrtbase.dll\nDLL Name: msvcr120.dll'])
    assert.throws(() => buildSaveSyncProbe(options, () => headers), /Wine UCRT/);
});
