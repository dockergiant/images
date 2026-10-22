const test = require('node:test');
const assert = require('node:assert/strict');
const palette = require('../src/components/palette.js');

const session = {
  env: 'shop',
  lan: false,
  adapters: [
    { kind: 'varnish', info: { present: true, engine: 'varnish' }, streams: [{ name: 'log', title: 'Varnish log', optIn: false }] },
    { kind: 'redis', info: { present: true, engine: 'valkey' }, streams: [{ name: 'monitor', title: 'Monitor', optIn: false }] },
    { kind: 'search', info: { present: true, engine: 'opensearch' }, streams: [{ name: 'capture', title: 'Capture all queries', optIn: true }] },
    { kind: 'rabbitmq', info: { present: false }, streams: [] },
  ],
};
const traces = [
  { id: 'aaaa1111bbbb2222cccc3333dddd4444', method: 'GET', url: 'http://shop.test/women/tops.html', route: 'catalog/category/view', duration: 961e6 },
  { id: 'eeee5555ffff6666aaaa7777bbbb8888', method: 'GET', url: 'http://shop.test/checkout/', route: 'checkout/index/index', duration: 420e6 },
  { id: 'cccc9999dddd0000eeee1111ffff2222', method: 'GET', url: 'http://shop.test/men.html', route: 'catalog/category/view', duration: 610e6 },
];
const timeline = [
  { key: 'k1', method: 'GET', url: 'http://shop.test/women/tops.html', handling: 'MISS', status: 200, traceId: traces[0].id },
  { key: 'k2', method: 'GET', url: 'http://shop.test/static/a.js', handling: 'HIT', status: 200 },
  { key: 'k3', method: 'GET', url: 'http://shop.test/women/tops.html', handling: 'HIT', status: 200 },
];
const indices = [
  { index: 'magento2_product_1_v4', aliases: ['magento2_product_1'], label: { title: 'Products · default' } },
  { index: '.plugins-ml-config', system: true, aliases: [] },
];

function items(extra) {
  return palette.buildItems(Object.assign({ session, traces, timeline, indices, drawer: { open: false, selected: [] }, raw: false, rawLabel: '' }, extra));
}

function labels(groups) {
  return groups.flatMap((g) => g.options.map((o) => o.label));
}

test('a substring at a word start beats a scattered match', () => {
  const a = palette.match('view', 'catalog/category/view');
  const b = palette.match('view', 'varnish request queue view-ish');
  const c = palette.match('cv', 'catalog/category/view');
  assert.ok(a && b && c);
  assert.ok(a.score > c.score);
  assert.deepEqual(a.hits, [17, 18, 19, 20]);
  assert.equal(palette.match('zzz', 'catalog/category/view'), null);
});

test('every token must match, in the label or the keywords', () => {
  assert.ok(palette.match('cat view', 'catalog/category/view'));
  assert.equal(palette.match('cat checkout', 'catalog/category/view'), null);
  const k = palette.match('aaaa1111', 'GET /women/tops.html', 'aaaa1111bbbb2222');
  assert.ok(k);
  assert.deepEqual(k.hits, []);
});

test('word-start subsequences land on the starts of words', () => {
  const m = palette.match('ccv', 'catalog/category/view');
  assert.deepEqual(m.hits, [0, 8, 17]);
  assert.equal(palette.match('cat', 'customer/account/login'), null, 'short tokens do not scatter inside words');
  assert.ok(palette.match('custlogin', 'customer/account/login'), 'longer tokens may');
});

test('segments split a label into highlighted runs', () => {
  assert.deepEqual(
    palette.segments('abcd', [1, 2]).map((s) => [s.text, s.hit]),
    [['a', false], ['bc', true], ['d', false]],
  );
  assert.deepEqual(palette.segments('ab', []).map((s) => s.text), ['ab']);
});

test('an empty query shows pages, recent requests and actions only', () => {
  const groups = palette.rank(items(), '');
  assert.deepEqual(groups.map((g) => g.id), ['pages', 'requests', 'actions']);
  const pages = groups[0].options.map((o) => o.label);
  assert.ok(pages.includes('Overview'));
  assert.ok(pages.includes('Valkey › Browse'));
  assert.ok(!pages.some((l) => l.startsWith('RabbitMQ')), 'absent services have no pages');
  assert.deepEqual(groups[1].options.map((o) => o.label), ['GET /women/tops.html', 'GET /static/a.js'], 'requests dedupe by URL');
  assert.ok(!labels(groups).includes('Find keys that start with this text'));
});

test('options are numbered across groups for the active index', () => {
  const groups = palette.rank(items(), '');
  const idx = groups.flatMap((g) => g.options.map((o) => o.index));
  assert.deepEqual(idx, idx.map((_, i) => i));
});

test('a route query puts the route first and links to the filtered request list', () => {
  const groups = palette.rank(items(), 'category view');
  assert.equal(groups[0].id, 'routes');
  const r = groups[0].options[0];
  assert.equal(r.label, 'catalog/category/view');
  assert.equal(r.hint, '2 traces');
  assert.equal(r.href, '#/traces/requests?route=catalog%2Fcategory%2Fview');
});

test('a trace id finds its trace and a URL finds the recent request', () => {
  let groups = palette.rank(items(), 'eeee5555');
  assert.equal(groups[0].id, 'traces');
  assert.equal(groups[0].options[0].href, '#/traces/eeee5555ffff6666aaaa7777bbbb8888');
  groups = palette.rank(items(), 'tops.html');
  const req = groups.find((g) => g.id === 'requests').options[0];
  assert.equal(req.href, '#/traces/' + traces[0].id, 'a traced request opens its trace');
  groups = palette.rank(items(), 'static a.js');
  assert.equal(groups.find((g) => g.id === 'requests').options[0].href, '#/overview?req=k2');
});

test('indices skip system indices and link to Browse', () => {
  const groups = palette.rank(items(), 'product');
  const ix = groups.find((g) => g.id === 'indices');
  assert.equal(ix.options.length, 1);
  assert.equal(ix.options[0].href, '#/search/browse?index=magento2_product_1_v4');
  assert.equal(palette.rank(items(), 'plugins-ml').find((g) => g.id === 'indices'), undefined);
});

test('actions: purge asks for a tag, live streams toggle, keys search is always offered', () => {
  let groups = palette.rank(items(), 'purge tag');
  const purge = groups.find((g) => g.id === 'actions').options[0];
  assert.equal(purge.action, 'purge-tag');
  assert.equal(purge.arg, true);
  groups = palette.rank(items({ drawer: { open: true, selected: ['varnish:log'] } }), 'varnish log');
  const live = labels(groups);
  assert.ok(live.includes('Stop Varnish log'));
  groups = palette.rank(items(), 'qqqqqq');
  assert.deepEqual(groups.map((g) => g.id), ['keys']);
  assert.equal(groups[0].options[0].action, 'keys');
});

test('argConfirm builds the same confirm call the views send', () => {
  const p = palette.argConfirm('purge-tag', 'cat_c.41', 0);
  assert.equal(p.kind, 'varnish');
  assert.equal(p.target, 'cat_c.41');
  assert.equal(p.call, 'PURGE / X-Magento-Tags-Pattern: ((^|,)cat_c\\.41(,|$))');
  assert.ok(palette.argConfirm('purge-tag', 'a b', 0).error);
  assert.ok(palette.argConfirm('ban-url', '(', 0).error);
  assert.deepEqual(palette.argConfirm('clean-tag', 'CAT_P_1', 1).params, { db: 1 });
});

test('key results link into Valkey Browse and keys are glob-escaped', () => {
  const groups = palette.rank(palette.keyItems(['69d_A', '69d_B'], 1), '', { all: true });
  assert.equal(groups[0].options.length, 2);
  assert.equal(groups[0].options[0].href, '#/redis/browse?db=1&key=69d_A');
  assert.equal(palette.globEscape('a*b?[c]\\'), 'a\\*b\\?\\[c\\]\\\\');
  assert.equal(palette.cacheDb({ cache: { default: { database: 3 } } }), 3);
  assert.equal(palette.cacheDb(null), 0);
});

test('asking to switch tracing off finds the Performance page first', () => {
  for (const q of ['perf', 'tracing off', 'profiling']) {
    const groups = palette.rank(items(), q);
    const top = groups[0].options[0];
    assert.equal(top.label, 'Performance', q);
    assert.equal(top.href, '#/performance', q);
  }
});

test('the palette announces the result count and the top result', () => {
  assert.equal(palette.announceText('', [{ label: 'Overview' }, { label: 'Varnish' }]), '2 results, Overview');
  assert.equal(palette.announceText('', [{ label: 'Overview' }]), '1 result, Overview');
  assert.equal(palette.announceText('Nothing matches "zz".', []), 'Nothing matches "zz".');
  assert.equal(palette.announceText('', []), 'No results');
});
