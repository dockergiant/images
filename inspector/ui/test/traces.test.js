const test = require('node:test');
const assert = require('node:assert/strict');
const tr = require('../src/components/traces.js');
const route = require('../src/lib/route.js');
const wf = require('../src/lib/waterfall.js');

const hotspots = {
  traces: 4,
  since: '2026-10-21T08:00:00Z',
  routes: [
    { route: 'catalog/category/view', count: 3, p50Ms: 600, p95Ms: 960, maxMs: 960, times: { sql: 100, cache: 20, search: 200, http: 0, php: 300 }, worstId: 'w1' },
    { route: 'cms/index/index', count: 9, p50Ms: 120, p95Ms: 300, maxMs: 310, times: { sql: 10, cache: 5, search: 0, http: 0, php: 100 }, worstId: 'w2' },
  ],
  operations: [
    { key: 'SELECT ? FROM t WHERE id IN (?)', kind: 'sql', selfMs: 812, calls: 1460, traces: 3, share: 0.2, worstId: 'w1', worstSpanId: 's9', example: 'SELECT 1 FROM t WHERE id IN (1,2)' },
    { key: 'Vendor\\Plugin', kind: 'plugin', selfMs: 4, calls: 3, traces: 1, share: 0.001, worstId: 'w2', worstSpanId: '', example: '' },
  ],
  repeats: [{ key: 'SELECT ?', kind: 'sql', perRequest: 118.04, msPerRequest: 45.1, traces: 2, routes: ['a', 'b'], worstId: 'w1' }],
};

test('heat levels follow the share of time', () => {
  assert.equal(tr.heatOf(0.2), '4');
  assert.equal(tr.heatOf(0.1), '3');
  assert.equal(tr.heatOf(0.05), '2');
  assert.equal(tr.heatOf(0.001), '1');
  assert.equal(tr.heatOf(0), '0');
});

test('routes sort by p95 descending by default and by any column on request', () => {
  assert.deepEqual(tr.routeRows(hotspots).map((r) => r.key), ['catalog/category/view', 'cms/index/index']);
  assert.deepEqual(tr.routeRows(hotspots, 'count', 'desc').map((r) => r.key), ['cms/index/index', 'catalog/category/view']);
  assert.deepEqual(tr.routeRows(hotspots, 'p95', 'asc').map((r) => r.key), ['cms/index/index', 'catalog/category/view']);
  assert.deepEqual(tr.routeRows(hotspots, 'route').map((r) => r.key), ['catalog/category/view', 'cms/index/index']);
  const row = tr.routeRows(hotspots)[0];
  assert.equal(row.href, '#/traces/requests?route=catalog%2Fcategory%2Fview');
  assert.equal(row.p95, '960 ms');
  assert.deepEqual(row.strip.map((s) => s.key), ['sql', 'cache', 'search', 'php']);
});

test('sort headers flip the direction of the current column only', () => {
  const r = route.parse('#/traces');
  const hs = tr.sortHeaders([{ id: 'route', label: 'Route', num: false, cls: '' }, { id: 'p95', label: 'p95', num: true, cls: '' }], 'p95', 'desc', (id) => (id === 'route' ? 'asc' : 'desc'), (id, d) => tr.withQuery(r, { sort: id, dir: d }));
  assert.equal(hs[1].ariaSort, 'descending');
  assert.equal(hs[1].href, '#/traces?dir=asc&sort=p95');
  assert.equal(hs[0].ariaSort, 'none');
  assert.equal(hs[0].href, '#/traces?dir=asc&sort=route');
  assert.equal(hs[1].cls, 'num ');
});

test('operations link to the worst trace with the span focused', () => {
  const ops = tr.operationRows(hotspots);
  assert.equal(ops[0].href, '#/traces/w1/waterfall?focus=s9');
  assert.equal(ops[0].heat, '4');
  assert.equal(ops[0].rank, '1');
  assert.equal(ops[0].title, 'SELECT 1 FROM t WHERE id IN (1,2)');
  assert.equal(ops[1].href, '#/traces/w2');
  const reps = tr.repeatRows(hotspots);
  assert.equal(reps[0].perRequest, '×118');
  assert.equal(reps[0].routes[1].href, '#/traces/requests?route=b');
});

test('request filters read the hash and combine', () => {
  const items = [
    { id: 'a', route: 'x', url: 'http://s/one', duration: 50e6, status: 200, start: '2026-10-21T08:00:01Z' },
    { id: 'b', route: 'x', url: 'http://s/two', duration: 700e6, status: 500, start: '2026-10-21T08:00:02Z' },
    { id: 'c', route: 'y', url: 'http://s/two', duration: 1500e6, status: 200, start: '2026-10-21T08:00:00Z' },
  ];
  const f = tr.requestFilter(route.parse('#/traces/requests?route=x&min=500').query);
  assert.deepEqual(tr.filterRequests(items, f).map((s) => s.id), ['b']);
  assert.deepEqual(tr.filterRequests(items, tr.requestFilter({ url: 'TWO' })).map((s) => s.id), ['c', 'b']);
  assert.deepEqual(tr.filterRequests(items, tr.requestFilter({ errors: '1' })).map((s) => s.id), ['b']);
  assert.deepEqual(tr.filterRequests(items, tr.requestFilter({ sort: 'time' })).map((s) => s.id), ['b', 'a', 'c']);
  assert.deepEqual(tr.mergeSummary(items, { id: 'd', duration: 1, start: '2026-10-21T08:00:03Z' }, 'time').map((s) => s.id), ['d', 'b', 'a', 'c']);
});

test('a request row carries the cost strip and markers', () => {
  const row = tr.requestRow({ id: 'a', method: 'GET', route: 'r', url: 'http://shop.test/p.html?x=1', status: 503, duration: 200e6, counts: { sql: 1200 }, nPlusOne: true, hasProfile: false, times: { sql: 50, cache: 0, search: 0, http: 0, php: 150 }, start: '2026-10-21T08:00:00Z' });
  assert.equal(row.path, '/p.html?x=1');
  assert.equal(row.statusCls, 'tone-error');
  assert.equal(row.sql, '1,200');
  assert.equal(row.strip.length, 2);
  assert.deepEqual(row.strip[0].style, { width: '25.000%' });
  assert.equal(row.nPlusOne, true);
  const opts = tr.routeOptions('z', [{ route: 'b' }, { route: 'a' }, { route: 'b' }]);
  assert.deepEqual(opts.map((o) => o.value), ['', 'a', 'b', 'z']);
  assert.equal(opts[3].selected, true);
});

const analysis = {
  id: 't1',
  duration: 1000,
  categories: [],
  hot: [
    { rank: 1, spanId: 'b1', spanIds: ['b1'], name: 'magento.block.render', resource: 'product_list', selfMs: 300, totalMs: 500, share: 0.3, calls: 1, path: ['launch', 'dispatch'], kind: 'block' },
    { rank: 2, spanId: 'q1', spanIds: ['q1', 'q2'], name: 'PDO.query', resource: 'PDO.query', selfMs: 20, totalMs: 20, share: 0.02, calls: 2, path: [], kind: 'sql' },
  ],
  repeats: [{ key: 'SELECT ?', kind: 'sql', count: 12, totalMs: 6, avgMs: 0.5, spanIds: ['q1', 'q2'], example: 'SELECT 1' }],
  slowest: [
    { kind: 'sql', spanId: 'q2', resource: 'SELECT 2', ms: 4 },
    { kind: 'cache', spanId: 'k1', resource: 'get FOO', ms: 1 },
    { kind: 'sql', spanId: 'q1', resource: 'SELECT 1', ms: 2 },
  ],
  profile: false,
};

test('hot rows show the chain and mark the focused group', () => {
  const rows = tr.hotRows(analysis, 'q2');
  assert.equal(rows[0].name, 'block.render');
  assert.equal(rows[0].path, 'launch › dispatch');
  assert.equal(rows[0].heat, '4');
  assert.equal(rows[0].href, '#/traces/t1/waterfall?focus=b1');
  assert.equal(rows[1].resource, '');
  assert.equal(rows[1].calls, '×2');
  assert.equal(rows[1].focused, 'true');
  assert.equal(rows[0].focused, 'false');
});

test('repeats show count × average and point at the slowest member', () => {
  const MS = 1e6;
  const tree = { byKey: new Map([['q1', { key: 'q1', dur: 1 * MS }], ['q2', { key: 'q2', dur: 3 * MS }]]) };
  const r = tr.repeatList(analysis, tree, '')[0];
  assert.equal(r.formula, '12 × 0.50 ms = 6.0 ms');
  assert.equal(r.href, '#/traces/t1/waterfall?focus=q2');
  assert.equal(r.share, '0.6%');
  const groups = tr.slowestGroups(analysis, 'k1');
  assert.deepEqual(groups.map((g) => g.kind), ['sql', 'cache']);
  assert.equal(groups[0].items.length, 2);
  assert.equal(groups[1].items[0].focused, 'true');
});

test('trace tabs keep the focus and filters in the hash', () => {
  const r = route.parse('#/traces/t1/waterfall?focus=s1&q=pdo');
  const tabs = tr.tabList(r, 'waterfall');
  assert.equal(tabs[0].href, '#/traces/t1?focus=s1&q=pdo');
  assert.equal(tabs[1].current, 'page');
  assert.equal(tabs[4].href, '#/traces/t1/spans?focus=s1&q=pdo');
  assert.equal(tr.withQuery(r, { q: '', hot: '1' }), '#/traces/t1/waterfall?focus=s1&hot=1');
  assert.equal(route.traceView(route.parse(tabs[0].href)).mode, 'bottlenecks');
});

test('time and share text', () => {
  assert.equal(tr.msText(0.384), '0.38 ms');
  assert.equal(tr.msText(4.25), '4.3 ms');
  assert.equal(tr.msText(961.2), '961 ms');
  assert.equal(tr.msText(2167.79), '2.17 s');
  assert.equal(tr.pctText(0.0004), '<0.1%');
  assert.equal(tr.pctText(0.171), '17%');
  assert.equal(wf.shareLabel(0.05), '5.0%');
});

test('long ancestor chains keep the root and the nearest three', () => {
  assert.equal(tr.shortPath(['a', 'b', 'c', 'd', 'e']), 'a › b › c › d › e');
  assert.equal(tr.shortPath(['a', 'b', 'c', 'd', 'e', 'f', 'g']), 'a › … › e › f › g');
  assert.equal(tr.shortPath([]), '');
});

test('operations heat against the top row, not against all traced time', () => {
  const ops = tr.operationRows({ operations: [
    { key: 'a', kind: 'sql', selfMs: 200, share: 0.09 },
    { key: 'b', kind: 'sql', selfMs: 100, share: 0.045 },
    { key: 'c', kind: 'sql', selfMs: 10, share: 0.004 },
  ] });
  assert.deepEqual(ops.map((o) => o.heat), ['4', '3', '1']);
  assert.deepEqual(ops[1].shareStyle, { width: '50.00%' });
  assert.equal(ops[1].share, '4.5%');
});

test('flame frames heat by their own share of the request', () => {
  const root = { value: 1000 };
  assert.equal(tr.flameHeat({ self: 60 }, root), 'fill-php-bar stroke-heat-4 stroke-2');
  assert.equal(tr.flameHeat({ self: 25 }, root), 'fill-php-bar stroke-heat-3 stroke-1');
  assert.equal(tr.flameHeat({ self: 5 }, root), 'fill-php-bar');
  assert.equal(tr.flameHeat({ self: 5 }, null), 'fill-php-bar');
});
