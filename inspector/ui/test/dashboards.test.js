const test = require('node:test');
const assert = require('node:assert/strict');
const d = require('../src/components/dashboards.js');
const charts = require('../src/lib/charts.js');

const T = 1792577000;

function pts(values, start, step) {
  return values.map((v, i) => [(start || T) + i * (step || 10), v]);
}

test('the window comes from the hash and falls back to 15 minutes', () => {
  assert.equal(d.windowOf({}), '15m');
  assert.equal(d.windowOf({ win: '5m' }), '5m');
  assert.equal(d.windowOf({ win: '60m' }), '60m');
  assert.equal(d.windowOf({ win: '2h' }), '15m');
  assert.deepEqual(
    d.windowButtons({ win: '60m' }).map((w) => [w.id, w.pressed]),
    [
      ['5m', 'false'],
      ['15m', 'false'],
      ['60m', 'true'],
    ],
  );
});

test('table views are a list in the hash that each chart adds itself to or leaves', () => {
  assert.deepEqual(d.tableList({}), []);
  assert.deepEqual(d.tableList({ table: 'hit,objects' }), ['hit', 'objects']);
  assert.equal(d.withTable({}, 'hit', true), 'hit');
  assert.equal(d.withTable({ table: 'hit' }, 'objects', true), 'hit,objects');
  assert.equal(d.withTable({ table: 'hit,objects' }, 'hit', false), 'objects');
  assert.equal(d.withTable({ table: 'hit' }, 'hit', false), '');
  assert.equal(d.withTable({ table: 'hit' }, 'hit', true), 'hit');
});

test('a ratio donut shows the window, the first minute as its start and the lifetime underneath', () => {
  const m = {
    samples: 30,
    series: {
      hit: pts([1, 1, 1, 1, 1, 1, 9, 9]),
      miss: pts([3, 3, 3, 3, 3, 3, 1, 1]),
      hitpass: pts([0, 0, 0, 0, 0, 0, 0, 0]),
    },
    totals: { hit_ratio: 0.6 },
    latest: { hit_ratio: 0.5044 },
  };
  const out = d.ratioDonut(m, 'hit_ratio', ['hit'], ['hit', 'miss', 'hitpass']);
  assert.equal(out.value, 0.6);
  assert.equal(out.start, 0.25);
  assert.equal(out.sub, 'Lifetime 50.4%');
  assert.equal(out.startLabel, 'at the window start');
});

test('every Varnish hit ratio divides by hits, misses and passes over the picked window', () => {
  assert.deepEqual(d.VARNISH_HIT.den, ['hit', 'miss', 'pass']);
  const m = {
    samples: 30,
    series: { hit: pts([0.3]), miss: pts([0]), pass: pts([21.5]) },
    totals: { hit: 3, miss: 0, pass: 215, hit_ratio: 3 / 218 },
    latest: { hit_ratio: 0.955 },
  };
  assert.deepEqual(d.varnishHitCard(m, '15m'), {
    label: 'Hit rate, last 15 min',
    match: 'hit rate',
    value: '1.4%',
    sub: '3 hits · 0 misses · 215 passes (not cacheable) · lifetime 95.5%',
    cls: '',
  });
  assert.equal(d.varnishHitCard(m, '60m').label, 'Hit rate, last 60 min');
  assert.equal(d.tileData('varnish', m).value, 3 / 218);
  const idle = d.varnishHitCard({ samples: 30, series: {}, totals: { hit_ratio: null }, latest: { hit_ratio: 0.5 } }, '5m');
  assert.equal(idle.value, '–');
  assert.equal(idle.sub, 'no requests in the last 5 min · lifetime 50.0%');
  assert.equal(d.varnishHitCard({ samples: 1 }, '5m'), null);
  assert.equal(d.varnishHitCard(null, '5m'), null);
});

test('a ratio with nothing counted says so, unless the history is still filling', () => {
  assert.equal(d.ratioEmpty({ samples: 1 }, 'hit_ratio', 'cache lookups'), charts.EMPTY_TEXT);
  assert.equal(d.ratioEmpty({ samples: 30, latest: { hit_ratio: 0.5 } }, 'hit_ratio', 'cache lookups'), 'No cache lookups in this window. Lifetime: 50.0%.');
  assert.equal(d.ratioEmpty({ samples: 30, latest: { hit_ratio: null } }, 'hit_ratio', 'cache lookups'), 'No cache lookups in this window.');
  const empty = d.ratioDonut({ samples: 30, series: {}, totals: { hit_ratio: null }, latest: {} }, 'hit_ratio', ['hit'], ['hit']);
  assert.equal(empty.value, null);
  assert.equal(empty.start, null);
});

test('a fill ring needs a limit, otherwise it has no value', () => {
  const fmtB = (n) => n + ' B';
  assert.deepEqual(d.fillDonut(25, 100, fmtB, 'used'), { value: 0.25, sub: '25 B used of 100 B' });
  assert.deepEqual(d.fillDonut(25, 0, fmtB, 'used'), { value: null });
  assert.equal(d.fillDonut(200, 100, fmtB, 'used').value, 1);
});

test('Varnish storage is a ring where free space is known and a line where it is not', () => {
  const chart = d.SPECS.varnish.charts.find((c) => c.id === 'storage');
  const withFree = { latest: { g_bytes: 25, g_space: 75 }, series: { g_bytes: pts([10, 25]), g_space: pts([90, 75]) } };
  assert.equal(d.formOf(chart, withFree, {}), 'donut');
  const ring = chart.data(withFree, {}, 'donut');
  assert.equal(ring.value, 0.25);
  assert.equal(ring.start, 0.1);
  const flat = chart.data({ latest: { g_bytes: 25, g_space: 75 }, series: { g_bytes: pts([25, 25]), g_space: pts([75, 75]) } }, {}, 'donut');
  assert.equal(flat.start, undefined);
  assert.equal(d.formOf(chart, { latest: { g_bytes: 25 } }, {}), 'timeseries');
  assert.equal(d.optsOf(chart, null, 'timeseries').unit, 'bytes');
});

test('Valkey memory is a ring only when maxmemory is set', () => {
  const chart = d.SPECS.redis.charts.find((c) => c.id === 'memory');
  assert.equal(d.formOf(chart, { latest: { maxmemory: 0, used_memory: 10 } }, {}), 'timeseries');
  const m = { latest: { maxmemory: 100, used_memory: 40 } };
  assert.equal(d.formOf(chart, m, {}), 'donut');
  assert.equal(chart.data(m, {}, 'donut').value, 0.4);
});

test('keys per database carry the Magento cache names', () => {
  const types = {
    types: [
      { name: 'default', label: 'Default cache', db: 0 },
      { name: 'page_cache', label: 'Page cache', db: 1 },
      { name: 'session', label: 'Sessions', db: 2 },
    ],
  };
  const rows = d.dbRows({ latest: { 'keys.db0': 1150, 'keys.db2': 4, ops: 9 } }, types);
  assert.deepEqual(
    rows.map((r) => [r.key, r.label, r.value]),
    [
      ['db0', 'db0 · Default cache', 1150],
      ['db2', 'db2 · Sessions', 4],
    ],
  );
  assert.equal(d.dbRows({ latest: { 'keys.db5': 1 } }, null)[0].label, 'db5');
});

test('command rows use the window totals and grey the folded tail', () => {
  const rows = d.commandRows({ totals: { 'calls.client|setname': 3, 'calls.get': 0, 'calls.other': 120, ops: 45 } });
  assert.deepEqual(
    rows.map((r) => [r.label, r.value, r.other]),
    [
      ['CLIENT SETNAME', 3, false],
      ['Other commands', 120, true],
    ],
  );
});

test('queues join ready and unacked, flag the ones without a consumer and skip the empty ones', () => {
  const latest = {
    'ready.a': 16,
    'unacked.a': 0,
    'consumers.a': 0,
    'ready.b': 2,
    'unacked.b': 3,
    'consumers.b': 1,
    'ready.c': 0,
    'unacked.c': 0,
    'consumers.c': 0,
    'ready.other': 5,
    'unacked.other': 0,
    'consumers.other': 0,
  };
  const rows = d.queueRows({ latest });
  assert.deepEqual(
    rows.map((r) => [r.key, r.label, r.values.ready, r.values.unacked, r.flag]),
    [
      ['a', 'a', 16, 0, 'no consumer'],
      ['b', 'b', 2, 3, ''],
      ['other', 'Other queues', 5, 0, ''],
    ],
  );
  assert.deepEqual(
    d.consumerRows({ latest }).map((r) => [r.label, r.value]),
    [['b', 1]],
  );
});

test('latency buckets get short axis labels and full names for the tooltip', () => {
  const hist = [
    { from: 0, to: 50, count: 1 },
    { from: 50, to: 100, count: 2 },
    { from: 100, to: 200, count: 0 },
    { from: 200, to: 500, count: 0 },
    { from: 500, to: 1000, count: 0 },
    { from: 1000, to: 2000, count: 0 },
    { from: 2000, to: 5000, count: 0 },
    { from: 5000, to: null, count: 4 },
  ];
  const rows = d.histogramRows({ histogram: hist });
  assert.deepEqual(
    rows.map((r) => r.label),
    ['≤50', '100', '200', '500', '1 s', '2 s', '5 s', '>5 s'],
  );
  assert.deepEqual(
    rows.map((r) => r.full),
    ['50 ms or less', '50 to 100 ms', '100 to 200 ms', '200 to 500 ms', '500 ms to 1 s', '1 to 2 s', '2 to 5 s', 'over 5 s'],
  );
  assert.deepEqual(
    rows.map((r) => r.value),
    [1, 2, 0, 0, 0, 0, 0, 4],
  );
});

test('missed pages keep the reason and flag the ones a cookie or header keeps out of cache', () => {
  const rows = d.missRows({
    raw: {
      seen: 40,
      topMisses: [
        { url: '/a', misses: 9, reason: 'Set-Cookie on the response' },
        { url: '/b', misses: 3, reason: 'first visit' },
      ],
    },
  });
  assert.equal(rows[0].flag, 'Set-Cookie on the response');
  assert.equal(rows[0].full, '/a · Set-Cookie on the response');
  assert.equal(rows[1].flag, '');
  assert.match(d.missEmpty({ raw: { seen: 0 } }), /Nothing counted yet/);
  assert.match(d.missEmpty({ raw: { seen: 40 } }), /last 40 requests/);
});

test('summing a group gives one series of messages waiting', () => {
  const m = { series: { 'ready.a': pts([1, 2]), 'ready.b': pts([3, 4]), 'unacked.a': pts([9, 9]) } };
  assert.deepEqual(d.sumSeries(m, 'ready.'), [
    [T, 4],
    [T + 10, 6],
  ]);
});

test('the PHP numbers read the window totals and show a dash for no data', () => {
  const k = d.phpKpis({ totals: { requests: 205, p50: 310, p95: 1100, max: 2400, sql_p50: 80, sql_p95: 160.4 } }, '15m');
  assert.deepEqual(
    k.map((x) => x.value),
    ['205', '310 ms', '1.1 s', '2.4 s', '160'],
  );
  assert.equal(k[0].sub, 'traced in the last 15 min');
  assert.deepEqual(
    k.map((x) => x.cls),
    ['dash-kpi', 'dash-kpi', 'dash-kpi', 'dash-kpi', 'dash-kpi dash-kpi-tail'],
  );
  const empty = d.phpKpis({ totals: { requests: 0, p50: null } }, '5m');
  assert.equal(empty[1].value, '–');
  assert.equal(empty[4].value, '–');
});

test('the note under the picker says how the history is sampled', () => {
  assert.equal(d.sampleNote('varnish', null, ''), 'Reading the history…');
  assert.equal(d.sampleNote('varnish', null, 'boom'), 'Metrics did not load: boom');
  assert.equal(d.sampleNote('varnish', { step: 10, samples: 1 }), 'Collecting: Inspector samples every 10 s');
  assert.equal(d.sampleNote('varnish', { step: 10, samples: 90 }), 'One sample every 10 s · 90 in the window');
  assert.equal(d.sampleNote('php', { samples: 0 }), 'No traced web request in this window yet');
  assert.equal(d.sampleNote('php', { samples: 1 }), '1 traced request in the window, one point per minute');
});

test('the newest sample of any series pins the shared time axis', () => {
  assert.equal(d.endOf({ series: { a: pts([1, 2]), b: pts([1, 2, 3]) } }), T + 20);
  assert.equal(d.endOf({ series: {} }), undefined);
});

test('every chart in a dashboard has its own id and host, and fixed series slots', () => {
  for (const kind of Object.keys(d.SPECS)) {
    const ids = new Set();
    const refs = new Set();
    for (const c of d.SPECS[kind].charts) {
      assert.ok(!ids.has(c.id), kind + ' repeats ' + c.id);
      assert.ok(!refs.has(c.ref), kind + ' repeats ' + c.ref);
      ids.add(c.id);
      refs.add(c.ref);
      for (const form of typeof c.form === 'function' ? ['donut', 'timeseries'] : [c.form]) {
        assert.equal(typeof charts[form], 'function', kind + '/' + c.id + ' uses an unknown form');
        const opts = d.optsOf(c, null, form);
        assert.ok(opts.title && opts.title.endsWith('?'), kind + '/' + c.id + ' title asks a question');
        for (const s of opts.series || []) assert.ok(s.slot >= 1 && s.slot <= 8, kind + '/' + c.id + ' gives ' + s.key + ' a fixed slot');
      }
    }
  }
  const outcomes = d.SPECS.varnish.charts.find((c) => c.id === 'outcomes').opts.series;
  assert.deepEqual(
    outcomes.map((s) => [s.key, s.slot]),
    [
      ['hit', 1],
      ['miss', 2],
      ['pass', 3],
    ],
  );
  const queues = d.SPECS.rabbitmq.charts.find((c) => c.id === 'queues').opts.series;
  assert.deepEqual(
    queues.map((s) => [s.key, s.slot]),
    [
      ['ready', 1],
      ['unacked', 2],
    ],
  );
});

test('the Overview has a tile per service with the PHP tile second', () => {
  const overview = {
    adapters: [
      { kind: 'varnish', info: { present: true, engine: 'varnish', version: '8.0.2' }, health: { status: 'ok', cards: [{ label: 'Hit rate, last 5 min', value: '67%' }, { label: 'Cached pages', value: '121', sub: '1 MB of 256 MB' }, { label: 'Backend (nginx:80)', value: 'Healthy' }] } },
      { kind: 'redis', info: { present: false } },
    ],
  };
  const php = { totals: { requests: 12, p95: 900, p50: 200 } };
  const list = d.tiles(overview, { php }, (k) => k);
  assert.deepEqual(
    list.map((t) => t.kind),
    ['varnish', 'php', 'redis', 'search', 'rabbitmq'],
  );
  const v = list[0];
  assert.equal(v.value, '121 pages cached');
  assert.equal(v.sub, 'backend healthy · 1 MB of 256 MB');
  assert.equal(v.href, '#/varnish/health');
  assert.equal(v.linkText, 'View dashboard');
  assert.equal(list[1].value, 'p95 900 ms');
  assert.equal(list[1].href, '#/traces');
  assert.equal(list[2].present, false);
  assert.equal(list[2].hasChart, 'false');
  assert.equal(list[2].linkText, 'How to add it');
  assert.equal(list[3].value, 'not in this env');
});

test('tile charts read the hit ratio, the summed queue depth or the payload itself', () => {
  assert.equal(d.tileData('varnish', null), undefined);
  assert.deepEqual(d.tileData('varnish', { series: {}, totals: { hit_ratio: 0.8 }, latest: {} }), { value: 0.8 });
  assert.deepEqual(d.tileData('rabbitmq', { series: { 'ready.a': pts([2]) } }), [[T, 2]]);
  const m = { series: { ops: pts([1, 2]) } };
  assert.equal(d.tileData('redis', m), m);
  assert.deepEqual(d.tileData('php', { series: { p95: pts([5, 6, 7], T, 60) } }).series.p95, pts([5, 6], T, 60));
});

test('ranked rows keep seven names and fold the rest, the server tail included, into one grey row', () => {
  const rows = [];
  for (let i = 1; i <= 9; i++) rows.push({ key: 'k' + i, label: 'k' + i, value: i, other: false });
  rows.push({ key: 'other', label: 'other', value: 100, other: true });
  const out = d.rankRows(rows, 'Other indices');
  assert.deepEqual(
    out.map((r) => r.label),
    ['k9', 'k8', 'k7', 'k6', 'k5', 'k4', 'k3', 'Other indices'],
  );
  assert.equal(out[7].value, 103);
  assert.equal(out[7].other, true);
  assert.deepEqual(
    d.rankRows([{ key: 'a', label: 'a', value: 1, other: false }], 'Other').map((r) => r.label),
    ['a'],
  );
});

test('the PHP charts stop at the last complete minute', () => {
  const m = { totals: { requests: 3 }, series: { requests: pts([1, 2, 3], T, 60) } };
  const out = d.completeMinutes(m);
  assert.deepEqual(out.series.requests, pts([1, 2], T, 60));
  assert.equal(out.totals.requests, 3);
  assert.equal(m.series.requests.length, 3);
  const fresh = d.completeMinutes({ since: T + 110, series: { requests: pts([0, 2, 4, 5], T, 60) } });
  assert.deepEqual(fresh.series.requests, pts([4], T + 120, 60));
});

test('the Valkey hit ratio card follows the picked window and divides like the donut', () => {
  const m = {
    samples: 30,
    series: { hits: pts([10, 10]), misses: pts([0, 1]) },
    totals: { hits: 997, misses: 3, hit_ratio: 0.997 },
    latest: { hit_ratio: 0.9512 },
  };
  assert.deepEqual(d.redisHitCard(m, '15m'), {
    label: 'Hit ratio, last 15 min',
    match: 'hit ratio',
    value: '99.7%',
    sub: '997 hits · 3 misses · lifetime 95.1%',
    cls: '',
  });
  assert.equal(d.redisHitCard(m, '5m').label, 'Hit ratio, last 5 min');
  assert.equal(d.ratioDonut(m, 'hit_ratio', ['hits'], ['hits', 'misses']).value, 0.997);
  const idle = d.redisHitCard({ samples: 30, series: {}, totals: { hit_ratio: null }, latest: { hit_ratio: 0.5 } }, '60m');
  assert.equal(idle.value, '–');
  assert.equal(idle.sub, 'no key lookups in the last 60 min · lifetime 50.0%');
  assert.equal(d.redisHitCard({ samples: 1 }, '15m'), null);
  const low = d.redisHitCard({ samples: 30, totals: { hits: 1, misses: 3, hit_ratio: 0.25 }, latest: {} }, '15m');
  assert.equal(low.cls, 'tone-warn');
  assert.equal(low.sub, '1 hit · 3 misses');
});

test('the RabbitMQ publish rate card averages the picked window, like the flow chart', () => {
  const m = {
    samples: 30,
    step: 10,
    series: { publish: pts([1, 2, 3]), deliver: pts([1, 2, 3]), ack: pts([1, 1, 1]) },
    totals: { publish: 60, deliver: 54, ack: 30 },
    latest: {},
  };
  assert.deepEqual(d.rabbitPublishCard(m, '15m'), {
    label: 'Publish rate, last 15 min',
    match: 'publish rate',
    value: '2.0/s',
    sub: '60 published · 54 delivered · 30 acknowledged',
    cls: '',
  });
  const none = d.rabbitPublishCard({ samples: 30, step: 10, series: {}, totals: {}, latest: {} }, '5m');
  assert.equal(none.value, '–');
  assert.equal(none.sub, 'the broker reports no message stats yet');
  const quiet = d.rabbitPublishCard({ samples: 30, step: 10, series: { publish: pts([0, 0]) }, totals: { publish: 0 }, latest: {} }, '5m');
  assert.equal(quiet.value, '0.0/s');
  assert.equal(quiet.sub, 'nothing published in the last 5 min');
  assert.equal(d.rabbitPublishCard({ samples: 1 }, '5m'), null);
});

test('the OpenSearch searches card reads the same window as the query charts', () => {
  const m = {
    samples: 30,
    step: 10,
    series: { queries: pts([0.5, 1.5]) },
    totals: { queries: 20, query_ms: 0.2264 },
    latest: {},
  };
  assert.deepEqual(d.searchQueriesCard(m, '15m'), {
    label: 'Searches, last 15 min',
    match: 'searches',
    value: '1.0/s',
    sub: '20 queries · avg 0.23 ms per query',
    cls: '',
  });
  const idle = d.searchQueriesCard({ samples: 30, step: 10, series: { queries: pts([0, 0]) }, totals: { queries: 0, query_ms: null }, latest: {} }, '5m');
  assert.equal(idle.value, '0.0/s');
  assert.equal(idle.sub, 'no queries in the last 5 min');
});

test('every dashboard with a window card names it on its spec', () => {
  assert.equal(d.SPECS.varnish.windowCard, d.varnishHitCard);
  assert.equal(d.SPECS.redis.windowCard, d.redisHitCard);
  assert.equal(d.SPECS.rabbitmq.windowCard, d.rabbitPublishCard);
  assert.equal(d.SPECS.search.windowCard, d.searchQueriesCard);
  assert.equal(d.SPECS.php.windowCard, undefined);
  assert.equal(d.varnishHitCard({ samples: 30, totals: { hit_ratio: 0.5, hit: 1, miss: 1 }, latest: {} }, '5m').match, 'hit rate');
});

test('the Overview Valkey tile takes its hit ratio from the 15 minutes its chart shows', () => {
  const health = { status: 'ok', cards: [{ label: 'Hit ratio, last 5 min', value: '–', sub: 'no key lookups in the last 5 min' }, { label: 'Memory', value: '4.4 MB' }, { label: 'Evictions', value: '0' }] };
  const overview = { adapters: [{ kind: 'redis', info: { present: true, engine: 'valkey', version: '9.0.6' }, health }] };
  const redis = { samples: 60, series: { ops: pts([1, 2]) }, totals: { hits: 997, misses: 3, hit_ratio: 0.997 }, latest: { hit_ratio: 0.95 } };
  const tile = d.tiles(overview, { redis }, (k) => k).find((t) => t.kind === 'redis');
  assert.equal(tile.value, '99.7% hits');
  assert.equal(tile.sub, 'last 15 min · 4.4 MB · 0 evictions');
  const idle = d.tiles(overview, { redis: { samples: 60, series: {}, totals: { hit_ratio: null }, latest: {} } }, (k) => k).find((t) => t.kind === 'redis');
  assert.equal(idle.value, 'idle');
  assert.equal(idle.sub, 'no key lookups in 15 min · 4.4 MB');
  const early = d.tiles(overview, { redis: { samples: 1 } }, (k) => k).find((t) => t.kind === 'redis');
  assert.equal(early.value, 'idle');
  assert.equal(early.sub, 'no key lookups in the last 5 min · 4.4 MB');
});

test('a lifetime ratio outside 0 to 1 after a server restart is left out', () => {
  const m = { samples: 30, series: {}, totals: { hit_ratio: 0.9, hits: 9, misses: 1 }, latest: { hit_ratio: -0.4 } };
  assert.equal(d.redisHitCard(m, '15m').sub, '9 hits · 1 miss');
  assert.equal(d.ratioDonut(m, 'hit_ratio', ['hits'], ['hits', 'misses']).sub, '');
  assert.equal(d.ratioEmpty(Object.assign({}, m, { latest: { hit_ratio: 1.7 } }), 'hit_ratio', 'key lookups'), 'No key lookups in this window.');
});
