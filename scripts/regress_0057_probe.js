#!/usr/bin/env node
// Refuter probe harness for S0057-holdfast-pinning-1 (impl gate, ordinal 1).
//
// Scratch review artifact. It copies the holdfast checkout into /tmp and runs
// every mutation THERE, so the reviewed checkout is never touched.
// scripts/check-pins.sh derives $here from its own location, so a copy behaves
// identically to the original. The copy gets its OWN throwaway git repo (the
// original .git is a submodule gitfile whose target is unreachable from /tmp,
// and the pre-existing rename guard correctly reds on an unrunnable git).
//
//   node scripts/regress_0057_probe.js routes   spec A3/A6/A8/A12/A13 routes
//   node scripts/regress_0057_probe.js adv      adversarial probe suite
//   node scripts/regress_0057_probe.js a14mut   delete each refusal, selftest must red
//   node scripts/regress_0057_probe.js a4       re-resolve every action pin via gh

'use strict';
const fs = require('fs');
const path = require('path');
const cp = require('child_process');

const SRC = '/workspace/.worktrees/S0057-holdfast-pinning-1/holdfast';
const SCRATCH = '/tmp/hf-probe';

function sh(cmd, args, opts = {}) {
  return cp.spawnSync(cmd, args, { encoding: 'utf8', ...opts });
}

function fresh() {
  fs.rmSync(SCRATCH, { recursive: true, force: true });
  fs.cpSync(SRC, SCRATCH, { recursive: true });
  for (const f of ['regress_0057_probe.js', 'regress_0057_probe.sh']) {
    fs.rmSync(path.join(SCRATCH, 'scripts', f), { force: true });
  }
  fs.rmSync(path.join(SCRATCH, '.git'), { recursive: true, force: true });
  sh('git', ['-C', SCRATCH, 'init', '-q']);
  sh('git', ['-C', SCRATCH, 'add', '-A']);
  sh('git', ['-C', SCRATCH, '-c', 'user.email=r@x', '-c', 'user.name=r',
    'commit', '-q', '-m', 'scratch']);
}

function gate(script = 'check-pins.sh') {
  const r = sh('./scripts/' + script, [], { cwd: SCRATCH });
  return { rc: r.status === null ? -1 : r.status, out: (r.stdout || '') + (r.stderr || '') };
}

function edit(rel, fn) {
  const p = path.join(SCRATCH, rel);
  fs.writeFileSync(p, fn(fs.readFileSync(p, 'utf8')));
}

let unexpected = 0;
function probe(label, want, script) {
  const { rc, out } = gate(script);
  const bit = rc !== 0;
  const ok = want === 'pass' ? !bit : bit;
  console.log('='.repeat(78));
  console.log(`PROBE: ${label}`);
  console.log(`  exit=${rc}  want=${want === 'pass' ? '0' : 'non-zero'}`);
  if (!ok) {
    unexpected++;
    console.log(want === 'pass'
      ? '  RESULT: *** GATE FALSE-REFUSED ***'
      : '  RESULT: *** GATE DID NOT BITE ***');
  } else {
    console.log('  RESULT: as expected');
  }
  const lines = out.trimEnd().split('\n');
  const shown = want === 'pass' ? lines.slice(-4) : lines.filter(l => l.startsWith('::error::')).slice(0, 4);
  console.log('  --- gate output ---');
  for (const l of (shown.length ? shown : lines.slice(-4))) console.log('  | ' + l);
  console.log('');
}

// ------------------------------------------------------------------ A4 ----
function a4() {
  const wfdir = path.join(SRC, '.github/workflows');
  const seen = new Map();
  for (const f of fs.readdirSync(wfdir)) {
    for (const line of fs.readFileSync(path.join(wfdir, f), 'utf8').split('\n')) {
      const m = line.match(/uses:\s*([^\s]+)@([0-9a-f]{40})\s*#\s*(\S+)/);
      if (m) seen.set(`${m[1]}@${m[2]} # ${m[3]}`, m);
    }
  }
  let ok = 0, bad = 0;
  for (const [, m] of seen) {
    const [, repo, sha, tag] = m;
    let r = sh('gh', ['api', `repos/${repo}/git/ref/tags/${tag}`, '--jq', '.object.type+" "+.object.sha']);
    let [type, objsha] = (r.stdout || '').trim().split(' ');
    if (type === 'tag') {
      objsha = (sh('gh', ['api', `repos/${repo}/git/tags/${objsha}`, '--jq', '.object.sha']).stdout || '').trim();
    }
    if (objsha === sha) { ok++; console.log(`  OK        ${repo} ${tag} -> ${objsha}`); }
    else { bad++; console.log(`  MISMATCH  ${repo} ${tag} -> ${objsha}  != committed ${sha}`); }
  }
  console.log(`\n  A4: ${ok} pins matched their named tag, ${bad} mismatched (of ${seen.size} distinct)`);
  if (bad) unexpected++;
}

// -------------------------------------------------- spec mutation routes --
function routes() {
  fresh();
  edit('.github/workflows/pin-health.yml', s => s.replace(/uses: actions\/checkout@.*/, 'uses: actions/checkout@v4'));
  probe('A3 - action reference reverted to a mutable tag', 'refuse');

  fresh();
  edit('docker-compose.yml', s => s.replace(/^ {4}image: .*$/m, '    image: ghcr.io/nschatz/holdfast:latest'));
  probe('A6 - compose image put back to a floating :latest pull', 'refuse');

  fresh();
  edit('Dockerfile', s => s.replace(/^ARG RUNTIME_IMAGE=(.*)@sha256:.*$/m, 'ARG RUNTIME_IMAGE=$1'));
  probe('A8 - RUNTIME_IMAGE digest stripped', 'refuse');

  fresh();
  fs.writeFileSync(path.join(SCRATCH, 'package.json'), '{}\n');
  probe('A12 - untracked node manifest, no lifecycle-script decision', 'refuse');

  fresh();
  fs.rmSync(path.join(SCRATCH, 'docker-compose.yml'));
  probe('A13 - docker-compose.yml hidden from the gate', 'refuse');
}

// ----------------------------------------------- adversarial probe suite --
function adv() {
  const appendCompose = extra => { fresh(); fs.appendFileSync(path.join(SCRATCH, 'docker-compose.yml'), extra); };

  fresh();
  probe('ADV0 - clean copy, nothing mutated (must PASS)', 'pass');

  appendCompose('\n  sidecar:\n    image: ghcr.io/someone/sidecar\n');
  probe('ADV1 - second compose service, registry image, no tag no digest', 'refuse');

  appendCompose('\n  sidecar:\n    image: ghcr.io/someone/sidecar:v1.2.3\n');
  probe('ADV2 - second compose service, tag but no digest', 'refuse');

  appendCompose('\n  cache:\n    image: redis:7.2\n');
  probe('ADV3 - bare Docker Hub name (no slash), service declares no build:', 'refuse');

  appendCompose('\n  cache:\n    image: redis:latest\n    build:\n      context: .\n');
  probe('ADV4 - :latest beside a build: stanza (reading 2 says refuse)', 'refuse');

  fresh();
  edit('docker-compose.yml', s => s.replace(/^ {4}image: .*$/m,
    '    image: ghcr.io/nschatz/holdfast@sha256:302242b66f9c160e69b1e7c37d57925ec593bc7ed0ee9df851af0ec58c7cd4b2'));
  probe('ADV5 - compose image digest-only, no tag (reading 3 says refuse)', 'refuse');

  fresh();
  edit('docker-compose.yml', s => s.replace(/@sha256:[0-9a-f]{64}/, '@sha256:deadbeef'));
  probe('ADV6 - compose digest truncated to 8 hex', 'refuse');

  fresh();
  edit('.github/workflows/pin-health.yml', s => s.replace(/uses: actions\/checkout@.*/,
    'uses: actions/checkout@11D5960A326750D5838078E36CF38B85AF677262 # v4'));
  probe('ADV7 - action pinned to an UPPERCASE 40-hex sha', 'refuse');

  fresh();
  edit('.github/workflows/pin-health.yml', s => s.replace(/uses: actions\/checkout@.*/,
    'uses: actions/checkout@11d5960a326750d5838078e36cf38b85af67726 # v4'));
  probe('ADV8 - action pinned to a 39-hex sha', 'refuse');

  fresh();
  fs.writeFileSync(path.join(SCRATCH, '.github/workflows/newthing.yml'),
    'name: n\non: workflow_dispatch\njobs:\n  x:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/checkout@v4\n      - uses: some/action@main\n');
  probe('ADV9 - a workflow file added LATER carrying mutable refs', 'refuse');

  fresh();
  fs.writeFileSync(path.join(SCRATCH, '.github/workflows/newthing.yaml'),
    'name: n\non: workflow_dispatch\njobs:\n  x:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/checkout@v4\n');
  probe('ADV10 - mutable ref in a .yaml (not .yml) workflow', 'refuse');

  fresh();
  edit('.github/workflows/pin-health.yml', s => s.replace(/uses: actions\/checkout@[0-9a-f]{40} # v4/,
    'uses: "actions/checkout@v4"'));
  probe('ADV11 - mutable ref written as a QUOTED yaml scalar', 'refuse');

  fresh();
  edit('Dockerfile', s => s.replace(/^ARG FETCH_IMAGE=(.*)@sha256:.*$/m, 'ARG FETCH_IMAGE=$1'));
  probe('ADV12 - FETCH_IMAGE digest stripped', 'refuse');

  fresh();
  edit('Dockerfile', s => s.replace(/^ARG RUNTIME_IMAGE=([^:\n]*):[^@\n]*@/m, 'ARG RUNTIME_IMAGE=$1:latest@'));
  probe('ADV13 - RUNTIME_IMAGE tagged :latest beside a valid digest', 'refuse');

  fresh();
  fs.appendFileSync(path.join(SCRATCH, 'Dockerfile'), '\nFROM alpine:3.20 AS extra\n');
  probe('ADV14 - a literal unpinned FROM added to the Dockerfile', 'refuse');

  fresh();
  edit('Dockerfile', s => s.replace(/^ARG RUNTIME_IMAGE=.*$/m, 'ARG RUNTIME_IMAGE'));
  probe('ADV15 - FROM $RUNTIME_IMAGE with no ARG default', 'refuse');

  fresh();
  fs.writeFileSync(path.join(SCRATCH, 'package.json'), '{}\n');
  fs.writeFileSync(path.join(SCRATCH, '.npmrc'), 'ignore-scripts=true\n');
  probe('ADV16 - node manifest + UNCOMMITTED .npmrc (must still refuse)', 'refuse');

  fresh();
  fs.writeFileSync(path.join(SCRATCH, 'package.json'), '{}\n');
  fs.writeFileSync(path.join(SCRATCH, '.npmrc'), 'ignore-scripts=false\n');
  sh('git', ['-C', SCRATCH, 'add', '-f', 'package.json', '.npmrc']);
  probe('ADV17 - committed .npmrc ignore-scripts=false, NO reason (must refuse)', 'refuse');

  fresh();
  fs.writeFileSync(path.join(SCRATCH, 'package.json'), '{}\n');
  fs.writeFileSync(path.join(SCRATCH, '.npmrc'), 'ignore-scripts=false\n# lifecycle-scripts-reason: native build\n');
  sh('git', ['-C', SCRATCH, 'add', '-f', 'package.json', '.npmrc']);
  probe('ADV18 - committed decision WITH a reason (must PASS: tripwire not ban)', 'pass');

  fresh();
  fs.mkdirSync(path.join(SCRATCH, 'internal/webui/app'), { recursive: true });
  fs.writeFileSync(path.join(SCRATCH, 'internal/webui/app/package.json'), '{}\n');
  probe('ADV19 - node manifest nested three levels down', 'refuse');

  fresh();
  for (const f of fs.readdirSync(path.join(SCRATCH, '.github/workflows'))) {
    fs.rmSync(path.join(SCRATCH, '.github/workflows', f));
  }
  probe('ADV20 - .github/workflows emptied (enumeration would assert nothing)', 'refuse');

  fresh();
  fs.rmSync(path.join(SCRATCH, '.github/workflows'), { recursive: true, force: true });
  probe('ADV21 - .github/workflows removed entirely', 'refuse');

  fresh();
  fs.writeFileSync(path.join(SCRATCH, 'docker-compose.yml'), 'services:\n  holdfast:\n    container_name: holdfast\n');
  probe('ADV22 - compose file with no service image: at all', 'refuse');

  fresh();
  fs.chmodSync(path.join(SCRATCH, 'docker-compose.yml'), 0o000);
  probe('ADV23 - docker-compose.yml present but unreadable', 'refuse');
  fs.chmodSync(path.join(SCRATCH, 'docker-compose.yml'), 0o644);

  fresh();
  fs.appendFileSync(path.join(SCRATCH, '.github/workflows/release.yml'),
    '\n      - name: promote latest\n        run: |\n          docker buildx imagetools create -t "ghcr.io/nschatz/holdfast:latest" "ghcr.io/nschatz/holdfast:v9.9.9"\n');
  probe('ADV24 - release.yml PUBLISHING :latest (must PASS: publish != depend)', 'pass');

  fresh();
  edit('.github/workflows/pin-health.yml', s => s.replace(/uses: actions\/checkout@.*/,
    'uses: ./.github/actions/local-thing'));
  probe('ADV25 - local ./ action reference (must PASS: nothing upstream to pin)', 'pass');

  fresh();
  edit('Dockerfile', s => s.replace(/^ARG GO_IMAGE=(.*)@sha256:.*$/m, 'ARG GO_IMAGE=$1'));
  probe('ADV26 - GO_IMAGE digest stripped (pre-existing section 3 must still bite)', 'refuse');
}

// ------------------- A14: delete each refusal, the selftest must go red ----
function a14mut() {
  const marks = [
    ['section 5 MUTABLE ACTION REFERENCE', 'MUTABLE ACTION REFERENCE'],
    ['section 5 UNREADABLE ACTION PIN', 'UNREADABLE ACTION PIN'],
    ["section 6 FLOATING ':latest' IMAGE", "FLOATING ':latest' IMAGE"],
    ['section 6 UNPINNED IMAGE', 'UNPINNED IMAGE'],
    ['section 7 UNPINNED BASE IMAGE', 'UNPINNED BASE IMAGE'],
    ['section 7 UNREADABLE BASE IMAGE PIN', 'UNREADABLE BASE IMAGE PIN'],
    ['section 8 NODE MANIFEST refusal', 'NODE MANIFEST WITH NO LIFECYCLE-SCRIPT DECISION'],
    ['section 0 need_file/need_dir preflight', '__PREFLIGHT__'],
  ];
  for (const [label, mark] of marks) {
    fresh();
    const p = path.join(SCRATCH, 'scripts/check-pins.sh');
    const before = fs.readFileSync(p, 'utf8');
    let s = before;
    if (mark === '__PREFLIGHT__') {
      s = s.replace(/if \[ ! -e "\$here\/\$1" \]; then/g, 'if false; then')
           .replace(/elif \[ ! -f "\$here\/\$1" \] \|\| \[ ! -r "\$here\/\$1" \]; then/g, 'elif false; then')
           .replace(/elif \[ ! -d "\$here\/\$1" \] \|\| \[ ! -r "\$here\/\$1" \] \|\| \[ ! -x "\$here\/\$1" \]; then/g, 'elif false; then');
    } else {
      const re = new RegExp('(\\n\\s*)bad "' + mark.replace(/[.*+?^${}()|[\]\\]/g, '\\$&'), 'g');
      s = s.replace(re, '$1: "' + mark);
    }
    console.log('='.repeat(78));
    console.log(`A14 MUTANT: removed ${label}`);
    if (s === before) { unexpected++; console.log('  *** MUTATION DID NOT APPLY - inconclusive ***\n'); continue; }
    fs.writeFileSync(p, s);
    const r = sh('./scripts/check-pins-selftest.sh', [], { cwd: SCRATCH });
    const out = (r.stdout || '') + (r.stderr || '');
    console.log(`  selftest exit=${r.status}  want=non-zero`);
    if (r.status === 0) { unexpected++; console.log('  RESULT: *** SELFTEST STILL GREEN - that refusal is NOT covered ***'); }
    else console.log('  RESULT: selftest reds - the refusal is covered');
    for (const l of out.split('\n').filter(l => /FAIL|cases bite|expected|selftest:/i.test(l)).slice(0, 6)) {
      console.log('  | ' + l);
    }
    console.log('');
  }
}

const what = process.argv[2] || 'all';
if (what === 'a4' || what === 'all') a4();
if (what === 'routes' || what === 'all') routes();
if (what === 'adv' || what === 'all') adv();
if (what === 'a14mut' || what === 'all') a14mut();
console.log(`\n### unexpected outcomes: ${unexpected}`);
