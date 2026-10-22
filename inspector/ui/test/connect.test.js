const test = require('node:test');
const assert = require('node:assert/strict');
const connect = require('../src/components/connect.js');
const route = require('../src/lib/route.js');
const rail = require('../src/lib/rail.js');
const palette = require('../src/components/palette.js');

const endpoints = [
  { method: 'GET', path: '/api/v1/', summary: 'index' },
  { method: 'GET', path: '/api/v1/openapi.json', summary: 'openapi' },
  { method: 'GET', path: '/api/v1/overview', summary: 'overview' },
  { method: 'GET', path: '/api/v1/page', summary: 'page' },
  { method: 'GET', path: '/api/v1/requests/{key}', summary: 'request' },
  { method: 'GET', path: '/api/v1/traces/{id}', summary: 'trace' },
  { method: 'GET', path: '/api/v1/hotspots', summary: 'hotspots' },
  { method: 'GET', path: '/api/v1/stream', summary: 'stream' },
  { method: 'GET', path: '/api/v1/varnish/info', summary: 'varnish info' },
  { method: 'GET', path: '/api/v1/redis/keys', summary: 'keys' },
  { method: 'GET', path: '/api/v1/varnish/bans', summary: 'bans' },
];

test('#/connect is the API & extensions page, reached from the rail footer and the palette', () => {
  const r = route.parse('#/connect?ep=%2Fapi%2Fv1%2Fpage');
  assert.equal(r.tab, 'connect');
  assert.equal(r.query.ep, '/api/v1/page');
  assert.equal(route.format({ tab: 'connect', query: { ep: '/api/v1/page' } }), '#/connect?ep=%2Fapi%2Fv1%2Fpage');
  assert.deepEqual(rail.crumbs({ route: r, session: null }).map((c) => c.label), ['API & extensions']);
  for (const q of ['userscript', 'extension', 'cors', 'api']) {
    const groups = palette.rank(palette.buildItems({ session: null, traces: [], timeline: [], indices: [], drawer: { open: false, selected: [] } }), q);
    const first = groups[0] && groups[0].options[0];
    assert.equal(first && first.href, '#/connect', q);
  }
});

test('endpoints group into Inspector, requests, traces, live, then each service in index order', () => {
  const groups = connect.groupEndpoints(endpoints);
  assert.deepEqual(groups.map((g) => g.id), ['inspector', 'requests', 'traces', 'live', 'varnish', 'redis']);
  assert.deepEqual(groups[0].items.map((e) => e.path), ['/api/v1/', '/api/v1/openapi.json', '/api/v1/overview']);
  assert.deepEqual(groups[4].items.map((e) => e.path), ['/api/v1/varnish/info', '/api/v1/varnish/bans']);
  assert.equal(groups[5].label, 'Valkey / Redis');
  assert.deepEqual(connect.groupEndpoints([]), []);
});

test('examples fill in values from this env', () => {
  const base = 'https://inspector.shop.test';
  const ctx = { domain: 'shop.test', pageUrl: 'https://app.shop.test/women.html?p=2', traceId: 'abc', requestKey: 'v:7' };
  assert.equal(connect.exampleURL(base, '/api/v1/page', ctx), base + '/api/v1/page?url=https%3A%2F%2Fapp.shop.test%2Fwomen.html%3Fp%3D2');
  assert.equal(connect.exampleURL(base, '/api/v1/traces/{id}', ctx), base + '/api/v1/traces/abc');
  assert.equal(connect.exampleURL(base, '/api/v1/requests/{key}', ctx), base + '/api/v1/requests/v%3A7');
  assert.equal(connect.exampleURL(base, '/api/v1/stream', ctx), base + '/api/v1/stream?sub=php%3Atraces');
  assert.equal(connect.exampleURL(base, '/api/v1/page', { domain: 'shop.test' }), base + '/api/v1/page?url=https%3A%2F%2Fapp.shop.test%2F');
  assert.ok(connect.needsValue(connect.exampleURL(base, '/api/v1/traces/{id}', {})));
  assert.ok(connect.needsValue(connect.exampleURL(base, '/api/v1/search/search', {})));
  assert.equal(connect.needsValue(connect.exampleURL(base, '/api/v1/hotspots', {})), false);
});

test('snippets quote safely and read the current tab for the page lookup', () => {
  assert.equal(connect.shellQuote("a'b"), "'a'\\''b'");
  assert.equal(connect.curlSnippet('https://x.test/api/v1/', '/api/v1/'), "curl -s 'https://x.test/api/v1/' | jq");
  assert.equal(connect.curlSnippet('https://x.test/api/v1/stream?sub=php%3Atraces', '/api/v1/stream'), "curl -sN 'https://x.test/api/v1/stream?sub=php%3Atraces'");
  assert.match(connect.fetchSnippet('https://x.test', 'https://x.test/api/v1/page?url=y', '/api/v1/page'), /encodeURIComponent\(location\.href\)/);
  assert.match(connect.fetchSnippet('https://x.test', 'https://x.test/api/v1/stream?sub=php%3Atraces', '/api/v1/stream'), /new EventSource\("https:\/\/x\.test\/api\/v1\/stream\?sub=php%3Atraces"\)/);
  assert.equal(connect.consoleSnippet('https://x.test'), 'fetch("https://x.test/api/v1/").then((r) => r.json()).then((j) => console.log(j.env, j.endpoints.length + " endpoints"))');
});

test('the probe reads Access-Control-Allow-Origin and says what it means', () => {
  const ok = { status: 200, acao: '*', body: { env: 'shop', endpoints: [1, 2] }, ms: 12.4 };
  assert.deepEqual(connect.probeRows(ok).map((r) => [r.label, r.value]), [
    ['Status', '200'],
    ['Access-Control-Allow-Origin', '*'],
    ['Env', 'shop'],
    ['Endpoints', '2'],
    ['Answered in', '12 ms'],
  ]);
  assert.equal(connect.probeVerdict(ok).tone, 'ok');
  assert.equal(connect.probeVerdict({ status: 200, acao: '' }).tone, 'warn');
  assert.equal(connect.probeVerdict({ status: 404, acao: '*' }).tone, 'error');
  assert.equal(connect.probeVerdict({ error: 'Failed to fetch' }).tone, 'error');
  assert.equal(connect.probeRows({ status: 404, acao: '', ms: -1 })[1].value, 'missing');
});
