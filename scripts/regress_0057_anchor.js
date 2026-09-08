#!/usr/bin/env node
// Probe: can a floating compose image reach a service through a YAML anchor
// without section 6 seeing it? Throwaway copy; the reviewed checkout is untouched.
'use strict';
const fs = require('fs');
const path = require('path');
const cp = require('child_process');
const SRC = '/workspace/.worktrees/S0057-holdfast-pinning-1/holdfast';
const S = '/tmp/hf-anchor';
const sh = (c, a, o = {}) => cp.spawnSync(c, a, { encoding: 'utf8', ...o });

function fresh() {
  fs.rmSync(S, { recursive: true, force: true });
  fs.cpSync(SRC, S, { recursive: true });
  for (const f of fs.readdirSync(path.join(S, 'scripts'))) {
    if (f.startsWith('regress_0057')) fs.rmSync(path.join(S, 'scripts', f));
  }
  fs.rmSync(path.join(S, '.git'), { recursive: true, force: true });
  sh('git', ['-C', S, 'init', '-q']); sh('git', ['-C', S, 'add', '-A']);
  sh('git', ['-C', S, '-c', 'user.email=r@x', '-c', 'user.name=r', 'commit', '-q', '-m', 's']);
}

function run(label, compose) {
  fresh();
  fs.writeFileSync(path.join(S, 'docker-compose.yml'), compose);
  const g = sh('./scripts/check-pins.sh', [], { cwd: S });
  const c = sh('docker', ['compose', '-f', 'docker-compose.yml', 'config'], { cwd: S });
  console.log('='.repeat(76));
  console.log(label);
  console.log('  check-pins.sh exit = ' + g.status + (g.status === 0 ? '   <-- GATE PASSED' : '   <-- gate refused'));
  const errs = ((g.stdout || '') + (g.stderr || '')).split('\n').filter(l => /::error::|image reference/.test(l));
  for (const l of errs.slice(0, 3)) console.log('  | ' + l);
  console.log('  what docker compose ACTUALLY resolves:');
  for (const l of (c.stdout || '').split('\n').filter(l => l.includes('image:'))) console.log('  > ' + l.trim());
  if (c.status !== 0) console.log('  (compose config exit ' + c.status + ': ' + (c.stderr || '').trim().slice(0, 200) + ')');
  console.log('');
}

const PINNED = 'ghcr.io/nschatz/holdfast:v0.1.0@sha256:302242b66f9c160e69b1e7c37d57925ec593bc7ed0ee9df851af0ec58c7cd4b2';

run('CONTROL - the tracked shape (one literal pinned image:)',
  `services:\n  holdfast:\n    image: ${PINNED}\n    container_name: holdfast\n`);

run('ANCHOR - a floating image reaches a service through a top-level YAML anchor',
  `x-common: &common\n  image: ghcr.io/nschatz/holdfast:latest\n\n` +
  `services:\n  holdfast:\n    image: ${PINNED}\n    container_name: holdfast\n` +
  `  sidecar:\n    <<: *common\n    container_name: sidecar\n`);

run('EXTENDS - a floating image reaches a service through `extends` in the same file',
  `services:\n  base:\n    image: ghcr.io/nschatz/holdfast:latest\n` +
  `  holdfast:\n    image: ${PINNED}\n    container_name: holdfast\n`);
