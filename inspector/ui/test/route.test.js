const test = require('node:test');
const assert = require('node:assert/strict');
const route = require('../src/lib/route.js');

test('parse defaults to the overview', () => {
  assert.deepEqual(route.parse(''), { tab: 'overview', section: '', rest: '', query: {} });
  assert.deepEqual(route.parse('#'), { tab: 'overview', section: '', rest: '', query: {} });
  assert.deepEqual(route.parse('#/'), { tab: 'overview', section: '', rest: '', query: {} });
  assert.deepEqual(route.parse('#/nope/health'), { tab: 'overview', section: '', rest: '', query: {} });
});

test('parse gives service tabs a valid section', () => {
  assert.deepEqual(route.parse('#/varnish'), { tab: 'varnish', section: 'health', rest: '', query: {} });
  assert.deepEqual(route.parse('#/varnish/browse'), { tab: 'varnish', section: 'browse', rest: '', query: {} });
  assert.deepEqual(route.parse('#/redis/bogus'), { tab: 'redis', section: 'health', rest: '', query: {} });
  assert.deepEqual(route.parse('#/rabbitmq/actions/'), { tab: 'rabbitmq', section: 'actions', rest: '', query: {} });
});

test('parse keeps the remainder for deep links', () => {
  assert.equal(route.parse('#/redis/browse/page_cache/zc%3Ak%3A69d_A1').rest, 'page_cache/zc:k:69d_A1');
  assert.deepEqual(route.parse('#/traces/abc123'), { tab: 'traces', section: 'abc123', rest: '', query: {} });
  assert.deepEqual(route.parse('#/traces/abc123/flame'), { tab: 'traces', section: 'abc123', rest: 'flame', query: {} });
  assert.equal(route.parse('#/overview/%E0%A4').section, '%E0%A4');
  assert.deepEqual(route.parse('#/performance'), { tab: 'performance', section: '', rest: '', query: {} });
});

test('format builds a hash that parse reads back', () => {
  assert.equal(route.format({ tab: 'overview' }), '#/overview');
  assert.equal(route.format({ tab: 'varnish', section: 'live' }), '#/varnish/live');
  const h = route.format({ tab: 'redis', section: 'browse', rest: 'page_cache/zc:k:69d A1' });
  assert.equal(h, '#/redis/browse/page_cache/zc%3Ak%3A69d%20A1');
  assert.deepEqual(route.parse(h), { tab: 'redis', section: 'browse', rest: 'page_cache/zc:k:69d A1', query: {} });
});

test('the query string round-trips, sorted and without empty values', () => {
  const h = route.format({ tab: 'traces', section: 'abc', rest: 'waterfall', query: { focus: 'span 1', raw: '', q: 'a&b=c' } });
  assert.equal(h, '#/traces/abc/waterfall?focus=span%201&q=a%26b%3Dc');
  assert.deepEqual(route.parse(h).query, { focus: 'span 1', q: 'a&b=c' });
  assert.deepEqual(route.parseQuery('a=1&&b&c=x+y&=z'), { a: '1', b: '', c: 'x y' });
  assert.equal(route.formatQuery({}), '');
  assert.equal(route.parse('#/varnish/bogus?raw=1').query.raw, '1');
});

test('traceView names the traces screen a route shows', () => {
  const tv = (h) => route.traceView(route.parse(h));
  assert.deepEqual(tv('#/traces'), { page: 'hotspots', id: '', mode: '' });
  assert.deepEqual(tv('#/traces/requests'), { page: 'requests', id: '', mode: '' });
  assert.deepEqual(tv('#/traces/abc'), { page: 'trace', id: 'abc', mode: 'bottlenecks' });
  assert.deepEqual(tv('#/traces/abc/flame?focus=s1'), { page: 'trace', id: 'abc', mode: 'flame' });
  assert.deepEqual(tv('#/traces/abc/nonsense'), { page: 'trace', id: 'abc', mode: 'bottlenecks' });
  assert.deepEqual(tv('#/overview'), { page: 'hotspots', id: '', mode: '' });
});

test('tab metadata', () => {
  assert.deepEqual(route.TABS.map((t) => t.id), ['overview', 'varnish', 'redis', 'search', 'rabbitmq', 'traces', 'connect', 'performance']);
  assert.equal(route.kindOf('traces'), 'php');
  assert.equal(route.kindOf('overview'), '');
  assert.equal(route.isService('search'), true);
  assert.equal(route.isService('traces'), false);
  assert.deepEqual(route.SECTIONS, ['health', 'browse', 'live', 'actions']);
});
