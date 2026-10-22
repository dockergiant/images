const test = require('node:test');
const assert = require('node:assert/strict');
const route = require('../src/lib/route.js');
const rail = require('../src/lib/rail.js');

const session = {
  env: 'shop',
  lan: false,
  adapters: [
    { kind: 'varnish', info: { present: true, engine: 'varnish' }, streams: [{ name: 'log', title: 'Varnish log', optIn: false }] },
    { kind: 'redis', info: { present: true, engine: 'valkey' }, streams: [{ name: 'monitor', title: 'Monitor', optIn: false }] },
    {
      kind: 'search',
      info: { present: true, engine: 'opensearch' },
      streams: [
        { name: 'slowlog', title: 'Slowlog tail', optIn: false },
        { name: 'capture', title: 'Capture all queries (slowlog at 0 ms)', optIn: true },
      ],
    },
    { kind: 'rabbitmq', info: { present: false }, streams: [{ name: 'rates', title: 'Publish rates', optIn: false }] },
  ],
};
const overview = { adapters: [{ kind: 'varnish', health: { status: 'warn' } }, { kind: 'redis', health: { status: 'ok' } }] };

test('services follow the rail order with PHP traces second', () => {
  const list = rail.services({ route: route.parse('#/overview'), session, overview });
  assert.deepEqual(list.map((s) => s.id), ['overview', 'traces', 'varnish', 'redis', 'search', 'rabbitmq']);
  assert.equal(list[0].current, 'page');
  assert.equal(list.find((s) => s.id === 'redis').label, 'Valkey');
});

test('the status dot follows the overview health tone', () => {
  const list = rail.services({ route: route.parse('#/overview'), session, overview });
  const tone = (id) => list.find((s) => s.id === id).tone;
  assert.equal(tone('varnish'), 'warn');
  assert.equal(tone('redis'), 'ok');
  assert.equal(tone('search'), 'idle');
  assert.equal(tone('rabbitmq'), 'absent');
  assert.equal(tone('overview'), '');
});

test('only the current service shows its sections, and absent services never do', () => {
  let list = rail.services({ route: route.parse('#/varnish/browse'), session, overview });
  const v = list.find((s) => s.id === 'varnish');
  assert.deepEqual(v.sections.map((s) => [s.id, s.current]), [['health', null], ['browse', 'page'], ['live', null], ['actions', null]]);
  assert.equal(list.find((s) => s.id === 'redis').hasSections, false);
  list = rail.services({ route: route.parse('#/rabbitmq/health'), session, overview });
  assert.equal(list.find((s) => s.id === 'rabbitmq').hasSections, false);
  assert.equal(list.find((s) => s.id === 'rabbitmq').absent, 'true');
});

test('traces nest Hotspots and Requests, plus the open trace', () => {
  let t = rail.services({ route: route.parse('#/traces'), session, overview }).find((s) => s.id === 'traces');
  assert.deepEqual(t.sections.map((s) => [s.id, s.current]), [['hotspots', 'page'], ['requests', null]]);
  t = rail.services({ route: route.parse('#/traces/abcdef0123456789/flame'), session, overview }).find((s) => s.id === 'traces');
  assert.deepEqual(t.sections.map((s) => s.id), ['hotspots', 'requests', 'trace']);
  assert.equal(t.sections[2].label, 'Trace abcdef01…');
  assert.equal(t.sections[2].current, 'page');
});

test('live lists every stream of a present service with its on state', () => {
  const closed = rail.live({ session, drawer: { open: false, selected: ['varnish:log'] } });
  assert.deepEqual(closed.map((s) => s.sub), ['varnish:log', 'redis:monitor', 'search:slowlog', 'search:capture', 'php:traces']);
  assert.ok(closed.every((s) => s.on === 'false'));
  const open = rail.live({ session, drawer: { open: true, selected: ['varnish:log', 'varnish:log;url=/a'] } });
  assert.equal(open[0].on, 'true');
  const filtered = open.find((s) => s.filtered);
  assert.equal(filtered.sub, 'varnish:log;url=/a');
  assert.equal(filtered.label, 'url=/a');
  assert.equal(rail.liveCount({ session, drawer: { open: true, selected: ['varnish:log', 'varnish:log;url=/a'] } }), 2);
  assert.equal(open.find((s) => s.sub === 'search:capture').optIn, true);
});

test('live rows carry a short label for the narrow rail', () => {
  const list = rail.live({ session, drawer: { open: false, selected: [] } });
  const short = (sub) => list.find((s) => s.sub === sub).short;
  assert.equal(short('varnish:log'), 'Varnish log');
  assert.equal(short('redis:monitor'), 'Valkey monitor');
  assert.equal(short('search:capture'), 'Capture all queries');
  assert.equal(short('php:traces'), 'PHP traces');
  assert.equal(list.find((s) => s.sub === 'search:capture').label, 'OpenSearch capture all queries');
});

test('opt-in streams are disabled on the LAN unless already running', () => {
  const lan = Object.assign({}, session, { lan: true, lanReason: 'Traefik listens on 0.0.0.0' });
  const list = rail.live({ session: lan, drawer: { open: true, selected: [] } });
  const cap = list.find((s) => s.sub === 'search:capture');
  assert.equal(cap.disabled, true);
  assert.match(cap.title, /0\.0\.0\.0/);
  assert.equal(list.find((s) => s.sub === 'search:slowlog').disabled, false);
});

test('crumbs name service, section and item', () => {
  const c = rail.crumbs({ route: route.parse('#/redis/browse'), session, item: 'zc:k:69d_A1' });
  assert.deepEqual(c.map((x) => x.label), ['Valkey', 'Browse', 'zc:k:69d_A1']);
  assert.equal(c[2].current, 'page');
  assert.equal(c[2].mono, true);
  assert.equal(c[0].current, null);
  const t = rail.crumbs({ route: route.parse('#/traces/abcdef0123456789'), session });
  assert.deepEqual(t.map((x) => x.label), ['PHP traces', 'abcdef01…']);
  const tr = rail.crumbs({ route: route.parse('#/traces/abcdef0123456789'), session, item: 'catalog/category/view' });
  assert.equal(tr[1].label, 'catalog/category/view');
  assert.deepEqual(rail.crumbs({ route: route.parse('#/traces/requests'), session }).map((x) => x.label), ['PHP traces', 'Requests']);
  assert.deepEqual(rail.crumbs({ route: route.parse('#/connect'), session }).map((x) => x.label), ['API & extensions']);
  assert.deepEqual(rail.crumbs({ route: route.parse('#/performance'), session }).map((x) => x.label), ['Performance']);
  assert.ok(!rail.services({ route: route.parse('#/performance'), session }).some((s) => s.current), 'a footer page marks no service current');
});

test('a service with a current section is marked current, not page', () => {
  const list = rail.services({ route: route.parse('#/traces/requests'), session, overview });
  const t = list.find((s) => s.id === 'traces');
  assert.equal(t.current, 'true');
  assert.equal(t.active, true);
  assert.equal(t.sections.find((s) => s.id === 'requests').current, 'page');
  const o = rail.services({ route: route.parse('#/overview'), session, overview }).find((s) => s.id === 'overview');
  assert.equal(o.current, 'page');
  assert.equal(o.active, true);
  assert.equal(list.find((s) => s.id === 'redis').active, false);
});

test('the document title names the screen, then the env', () => {
  const c = rail.crumbs({ route: route.parse('#/traces/requests'), session });
  assert.equal(rail.pageTitle(c, 'inspector-e2e'), 'Requests · PHP traces · inspector-e2e');
  assert.equal(rail.pageTitle([], ''), 'Roll Inspector');
});
