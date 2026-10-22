const test = require('node:test');
const assert = require('node:assert/strict');
const overview = require('../src/components/overview.js');
const varnish = require('../src/components/varnish.js');
const redis = require('../src/components/redis.js');
const search = require('../src/components/search.js');
const rabbitmq = require('../src/components/rabbitmq.js');

test('laneBars turns span fractions into clamped percentages with a minimum width', () => {
  const bars = overview.laneBars([[0, 1], [0.5, 0.5], [0.999, 1]]);
  assert.deepEqual(bars[0], { left: 0, width: 100 });
  assert.ok(bars[1].width >= 0.6);
  assert.ok(bars[2].left + bars[2].width <= 100.0001);
});

test('laneRows orders lanes, hides spans of a none join and explains a probable join', () => {
  const rows = overview.laneRows(
    [
      { kind: 'redis', join: 'probable', reason: 'matched by pid', summary: '3 calls', spans: [[0.1, 0.2]] },
      { kind: 'varnish', join: 'exact', reason: '', summary: 'MISS', spans: [[0, 1]] },
      { kind: 'rabbitmq', join: 'none', reason: 'nothing published', summary: '', spans: [[0.2, 0.3]] },
    ],
    (k) => k,
  );
  assert.deepEqual(rows.map((r) => r.kind), ['varnish', 'redis', 'rabbitmq']);
  assert.equal(rows[0].caption, 'MISS');
  assert.equal(rows[1].caption, '3 calls · matched by pid');
  assert.equal(rows[2].bars.length, 0);
  assert.equal(rows[1].joinCls, 'text-probable');
});

test('the Valkey lane says whether trace spans or MONITOR fed it', () => {
  const rows = overview.laneRows(
    [
      { kind: 'redis', join: 'exact', reason: '12 cache spans in the trace', summary: '12 calls · 3 ms', source: 'trace', spans: [] },
      { kind: 'php', join: 'exact', reason: '', summary: 'x', spans: [] },
    ],
    (k) => k,
  );
  assert.equal(rows.find((r) => r.kind === 'redis').caption, '12 calls · 3 ms · from trace spans');
  assert.equal(rows.find((r) => r.kind === 'php').caption, 'x');
  const mon = overview.laneRows([{ kind: 'redis', join: 'probable', reason: 'by pid', summary: '2 reads', source: 'monitor', spans: [] }], (k) => k);
  assert.equal(mon[0].caption, '2 reads · by pid · from MONITOR');
});

test('a service Health card is swapped for the dashboard card of the picked window', () => {
  const cards = [
    { label: 'Hit rate, last 5 min', value: '100.0%', sub: '3 hits', cls: '' },
    { label: 'Cached pages', value: '12', sub: '', cls: '' },
  ];
  const win = { label: 'Hit rate, last 15 min', match: 'hit rate', value: '1.4%', sub: 'x', cls: '' };
  const out = overview.view.withWindowCard(cards, win);
  assert.deepEqual(out.map((c) => c.label), ['Hit rate, last 15 min', 'Cached pages']);
  assert.equal(out[0].value, '1.4%');
  assert.equal(overview.view.withWindowCard(cards, null), cards);
  const valkey = [{ label: 'Hit ratio, last 5 min', value: '–', sub: 'no key lookups in the last 5 min', cls: '' }];
  const hit = { label: 'Hit ratio, last 15 min', match: 'hit ratio', value: '99.7%', sub: '997 hits', cls: '' };
  assert.equal(overview.view.withWindowCard(valkey, hit)[0].value, '99.7%');
});

test('mergeRequest upserts by key and keeps newest first', () => {
  let list = [{ key: 'a', start: '2026-01-01T00:00:02Z' }, { key: 'b', start: '2026-01-01T00:00:01Z' }];
  list = overview.mergeRequest(list, { key: 'b', start: '2026-01-01T00:00:03Z', lanes: [] });
  assert.deepEqual(list.map((r) => r.key), ['b', 'a']);
  assert.equal(list.length, 2);
  assert.equal(overview.visibleRequests([{ handling: 'HIT' }, { handling: 'MISS' }, { handling: '' }], true).length, 2);
});

test('context warnings keep their fix command', () => {
  const rows = overview.warningRows([{ id: 'fpc-disabled', text: 'x', fix: 'roll magento cache:enable full_page', tone: 'warn' }]);
  assert.equal(rows[0].hasFix, true);
  assert.equal(rows[0].fix, 'roll magento cache:enable full_page');
});

test('service headlines read the right card per engine', () => {
  const h = overview.headline('rabbitmq', { status: 'warn', cards: [{ label: 'Queues', value: '23', sub: '1 hold messages', tone: 'ok' }, { label: 'Messages ready', value: '16', sub: '', tone: 'warn' }] });
  assert.equal(h.value, '16 messages waiting');
  assert.equal(h.sub, '1 hold messages');
  const v = overview.headline('varnish', { status: 'ok', cards: [{ label: 'Hit rate, last 5 min', value: '–', sub: '' }] });
  assert.equal(v.value, 'no requests yet');
  const r = overview.headline('redis', { status: 'ok', cards: [{ label: 'Hit ratio', value: '–', sub: 'no reads in the last 10s' }, { label: 'Memory', value: '4.4 MB' }, { label: 'Evictions', value: '0' }] });
  assert.equal(r.value, 'idle');
  assert.equal(r.sub, 'no reads in the last 10s · 4.4 MB');
});

test('varnish helpers build filter subs and escaped tag patterns', () => {
  assert.equal(varnish.filterSub('', ''), 'varnish:log');
  assert.equal(varnish.filterSub('/a', 'RespStatus == 500'), 'varnish:log;q=RespStatus == 500;url=/a');
  assert.equal(varnish.tagPattern('cat.p'), '((^|,)cat\\.p(,|$))');
});

test('redis helpers recognise URLs and describe entries', () => {
  assert.equal(redis.looksLikeUrl('https://shop.test/a.html'), true);
  assert.equal(redis.looksLikeUrl('/a.html'), true);
  assert.equal(redis.looksLikeUrl('CONFIG_1'), false);
  assert.equal(redis.monitorSub('1', ' ab '), 'redis:monitor;db=1;match=ab');
  assert.equal(redis.expiryText(0, true, -1), 'never expires');
  assert.equal(redis.expiryText(1000 + 120, false, 0, 1000 * 1000), 'expires in 2 min 0 s');
  const d = redis.detailOf({ key: 'k', ttl: 5, entry: { tags: ['A'], expire: 0, infinite: false, dataForm: 'json', dataSize: 10, preview: '{}' } });
  assert.deepEqual(d.tags, ['A']);
  assert.equal(d.body, '{}');
});

test('search helpers pick a default alias and explain the strongest field', () => {
  const list = [{ index: '.kibana', system: true, aliases: [] }, { index: 'm_product_1_v2', aliases: ['m_product_1'], label: { entity: 'product', title: 'Products · default' } }];
  assert.equal(search.defaultTarget(list), 'm_product_1');
  const exp = {
    value: 3,
    description: 'sum of:',
    details: [
      { value: 0.1, description: 'weight(_search:espresso in 3) [PerFieldSimilarity], result of:', details: [] },
      { value: 2.9, description: 'weight(name:espresso in 3) [PerFieldSimilarity], result of:', details: [{ value: 5, description: 'boost', details: [] }] },
    ],
  };
  assert.equal(search.whyOf(exp).text, 'name ×5 boost');
  const syn = { value: 1, description: 'weight(Synonym(name:simple name:simpl) in 7) [PerFieldSimilarity], result of:', details: [{ value: 5, description: 'boost', details: [] }] };
  assert.equal(search.whyOf(syn).text, 'name ×5 boost');
  assert.deepEqual(search.analyzeTokens({ detail: { tokenizer: { tokens: [{ token: 'Ab' }] }, tokenfilters: [{ tokens: [{ token: 'ab' }] }] } }), ['ab']);
});

test('rabbitmq helpers describe queues and the purge effect', () => {
  const rows = rabbitmq.queueRows([{ name: 'async.operations.all', ready: 148, unacked: 0, consumers: 0, published: 1, stuck: true }]);
  assert.equal(rows[0].what, 'Bulk and async API jobs');
  assert.equal(rows[0].consumers, 'none');
  assert.match(rabbitmq.purgeEffect({ ready: 148 }), /^148 waiting messages are deleted/);
  assert.match(rabbitmq.peekText({ exchange: '', routing_key: 'q', payload_bytes: 9, payload: '{"a":1}', payload_encoding: 'string' }), /"a": 1/);
});

test('visibleRequests hides static and media files unless asked', () => {
  const list = [
    { url: '/static/version1/frontend/x.js', handling: 'PASS' },
    { url: '/pub/media/catalog/a.jpg', handling: 'PASS' },
    { url: '/category-1.html', handling: 'MISS' },
    { url: '/media-kit.html', handling: 'MISS' },
  ];
  assert.deepEqual(overview.visibleRequests(list, false, true).map((r) => r.url), ['/category-1.html', '/media-kit.html']);
  assert.equal(overview.visibleRequests(list, false, false).length, 4);
  assert.equal(overview.visibleRequests(list, true).length, 4);
});

test('redis detailOf pretty-prints a JSON entry and keeps a cut one as it is', () => {
  const d = redis.detailOf({ key: 'k', entry: { dataForm: 'json', preview: '{"a":{"b":1}}' } });
  assert.equal(d.body, '{\n  "a": {\n    "b": 1\n  }\n}');
  const cut = redis.detailOf({ key: 'k', entry: { dataForm: 'json', preview: '{"a":{"b":', truncated: true } });
  assert.equal(cut.body, '{"a":{"b":');
});

test('redis keyRows hides tag sets and indexes unless raw keys are on', () => {
  const keys = [
    { key: '69d_:A', id: 'A', family: 'entry' },
    { key: 'cache:all_ids', family: 'index' },
    { key: 'cache:id_tags:69d_A', family: 'reverse-index' },
    { key: 'cache:tags:69d_CAT_C', family: 'tag' },
    { key: 'other', family: '' },
  ];
  assert.deepEqual(redis.keyRows(keys, '', false).map((r) => r.title), ['A', 'other']);
  assert.equal(redis.keyRows(keys, '', true).length, 5);
});

test('mergeRequest drops the rows the server says the request replaces', () => {
  const access = { key: 'n:1', source: 'nginx', method: 'GET', url: '/category-3.html?p=2', start: '2026-10-20T23:21:26.674Z' };
  const trace = { key: 't:1', source: 'trace', method: 'GET', url: '/category-3.html', start: '2026-10-20T23:21:26.150Z' };
  const other = { key: 'n:2', source: 'nginx', method: 'GET', url: '/other.html', start: '2026-10-20T23:21:26.600Z' };
  const v = { key: 'v:1', source: 'varnish', method: 'GET', url: '/category-3.html', start: '2026-10-20T23:21:26.141Z', duration: 532815000, replaces: ['n:1', 't:1'] };
  const list = overview.mergeRequest([access, trace, other], v);
  assert.deepEqual(list.map((r) => r.key).sort(), ['n:2', 'v:1']);
});

test('mergeRequest keeps an nginx row for the same URL that the server did not merge', () => {
  const access = { key: 'n:1', source: 'nginx', method: 'GET', url: '/category-3.html', start: '2026-10-20T23:21:26.674Z' };
  const v = { key: 'v:1', source: 'varnish', method: 'GET', url: '/category-3.html', start: '2026-10-20T23:21:26.141Z', duration: 532815000 };
  const list = overview.mergeRequest([access], v);
  assert.deepEqual(list.map((r) => r.key).sort(), ['n:1', 'v:1']);
});

test('redis tagCall names the tag index of the layout the db holds', () => {
  const types = {
    types: [
      { name: 'default', db: 0, layout: 'cm', prefix: '69d_' },
      { name: 'page_cache', db: 1, layout: 'symfony', prefix: '69d_' },
      { name: 'other', db: 2, layout: 'symfony', prefix: '' },
      { name: 'a', db: 3, layout: 'cm', prefix: 'a_' },
      { name: 'b', db: 3, layout: 'cm', prefix: 'b_' },
      { name: 'session', db: 4, layout: '' },
    ],
  };
  assert.equal(redis.tagCall(types, '0', 'CONFIG'), 'SMEMBERS zc:ti:69d_CONFIG, then UNLINK each entry');
  assert.equal(redis.tagCall(types, '1', 'CONFIG'), 'SMEMBERS cache:tags:69d_CONFIG, then UNLINK each entry');
  assert.equal(redis.tagCall(types, '0', 'cat_c_3'), 'SMEMBERS zc:ti:69d_cat_c_3 (or zc:ti:69d_CAT_C_3), then UNLINK each entry');
  const generic = 'SMEMBERS the tag index for CONFIG (cache:tags: or zc:ti:, by cache layout), then UNLINK each entry';
  assert.equal(redis.tagCall(types, '2', 'CONFIG'), generic);
  assert.equal(redis.tagCall(types, '3', 'CONFIG'), generic);
  assert.equal(redis.tagCall(types, '4', 'CONFIG'), generic);
  assert.equal(redis.tagCall(types, '9', 'CONFIG'), generic);
  assert.equal(redis.tagCall(null, '0', 'CONFIG'), generic);
});

test('redis detailOf calls a raw key outside a Magento layout a key', () => {
  const raw = redis.detailOf({ key: 'roll:e2e:hello', entry: { layout: '', dataForm: 'text', preview: 'world' } });
  assert.equal(raw.title, 'Key');
  const entry = redis.detailOf({ key: '69d_:A', family: 'entry', entry: { layout: 'symfony', dataForm: 'json', preview: '{}' } });
  assert.equal(entry.title, 'Cache entry');
});

test('varnish tagPreview counts the pages the log saw with a tag', () => {
  const res = { requests: [
    { method: 'GET', url: '/category-1.html', tags: ['cat_c_3'] },
    { method: 'GET', url: '/category-1.html', tags: ['cat_c_3'] },
    { method: 'GET', url: '/category-1/a.html?p=2', tags: ['cat_c_3'] },
    { method: 'PURGE', url: '/', tags: [] },
  ] };
  assert.equal(varnish.tagPreview(res), 'The Varnish log holds 3 requests for 2 pages with this tag, for example /category-1.html.');
  assert.equal(varnish.tagPreview({ requests: [] }), 'The Varnish log holds no request with this tag right now, so the purge may drop nothing.');
  assert.equal(varnish.tagPreview(null), '');
});

test('the redis view releases its browse effect when destroyed', () => {
  let factory = null;
  const released = [];
  const route = { tab: 'redis', section: 'browse' };
  const Alpine = {
    data: (name, f) => {
      factory = f;
    },
    effect: (fn) => {
      const h = () => fn();
      return h;
    },
    release: (h) => released.push(h),
    store: (n) => (n === 'app' ? { route } : { has: () => false }),
  };
  const I = { stream: { subscribe: () => () => {}, watch: () => () => {} }, present: () => true, adapter: () => null, client: { get: async () => ({}) } };
  redis.register(Alpine, I);
  const c = factory();
  c.startPolling = () => {};
  c.stopPolling = () => {};
  c.init();
  c.destroy();
  assert.equal(released.length, 1);
  c.destroy();
  assert.equal(released.length, 1);
});

test('overview picks the request named in the hash and draws its trace strip', () => {
  const list = [
    { key: 'v:1', traceId: '', handling: 'HIT' },
    { key: 'v:2', traceId: 't2', replaces: ['t:t2'] },
  ];
  assert.equal(overview.pickRequest(list, { req: 'v:2' }).key, 'v:2');
  assert.equal(overview.pickRequest(list, { req: 't:t2' }).key, 'v:2');
  assert.equal(overview.pickRequest(list, { trace: 't2' }).key, 'v:2');
  assert.equal(overview.pickRequest(list, { req: 'gone' }).key, 'v:1');
  assert.equal(overview.pickRequest([], {}), null);
  const sums = overview.summaryMap([{ id: 't2', duration: 100e6, times: { sql: 25, cache: 0, search: 0, http: 0, php: 75 } }]);
  const row = overview.requestRow(list[1], 'v:2', sums);
  assert.equal(row.hasStrip, true);
  assert.deepEqual(row.strip.map((s) => s.key), ['sql', 'php']);
  assert.equal(overview.requestRow(list[0], '', sums).hasStrip, false);
});

test('a query-only hash change does not refetch the browse data', async () => {
  for (const [mod, tab, load] of [
    [redis, 'redis', 'loadTypes'],
    [varnish, 'varnish', 'loadTab'],
    [search, 'search', 'loadIndices'],
    [rabbitmq, 'rabbitmq', 'loadTab'],
  ]) {
    let factory = null;
    let effect = null;
    const app = { route: { tab, section: 'browse', query: {} } };
    const Alpine = {
      data: (name, f) => {
        factory = f;
      },
      effect: (fn) => {
        effect = fn;
        fn();
        return fn;
      },
      release: () => {},
      store: (n) => (n === 'app' ? app : { has: () => false }),
    };
    const I = { stream: { subscribe: () => () => {}, watch: () => () => {} }, present: () => true, adapter: () => null, client: { get: async () => ({}) } };
    mod.register(Alpine, I);
    const c = factory();
    let loads = 0;
    c.startPolling = () => {};
    c.stopPolling = () => {};
    c.loadQueues = () => {};
    c[load] = () => {
      loads++;
    };
    c.init();
    const tick = () => new Promise((r) => setTimeout(r, 5));
    await tick();
    assert.equal(loads, 1, tab + ': first load');
    app.route = { tab, section: 'browse', query: { raw: '1' } };
    effect();
    await tick();
    assert.equal(loads, 1, tab + ': a query change refetched');
    app.route = { tab, section: 'health', query: {} };
    effect();
    app.route = { tab, section: 'browse', query: {} };
    effect();
    await tick();
    assert.equal(loads, 2, tab + ': coming back to browse did not reload');
    c.destroy();
  }
});
