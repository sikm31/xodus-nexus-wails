import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import { audioProbeBuild, buildUserAudioProbe } from './build-user-audio-probe.mjs';
const options = { source: '/tmp/source', build: '/tmp/build with spaces', tests: '/tmp/tests', project: '/tmp/project' };
test('audio probe uses pinned Wine UCRT and actual helper with test-only COM source', () => {
  const probe = audioProbeBuild(options);
  assert.equal(probe.command, '/tmp/build with spaces/tools/winegcc/winegcc');
  assert.ok(probe.args.includes('-D_UCRT'));
  assert.ok(probe.args.includes('/tmp/source/dlls/xgameruntime'));
  assert.ok(probe.args.includes('/tmp/source/include/msvcrt'));
  const source = fs.readFileSync(new URL('./user-audio-probe.c', import.meta.url), 'utf8');
  for (const text of ['#include "xuser_audio.h"', '#define CoCreateInstance create_enumerator', '#undef CoCreateInstance', 'native_abi()', 'startup_timeout()']) assert.ok(source.includes(text));
  assert.ok(!source.includes('SetDefaultEndpoint'));
});
test('audio probe rejects legacy or missing CRT imports', () => {
  const calls = [];
  buildUserAudioProbe(options, (...args) => { calls.push(args); return 'DLL Name: ucrtbase.dll'; });
  assert.equal(calls.length, 2);
  for (const headers of ['', 'DLL Name: msvcrt.dll', 'DLL Name: ucrtbase.dll\nDLL Name: msvcr120.dll'])
    assert.throws(() => buildUserAudioProbe(options, () => headers), /Wine UCRT/);
});
