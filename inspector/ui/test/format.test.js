process.env.TZ = 'UTC';

const test = require('node:test');
const assert = require('node:assert/strict');
const fmt = require('../src/lib/format.js');

const MS = 1e6;

test('ms formats Go durations in nanoseconds', () => {
  assert.equal(fmt.ms(0), '0 ms');
  assert.equal(fmt.ms(420000), '<1 ms');
  assert.equal(fmt.ms(3 * MS), '3 ms');
  assert.equal(fmt.ms(812.4 * MS), '812 ms');
  assert.equal(fmt.ms(1234 * MS), '1.2 s');
  assert.equal(fmt.ms(59.94 * 1000 * MS), '59.9 s');
  assert.equal(fmt.ms(125 * 1000 * MS), '2 min 5 s');
  assert.equal(fmt.ms(null), '');
  assert.equal(fmt.ms(undefined), '');
});

test('millis formats plain milliseconds', () => {
  assert.equal(fmt.millis(58), '58 ms');
  assert.equal(fmt.millis(1500), '1.5 s');
});

test('bytes uses binary units with sensible precision', () => {
  assert.equal(fmt.bytes(0), '0 B');
  assert.equal(fmt.bytes(512), '512 B');
  assert.equal(fmt.bytes(1536), '1.5 KB');
  assert.equal(fmt.bytes(412 * 1024), '412 KB');
  assert.equal(fmt.bytes(118 * 1024 * 1024), '118 MB');
  assert.equal(fmt.bytes(3 * 1024 ** 3), '3.0 GB');
  assert.equal(fmt.bytes(-1), '');
  assert.equal(fmt.bytes('x'), '');
});

test('num groups thousands with commas', () => {
  assert.equal(fmt.num(0), '0');
  assert.equal(fmt.num(1204), '1,204');
  assert.equal(fmt.num(-1234567), '-1,234,567');
  assert.equal(fmt.num(12.5), '12.5');
  assert.equal(fmt.num(null), '');
});

test('pct turns a ratio into a percentage', () => {
  assert.equal(fmt.pct(0.87), '87%');
  assert.equal(fmt.pct(0.8749, 1), '87.5%');
  assert.equal(fmt.pct(1), '100%');
  assert.equal(fmt.pct(NaN), '');
});

test('clock shows wall time with milliseconds', () => {
  assert.equal(fmt.clock('2026-10-20T14:02:11.204Z'), '14:02:11.204');
  assert.equal(fmt.clock('2026-10-20T04:05:06Z'), '04:05:06.000');
  assert.equal(fmt.clock(''), '');
  assert.equal(fmt.clock('0001-01-01T00:00:00Z'), '');
  assert.equal(fmt.clock('garbage'), '');
});

test('ago describes a past time relative to now', () => {
  const now = Date.parse('2026-10-20T14:10:00Z');
  assert.equal(fmt.ago('2026-10-20T14:09:58Z', now), 'just now');
  assert.equal(fmt.ago('2026-10-20T14:09:30Z', now), '30 s ago');
  assert.equal(fmt.ago('2026-10-20T14:08:00Z', now), '2 min ago');
  assert.equal(fmt.ago('2026-10-20T11:10:00Z', now), '3 h ago');
  assert.equal(fmt.ago('2026-10-18T14:10:00Z', now), '2 d ago');
  assert.equal(fmt.ago('2026-10-20T14:10:30Z', now), 'just now');
  assert.equal(fmt.ago(1760961331, now), fmt.ago(new Date(1760961331 * 1000).toISOString(), now));
  assert.equal(fmt.ago('', now), '');
});

test('span renders seconds as a coarse length', () => {
  assert.equal(fmt.span(42), '42 s');
  assert.equal(fmt.span(86212), '23 h 56 min');
  assert.equal(fmt.span(150), '2 min 30 s');
  assert.equal(fmt.span(3 * 86400 + 7200), '3 d 2 h');
  assert.equal(fmt.span(-1), '');
});

test('truncate cuts long strings with an ellipsis', () => {
  assert.equal(fmt.truncate('abcdef', 4), 'abc…');
  assert.equal(fmt.truncate('abc', 4), 'abc');
  assert.equal(fmt.truncate(null, 4), '');
});

test('engine and version labels', () => {
  assert.equal(fmt.engineName('valkey'), 'Valkey');
  assert.equal(fmt.engineName('opensearch'), 'OpenSearch');
  assert.equal(fmt.engineName('elasticsearch'), 'Elasticsearch');
  assert.equal(fmt.engineName('dragonfly'), 'Dragonfly');
  assert.equal(fmt.engineName('RabbitMQ'), 'RabbitMQ');
  assert.equal(fmt.engineName('somethingelse'), 'somethingelse');
  assert.equal(fmt.shortVersion('8.0.1'), '8.0');
  assert.equal(fmt.shortVersion('9'), '9');
  assert.equal(fmt.shortVersion(''), '');
});

test('serviceLabel prefers the detected engine', () => {
  assert.equal(fmt.serviceLabel('redis', { engine: 'valkey' }), 'Valkey');
  assert.equal(fmt.serviceLabel('redis', null), 'Redis');
  assert.equal(fmt.serviceLabel('search', { engine: '' }), 'Search');
  assert.equal(fmt.serviceLabel('search', { engine: 'opensearch' }), 'OpenSearch');
  assert.equal(fmt.serviceLabel('varnish', { engine: 'Varnish' }), 'Varnish');
  assert.equal(fmt.serviceLabel('rabbitmq', {}), 'RabbitMQ');
  assert.equal(fmt.serviceLabel('php', {}), 'PHP');
});

test('streamLabel avoids repeating the service name', () => {
  assert.equal(fmt.streamLabel('Varnish', 'Varnish log'), 'Varnish log');
  assert.equal(fmt.streamLabel('Valkey', 'Monitor'), 'Valkey monitor');
  assert.equal(fmt.streamLabel('RabbitMQ', 'Publish firehose'), 'RabbitMQ publish firehose');
  assert.equal(fmt.streamLabel('OpenSearch', 'Top queries (Query Insights, top 10 per 5 minutes)'), 'OpenSearch top queries');
});

test('tone and kind classes', () => {
  assert.equal(fmt.toneClass('ok'), 'tone-ok');
  assert.equal(fmt.toneClass('warn'), 'tone-warn');
  assert.equal(fmt.toneClass('error'), 'tone-error');
  assert.equal(fmt.toneClass('info'), 'tone-info');
  assert.equal(fmt.toneClass('weird'), 'tone-info');
  assert.equal(fmt.kindClass('varnish'), 'kind-varnish');
  assert.equal(fmt.kindClass('php'), 'kind-php');
  assert.equal(fmt.kindClass('nope'), 'kind-none');
});

test('handlingTone maps Varnish handling to a tone', () => {
  assert.equal(fmt.handlingTone('hit'), 'ok');
  assert.equal(fmt.handlingTone('HIT'), 'ok');
  assert.equal(fmt.handlingTone('miss'), 'warn');
  assert.equal(fmt.handlingTone('pass'), 'info');
  assert.equal(fmt.handlingTone('synth'), 'info');
  assert.equal(fmt.handlingTone(''), 'info');
});

test('pretty prints values for raw panels', () => {
  assert.equal(fmt.pretty({ a: 1 }), '{\n  "a": 1\n}');
  assert.equal(fmt.pretty('text'), 'text');
  assert.equal(fmt.pretty(undefined), '');
});

test('eventRow renders a varnish request', () => {
  const row = fmt.eventRow({
    sub: 'varnish:log', type: 'varnish.req', kind: 'varnish', time: '2026-10-20T14:02:11.204Z',
    data: { vxid: '32781', method: 'GET', url: '/koffie.html', status: 200, handling: 'miss', duration: 812 * MS, tags: ['a', 'b'], start: '2026-10-20T14:02:11.204Z' },
  }, 7);
  assert.equal(row.id, 7);
  assert.equal(row.time, '14:02:11.204');
  assert.equal(row.kind, 'varnish');
  assert.equal(row.badge, 'MISS');
  assert.equal(row.tone, 'warn');
  assert.equal(row.text, 'GET /koffie.html 200 812 ms · tags 2');
  assert.equal(row.sub, 'varnish:log');
});

test('eventRow renders a redis command with folded repeats', () => {
  const row = fmt.eventRow({
    sub: 'redis:monitor', type: 'redis.cmd', kind: 'redis', time: '2026-10-20T14:02:11.231Z',
    data: { db: 1, cmd: 'HGETALL', args: ['zc:k:69d_A1F3C0'], container: 'php-fpm', folded: 3 },
  }, 1);
  assert.equal(row.badge, 'db1');
  assert.equal(row.text, 'HGETALL zc:k:69d_A1F3C0 ×3 · php-fpm');
});

test('eventRow truncates long redis arguments', () => {
  const row = fmt.eventRow({ type: 'redis.cmd', kind: 'redis', data: { db: 0, cmd: 'SET', args: ['k', 'x'.repeat(500)] } }, 1);
  assert.ok(row.text.length < 260);
  assert.ok(row.text.endsWith('…'));
});

test('eventRow renders a search query', () => {
  const row = fmt.eventRow({
    type: 'search.query', kind: 'search', time: '2026-10-20T14:02:06.233Z',
    data: { label: 'Products · nl', index: 'magento2_product_1_v3', phase: 'query', took: 41 * MS, summary: '"espresso" 31 hits' },
  }, 1);
  assert.equal(row.badge, 'query');
  assert.equal(row.text, 'Products · nl 41 ms "espresso" 31 hits');
});

test('eventRow renders a queue publish', () => {
  const row = fmt.eventRow({
    type: 'mq.pub', kind: 'rabbitmq',
    data: { exchange: 'magento', routingKey: 'async.operations.all', queues: ['async.operations.all'], count: 3, size: 120, source: 'firehose' },
  }, 1);
  assert.equal(row.badge, 'publish');
  assert.equal(row.text, 'magento → async.operations.all ×3 · 120 B');
  const delta = fmt.eventRow({ type: 'mq.pub', kind: 'rabbitmq', data: { exchange: '', routingKey: 'q', queues: ['q'], count: 1, source: 'delta' } }, 2);
  assert.equal(delta.text, '(default) → q');
});

test('eventRow renders traces, access lines, errors and unknown events', () => {
  assert.equal(fmt.eventRow({ type: 'trace', kind: 'php', data: { method: 'GET', url: '/x', duration: 790 * MS, status: 200, nPlusOne: true } }, 1).text, 'GET /x 200 790 ms · N+1');
  assert.equal(fmt.eventRow({ type: 'nginx.access', kind: 'php', data: { method: 'POST', url: '/c', status: 302 } }, 1).text, 'POST /c 302');
  const err = fmt.eventRow({ type: 'stream.error', kind: 'search', data: 'Query Insights needs 2.12' }, 1);
  assert.equal(err.tone, 'error');
  assert.equal(err.text, 'Query Insights needs 2.12');
  assert.equal(fmt.eventRow({ type: 'x.y', kind: 'varnish', data: { a: 1 } }, 1).text, '{"a":1}');
});

test('eventRow search text covers every visible field', () => {
  const row = fmt.eventRow({ type: 'varnish.req', kind: 'varnish', data: { method: 'GET', url: '/a', handling: 'hit', status: 200 } }, 1);
  assert.ok(row.search.includes('hit'));
  assert.ok(row.search.includes('/a'));
  assert.equal(row.search, row.search.toLowerCase());
});
