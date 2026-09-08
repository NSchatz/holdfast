#!/usr/bin/env node
// A15 final: run the WHOLE cmd/holdfast package (the only package that ever failed)
// on both the S0057 branch and origin/main, with TMPDIR on durable local storage.
'use strict';
const fs = require('fs');
const cp = require('child_process');
const BRANCH = '/workspace/.worktrees/S0057-holdfast-pinning-1/holdfast';
const MAIN = '/tmp/hf-main';
const TMP = '/workspace/.hf-a15-tmp';
const sh = (c, a, o = {}) => cp.spawnSync(c, a, { encoding: 'utf8', maxBuffer: 128 * 1024 * 1024, ...o });
fs.mkdirSync(TMP, { recursive: true });

for (const [label, dir] of [['S0057 branch', BRANCH], ['origin/main', MAIN]]) {
  const r = sh('go', ['test', '-race', '-covermode=atomic', '-count=1', './cmd/holdfast'],
    { cwd: dir, timeout: 1500000, env: { ...process.env, TMPDIR: TMP } });
  const out = ((r.stdout || '') + (r.stderr || '')).split('\n')
    .filter(l => !l.startsWith('time=')).filter(l => l.trim());
  console.log('='.repeat(74));
  console.log(`${label}: go test -race -covermode=atomic ./cmd/holdfast   exit=${r.status}`);
  console.log(out.slice(-10).map(l => '  | ' + l).join('\n'));
  console.log('');
}
