const test = require('node:test');
const assert = require('node:assert/strict');
const p = require('../src/components/performance.js');

function php(over) {
  return Object.assign(
    {
      service: 'php-fpm',
      present: true,
      available: true,
      tracing: true,
      detail: 'full',
      profiling: true,
      env: { key: 'ROLL_INSPECTOR_TRACE', tracing: true, detail: 'full', profiling: true },
      differs: false,
      permanent: 'ROLL_INSPECTOR_TRACE=1\nROLL_INSPECTOR_TRACE_DETAIL=full\nROLL_INSPECTOR_PROFILE=excimer',
    },
    over || {},
  );
}

test('a switch row shows the live state, the .env.roll value and what is pressed', () => {
  const [row] = p.switchRows([php()], { busy: false, pending: '' });
  assert.equal(row.title, 'php-fpm');
  assert.equal(row.tracing.checked, 'true');
  assert.equal(row.tracing.disabled, false);
  assert.equal(row.profiling.checked, 'true');
  assert.deepEqual(
    row.detail.options.map((o) => [o.id, o.pressed, o.disabled]),
    [
      ['full', 'true', false],
      ['lean', 'false', false],
    ],
  );
  assert.equal(row.envText, '.env.roll: tracing on · full · profiling');
  assert.equal(row.differs, false);
});

test('with tracing off, detail and profiling are disabled and say why', () => {
  const [row] = p.switchRows([php({ tracing: false, profiling: false, detailReason: 'tracing is off' })], { busy: false, pending: '' });
  assert.equal(row.tracing.checked, 'false');
  assert.equal(row.profiling.disabled, true);
  assert.equal(row.profiling.why, 'tracing is off');
  assert.ok(row.detail.options.every((o) => o.disabled));
  assert.equal(row.detail.why, 'tracing is off');
});

test('a change against .env.roll shows the lines that make it permanent', () => {
  const [row] = p.switchRows([php({ detail: 'lean', profiling: false, differs: true, permanent: 'ROLL_INSPECTOR_TRACE=1\nROLL_INSPECTOR_TRACE_DETAIL=lean\nROLL_INSPECTOR_PROFILE=off' })], { busy: false, pending: '' });
  assert.equal(row.differs, true);
  assert.match(row.lasts, /until php-fpm restarts/);
  assert.equal(row.permanent, 'ROLL_INSPECTOR_TRACE=1\nROLL_INSPECTOR_TRACE_DETAIL=lean\nROLL_INSPECTOR_PROFILE=off');
});

test('an unavailable container or a running benchmark disables every switch', () => {
  const [gone] = p.switchRows([php({ service: 'php-debug', present: false, available: false, reason: 'php-debug is not running in this env', tracing: false })], { busy: false, pending: '' });
  assert.equal(gone.tracing.disabled, true);
  assert.equal(gone.reason, 'php-debug is not running in this env');
  const [old] = p.switchRows([php({ available: false, reason: 'this php-fpm image predates tracing: run roll env pull && roll env up' })], { busy: false, pending: '' });
  assert.equal(old.tracing.disabled, true);
  assert.match(old.reason, /predates/);
  const [busy] = p.switchRows([php()], { busy: true, pending: '' });
  assert.equal(busy.tracing.disabled, true);
  assert.match(busy.tracing.why, /benchmark/);
  const [pending] = p.switchRows([php()], { busy: false, pending: 'php-fpm' });
  assert.equal(pending.tracing.disabled, true);
});

test('lean is offered only where the image knows it', () => {
  const [row] = p.switchRows([php({ detailReason: 'this php-fpm image predates lean tracing: run roll env pull && roll env up' })], { busy: false, pending: '' });
  assert.equal(row.detail.options.find((o) => o.id === 'lean').disabled, true);
  assert.equal(row.detail.options.find((o) => o.id === 'full').disabled, false);
  assert.match(row.detail.why, /predates lean/);
});

test('sampling options name the live interval and the .env.roll default', () => {
  const s = { interval: 30, paused: false, default: 10, defaultPaused: false, options: ['10', '30', '60', 'off'], permanent: 'ROLL_INSPECTOR_SAMPLE=30' };
  assert.deepEqual(
    p.sampleOptions(s).map((o) => [o.id, o.label, o.pressed]),
    [
      ['10', '10 s', 'false'],
      ['30', '30 s', 'true'],
      ['60', '60 s', 'false'],
      ['off', 'Paused', 'false'],
    ],
  );
  assert.equal(p.sampleNote(s), '.env.roll: every 10 s. This lasts until Inspector restarts.');
  assert.equal(p.sampleDiffers(s), true);
  assert.equal(p.sampleDiffers({ interval: 10, paused: false, default: 10, defaultPaused: false }), false);
  assert.equal(p.sampleOptions({ interval: 0, paused: true, options: ['10', '30', '60', 'off'] })[3].pressed, 'true');
});

test('the cost KPIs read the snapshot', () => {
  const snap = {
    inspector: { cpuPct: 2.04, memBytes: 13 << 20, memLimit: 768 << 20, memPct: 1.69 },
    intake: { window: 300, traces: 12, avgSpans: 2430.4, avgBytes: 1.5 * (1 << 20), mbPerMin: 4.2, evicted: 3, dropped: 1, profiles: 12, avgProfileBytes: 300 << 10 },
    sampler: { execsPerMin: 22.8 },
  };
  const k = Object.fromEntries(p.kpis(snap).map((x) => [x.key, x]));
  assert.equal(k.cpu.value, '2.0%');
  assert.equal(k.mem.value, '13 MB');
  assert.equal(k.mem.sub, 'of 768 MB (1.7%)');
  assert.equal(k.traces.value, '12');
  assert.equal(k.traces.sub, 'traced in 5 min');
  assert.equal(k.spans.value, '2,430');
  assert.equal(k.payload.value, '1.5 MB');
  assert.equal(k.payload.sub, '+ 300 KB profile');
  assert.equal(k.intake.value, '4.2 MB/min');
  assert.equal(k.dropped.value, '4');
  assert.equal(k.dropped.sub, 'by the store budget');
  assert.equal(k.execs.value, '23/min');
  const skipped = Object.fromEntries(
    p.kpis({ inspector: {}, intake: { traces: 3, rejected: 2, lastReject: 'profile x: body is over the limit', skipped: { benchmark: 41, inspector: 0 } }, sampler: {} }).map((x) => [x.key, x]),
  );
  assert.equal(skipped.traces.sub, 'traced in 5 min · 41 benchmark requests left out');
  assert.equal(skipped.dropped.sub, 'by the store budget · 2 payloads refused, last: profile x: body is over the limit');
  const first = Object.fromEntries(p.kpis({ inspector: { cpuPct: null }, intake: {}, sampler: {} }).map((x) => [x.key, x]));
  assert.equal(first.cpu.value, '–');
  assert.equal(first.cpu.sub, 'measuring');
});

test('sampler ticks become ranked bars with the slowest tick and errors as a flag', () => {
  const rows = p.tickRows([
    { kind: 'redis', ticks: 7, avgMs: 141, maxMs: 373, errors: 0 },
    { kind: 'rabbitmq', ticks: 6, avgMs: 315.5, maxMs: 577, errors: 2, lastError: 'boom' },
  ]);
  assert.deepEqual(
    rows.map((r) => [r.key, r.label, Math.round(r.value)]),
    [
      ['redis', 'Valkey / Redis', 141],
      ['rabbitmq', 'RabbitMQ', 316],
    ],
  );
  assert.equal(rows[1].flag, '2 failed');
  assert.match(rows[0].full, /slowest 373 ms/);
});

test('intake series turn MB into bytes for the chart axis', () => {
  const s = p.intakeSeries({ series: { step: 60, mb: [[100, 1], [160, 0.5]], traces: [[100, 3], [160, 1]] } });
  assert.deepEqual(s.series.bytes, [
    [100, 1 << 20],
    [160, 0.5 * (1 << 20)],
  ]);
});

test('benchmark rows carry the delta against off as their note', () => {
  const rows = p.benchRows({
    arms: [
      { id: 'off', label: 'Tracing off', samples: [400, 410], minMs: 400, maxMs: 410, medianMs: 405, deltaPct: null },
      { id: 'full', label: 'Full', samples: [500], minMs: 500, maxMs: 500, medianMs: 500, deltaPct: 23.456 },
      { id: 'lean', label: 'Lean', samples: [], deltaPct: -1.2 },
    ],
  });
  assert.deepEqual(
    rows.map((r) => [r.key, r.median, r.note]),
    [
      ['off', 405, ''],
      ['full', 500, '+23%'],
      ['lean', undefined, '−1%'],
    ],
  );
});

test('benchmark progress and phase read like a sentence', () => {
  const b = { running: true, phase: 'warming', round: 2, rounds: 5, arm: 'full+profile', done: 6, total: 20, warmups: 2 };
  const pr = p.benchProgress(b);
  assert.equal(pr.pct, 30);
  assert.equal(pr.text, 'Round 2 of 5 · Full + profiling · warming up (2 requests)');
  assert.equal(p.benchProgress({ running: true, phase: 'probing', done: 0, total: 20 }).text, 'Checking that the page answers 200');
  assert.equal(p.benchProgress({ running: false, phase: 'done', done: 20, total: 20 }).pct, 100);
});

test('the benchmark verdict names the costliest arm and the restore', () => {
  const b = {
    phase: 'done',
    restored: true,
    arms: [
      { id: 'off', medianMs: 400, samples: [1] },
      { id: 'lean', medianMs: 420, deltaPct: 5, samples: [1] },
      { id: 'full', medianMs: 480, deltaPct: 20, samples: [1] },
      { id: 'full+profile', medianMs: 500, deltaPct: 25, samples: [1] },
    ],
  };
  assert.equal(p.benchVerdict(b), 'Full tracing with profiling makes this page 25% slower (400 ms → 500 ms median); lean costs 5%. Your previous settings are back.');
  assert.match(p.benchVerdict({ phase: 'cancelled', restored: true, arms: [] }), /Cancelled/);
  assert.match(p.benchVerdict({ phase: 'failed', error: 'x answered 302', restored: true, arms: [] }), /302/);
  assert.match(p.benchVerdict({ phase: 'done', restored: false, restoreError: 'exec failed', arms: [] }), /could not put back.*exec failed/);
});

test('the top-bar pill reads the session summary', () => {
  assert.deepEqual(p.pill({ label: 'Tracing · full · profiling', tracing: true, busy: false }), { label: 'Tracing · full · profiling', on: 'true', busy: 'false' });
  assert.equal(p.pill({ label: 'Tracing off', tracing: false }).on, 'false');
  assert.equal(p.pill({ label: 'Tracing · lean', tracing: true, busy: true }).label, 'Measuring overhead…');
  assert.equal(p.pill(null), null);
});

test('load and live sources read in plain words', () => {
  assert.equal(p.loadText({ one: 12, five: 9.07, fifteen: 8.23, cpus: 10 }), '12.00 · 9.07 · 8.23 on 10 CPUs');
  assert.equal(p.loadText({ error: 'no file' }), '–');
});

test('the benchmark URL comes from the hash, then the slowest page', () => {
  assert.equal(p.benchURL({ url: '/women.html' }, 'https://app.shop.test/x'), '/women.html');
  assert.equal(p.benchURL({}, 'https://app.shop.test/x'), 'https://app.shop.test/x');
  assert.equal(p.benchURL({}, ''), '/');
});

test('a traced setting faster than off is called noise, not a saving', () => {
  const b = {
    phase: 'done',
    restored: true,
    arms: [
      { id: 'off', medianMs: 224, samples: [1] },
      { id: 'lean', medianMs: 209, deltaPct: -7, samples: [1] },
      { id: 'full', medianMs: 336, deltaPct: 49.5, samples: [1] },
      { id: 'full+profile', medianMs: 280, deltaPct: 24.8, samples: [1] },
    ],
  };
  assert.equal(
    p.benchVerdict(b),
    'Full tracing with profiling makes this page 25% slower (224 ms → 280 ms median); lean came out 7% faster than off. The settings did not come out in cost order, so differences this size are noise from other load: run it again when the machine is quieter. Your previous settings are back.',
  );
});

test('settings out of cost order are called noise too', () => {
  const arm = (id, d) => ({ id, medianMs: 200, deltaPct: d, samples: [1] });
  const out = p.benchVerdict({ phase: 'done', restored: true, arms: [{ id: 'off', medianMs: 195, samples: [1] }, arm('lean', 10), arm('full', 28.4), arm('full+profile', 12)] });
  assert.match(out, /not come out in cost order/);
  const clean = p.benchVerdict({ phase: 'done', restored: true, arms: [{ id: 'off', medianMs: 195, samples: [1] }, arm('lean', 3), arm('full', 6), arm('full+profile', 8)] });
  assert.doesNotMatch(clean, /noise/);
});

test('a skipped arm says why in its row and in the verdict', () => {
  const b = {
    phase: 'done',
    restored: true,
    arms: [
      { id: 'off', medianMs: 400, samples: [1] },
      { id: 'lean', samples: [], deltaPct: null, skipped: 'not supported by this php-fpm image: it predates lean tracing' },
      { id: 'full', medianMs: 480, deltaPct: 20, samples: [1] },
      { id: 'full+profile', medianMs: 500, deltaPct: 25, samples: [1] },
    ],
  };
  const lean = p.benchRows(b).find((r) => r.key === 'lean');
  assert.equal(lean.note, 'not measured');
  assert.match(p.benchVerdict(b), /Lean was not measured: not supported by this php-fpm image/);
});

test('a finished run shows the load during the run, and warns when it was high', () => {
  const b = { phase: 'done', running: false, restored: true, loadBefore: { one: 3.1, five: 2, fifteen: 1, cpus: 10 }, loadAfter: { one: 4.04, five: 2, fifteen: 1, cpus: 10 }, arms: [] };
  assert.equal(p.runLoadText(b), 'Docker VM load 3.10 → 4.04 on 10 CPUs during the run');
  assert.equal(p.runLoadText({ running: true, loadBefore: { one: 1, cpus: 2 } }), '');
  assert.equal(p.runLoadText({ phase: 'done', loadBefore: { error: 'x' } }), '');
  const busy = {
    phase: 'done',
    restored: true,
    loadBefore: { one: 9, cpus: 10 },
    loadAfter: { one: 4, cpus: 10 },
    arms: [
      { id: 'off', medianMs: 400, samples: [1] },
      { id: 'full', medianMs: 480, deltaPct: 20, samples: [1] },
    ],
  };
  assert.match(p.benchVerdict(busy), /busy during the run \(load 9\.00 on 10 CPUs\)/);
  const quiet = Object.assign({}, busy, { loadBefore: { one: 2, cpus: 10 }, loadAfter: { one: 3, cpus: 10 } });
  assert.doesNotMatch(p.benchVerdict(quiet), /busy/);
});

test('live sources say who holds them and offer a stop', () => {
  const rows = p.liveRows([
    { key: 'redis:monitor;match=a', holders: ['api'] },
    { key: 'timeline', holders: ['overview'] },
    { key: 'varnish:log', holders: ['drawer', 'timeline'] },
    { key: 'varnish:log;q=x', holders: ['api lease'] },
  ]);
  assert.deepEqual(
    rows.map((r) => [r.key, r.holders]),
    [
      ['redis:monitor;match=a', 'a page through /api/v1 (any site open in this browser)'],
      ['timeline', 'the Overview'],
      ['varnish:log', 'the live drawer, the Overview timeline'],
      ['varnish:log;q=x', 'api lease: a /api/v1/page or /api/v1/requests call in the last 3 min'],
    ],
  );
  assert.equal(rows[0].stopLabel, 'Stop redis:monitor;match=a');
});
