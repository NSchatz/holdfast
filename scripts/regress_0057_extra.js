#!/usr/bin/env node
// Second refuter probe set for S0057-holdfast-pinning-1: edge shapes the first
// suite did not cover. Same throwaway-copy discipline; the reviewed checkout is
// never mutated.
'use strict';
const fs = require('fs');
const path = require('path');
const cp = require('child_process');

const SRC = '/workspace/.worktrees/S0057-holdfast-pinning-1/holdfast';
const SCRATCH = '/tmp/hf-probe2';
const sh = (c, a, o = {}) => cp.spawnSync(c, a, { encoding: 'utf8', ...o });

function fresh() {
  fs.rmSync(SCRATCH, { recursive: true, force: true });
  fs.cpSync(SRC, SCRATCH, { recursive: true });
  for (const f of fs.readdirSync(path.join(SCRATCH, 'scripts'))) {
    if (f.startsWith('regress_0057')) fs.rmSync(path.join(SCRATCH, 'scripts', f));
  }
  fs.rmSync(path.join(SCRATCH, '.git'), { recursive: true, force: true });
  sh('git', ['-C', SCRATCH, 'init', '-q']);
  sh('git', ['-C', SCRATCH, 'add', '-A']);
  sh('git', ['-C', SCRATCH, '-c', 'user.email=r@x', '-c', 'user.name=r', 'commit', '-q', '-m', 's']);
}
const edit = (rel, fn) => {
  const p = path.join(SCRATCH, rel);
  fs.writeFileSync(p, fn(fs.readFileSync(p, 'utf8')));
};

let unexpected = 0;
function probe(label, want) {
  const r = sh('./scripts/check-pins.sh', [], { cwd: SCRATCH });
  const out = (r.stdout || '') + (r.stderr || '');
  const bit = r.status !== 0;
  const ok = want === 'pass' ? !bit : bit;
  console.log('='.repeat(78));
  console.log(`PROBE: ${label}`);
  console.log(`  exit=${r.status}  want=${want === 'pass' ? '0' : 'non-zero'}`);
  if (!ok) { unexpected++; console.log(want === 'pass' ? '  RESULT: *** GATE FALSE-REFUSED ***' : '  RESULT: *** GATE DID NOT BITE ***'); }
  else console.log('  RESULT: as expected');
  const errs = out.split('\n').filter(l => l.startsWith('::error::')).slice(0, 3);
  for (const l of (errs.length ? errs : out.trimEnd().split('\n').slice(-3))) console.log('  | ' + l);
  console.log('');
}

// X1 - a REUSABLE WORKFLOW call at job level (a different YAML level from a step)
fresh();
fs.writeFileSync(path.join(SCRATCH, '.github/workflows/reuse.yml'),
  'name: reuse\non: workflow_dispatch\njobs:\n  call:\n    uses: someone/repo/.github/workflows/build.yml@v1\n');
probe('X1 - reusable-workflow call pinned to a mutable tag (job level, not a step)', 'refuse');

// X2 - the same reusable-workflow call, correctly SHA-pinned with a comment
fresh();
fs.writeFileSync(path.join(SCRATCH, '.github/workflows/reuse.yml'),
  'name: reuse\non: workflow_dispatch\njobs:\n  call:\n    uses: someone/repo/.github/workflows/build.yml@11d5960a326750d5838078e36cf38b85af677262 # v1\n');
probe('X2 - reusable-workflow call SHA-pinned with a version comment (must PASS)', 'pass');

// X3 - a docker:// action reference
fresh();
edit('.github/workflows/pin-health.yml', s => s.replace(/uses: actions\/checkout@.*/, 'uses: docker://alpine:3.20'));
probe('X3 - docker:// action reference', 'refuse');

// X4 - action pinned to a 40-char string that is not hex
fresh();
edit('.github/workflows/pin-health.yml', s => s.replace(/uses: actions\/checkout@.*/,
  'uses: actions/checkout@zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz # v4'));
probe('X4 - action pinned to a 40-char NON-hex string', 'refuse');

// X5 - FROM scratch (a legitimate base that cannot carry a digest)
fresh();
fs.appendFileSync(path.join(SCRATCH, 'Dockerfile'), '\nFROM scratch AS empty\nCOPY --from=build /out/holdfast /holdfast\n');
probe('X5 - FROM scratch added (no exemption path exists)', 'refuse');

// X6 - the SHA is valid but the trailing comment is pure whitespace
fresh();
edit('.github/workflows/pin-health.yml', s => s.replace(/uses: actions\/checkout@([0-9a-f]{40}) # v4/,
  'uses: actions/checkout@$1 #   '));
probe('X6 - valid SHA, comment present but empty', 'refuse');

// X7 - compose service with build: and a bare local tag (A5's stated exemption)
fresh();
edit('docker-compose.yml', s => s.replace(/^ {4}image: .*$/m, '    image: holdfast-local:dev\n    build:\n      context: .'));
probe('X7 - bare local tag WITH a build: stanza (A5 exemption, must PASS)', 'pass');

// X8 - same bare local tag but WITHOUT build:
fresh();
edit('docker-compose.yml', s => s.replace(/^ {4}image: .*$/m, '    image: holdfast-local:dev'));
probe('X8 - bare local tag with NO build: stanza (must refuse)', 'refuse');

// X9 - NOTICE removed (a preflight dependency)
fresh();
fs.rmSync(path.join(SCRATCH, 'NOTICE'));
probe('X9 - NOTICE removed (preflight dependency)', 'refuse');

// X10 - Dockerfile removed
fresh();
fs.rmSync(path.join(SCRATCH, 'Dockerfile'));
probe('X10 - Dockerfile removed (preflight dependency)', 'refuse');

// X11 - a package.json that is a DIRECTORY, not a file
fresh();
fs.mkdirSync(path.join(SCRATCH, 'package.json'));
probe('X11 - package.json exists as a DIRECTORY (must not crash; PASS is correct)', 'pass');

// X12 - .github/workflows contains only a non-workflow file
fresh();
for (const f of fs.readdirSync(path.join(SCRATCH, '.github/workflows'))) {
  fs.rmSync(path.join(SCRATCH, '.github/workflows', f));
}
fs.writeFileSync(path.join(SCRATCH, '.github/workflows/README.md'), 'notes\n');
probe('X12 - workflows dir holds only a README (no .yml at all)', 'refuse');

console.log(`\n### unexpected outcomes: ${unexpected}`);
