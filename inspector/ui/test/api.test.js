const test = require('node:test');
const assert = require('node:assert/strict');
const api = require('../src/lib/api.js');

function jsonResponse(status, body) {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
}

function fakeFetch(handler) {
  const calls = [];
  const fn = async (url, init) => {
    calls.push({ url, init: init || {} });
    return handler(url, init || {}, calls.length);
  };
  fn.calls = calls;
  return fn;
}

test('query skips empty values and encodes the rest', () => {
  assert.equal(api.query({}), '');
  assert.equal(api.query({ a: 1, b: '', c: null, d: undefined, e: 'x y&z' }), '?a=1&e=x%20y%26z');
  assert.equal(api.query({ f: false, g: 0 }), '?f=false&g=0');
});

test('stringParams turns every value into a string and never returns null', () => {
  assert.deepEqual(api.stringParams(null), {});
  assert.deepEqual(api.stringParams(undefined), {});
  assert.deepEqual(api.stringParams({ db: 1, on: true, s: 'x', n: null, u: undefined }), { db: '1', on: 'true', s: 'x', n: '', u: '' });
});

test('canonicalSub sorts filters like the server stream key', () => {
  assert.equal(api.canonicalSub('varnish:log'), 'varnish:log');
  assert.equal(api.canonicalSub(' varnish:log;url=/a;db=1 '), 'varnish:log;db=1;url=/a');
  assert.equal(api.canonicalSub('redis:monitor;;db=0'), 'redis:monitor;db=0');
});

test('streamURL lists each real stream once in a stable order', () => {
  assert.equal(api.streamURL([]), '/api/stream');
  assert.equal(api.streamURL(['timeline']), '/api/stream?sub=timeline');
  assert.equal(
    api.streamURL(['redis:monitor', 'varnish:log;q=ReqURL ~ "^/x"', 'timeline', 'redis:monitor']),
    '/api/stream?sub=redis%3Amonitor&sub=timeline&sub=varnish%3Alog%3Bq%3DReqURL%20~%20%22%5E%2Fx%22',
  );
});

test('normalizeEvent reads wire events, hub errors and gaps', () => {
  const ev = api.normalizeEvent('varnish.req', JSON.stringify({ sub: 'varnish:log', type: 'varnish.req', time: 't', kind: 'varnish', data: { url: '/' } }), '12');
  assert.deepEqual(ev, { sub: 'varnish:log', type: 'varnish.req', time: 't', kind: 'varnish', data: { url: '/' }, id: '12' });
  const err = api.normalizeEvent('stream.error', JSON.stringify({ sub: 'search:capture', error: 'boom' }), '');
  assert.equal(err.type, 'stream.error');
  assert.equal(err.sub, 'search:capture');
  assert.equal(err.kind, 'search');
  assert.equal(err.data, 'boom');
  const gap = api.normalizeEvent('gap', '{}', '');
  assert.equal(gap.type, 'gap');
  assert.equal(api.normalizeEvent('trace', 'not json', ''), null);
});

test('errorMessage prefers the JSON error field', async () => {
  assert.equal(await api.errorMessage(jsonResponse(404, { error: 'no such API route' })), 'no such API route');
  assert.equal(await api.errorMessage(new Response('forbidden\n', { status: 403 })), 'forbidden');
  assert.equal(await api.errorMessage(new Response('', { status: 502, statusText: 'Bad Gateway' })), 'HTTP 502 Bad Gateway');
});

test('get sends no write headers and returns JSON', async () => {
  const fetch = fakeFetch(() => jsonResponse(200, { ok: 1 }));
  const c = api.createClient({ fetch, csrf: () => 'tok' });
  assert.deepEqual(await c.get('/api/overview', { query: { refresh: 1 } }), { ok: 1 });
  assert.equal(fetch.calls[0].url, '/api/overview?refresh=1');
  assert.equal(fetch.calls[0].init.method, 'GET');
  assert.equal(fetch.calls[0].init.headers['X-Inspector-Csrf'], undefined);
  assert.equal(fetch.calls[0].init.headers['Content-Type'], undefined);
});

test('post adds Content-Type and the csrf header', async () => {
  const fetch = fakeFetch(() => jsonResponse(200, { message: 'done' }));
  const c = api.createClient({ fetch, csrf: () => 'tok' });
  await c.post('/api/x', { a: 1 });
  const init = fetch.calls[0].init;
  assert.equal(init.method, 'POST');
  assert.equal(init.headers['Content-Type'], 'application/json');
  assert.equal(init.headers['X-Inspector-Csrf'], 'tok');
  assert.equal(init.body, '{"a":1}');
});

test('errors raise ApiError and reach onError unless quiet', async () => {
  const seen = [];
  const fetch = fakeFetch(() => jsonResponse(423, { error: 'Traefik listens beyond loopback' }));
  const c = api.createClient({ fetch, csrf: () => 't', onError: (e) => seen.push(e) });
  await assert.rejects(c.post('/api/x', {}), (e) => e instanceof api.ApiError && e.status === 423 && e.message === 'Traefik listens beyond loopback');
  assert.equal(seen.length, 1);
  await assert.rejects(c.get('/api/y', { quiet: true }));
  assert.equal(seen.length, 1);
});

test('a stale csrf token refreshes the session once and retries', async () => {
  let token = 'old';
  const fetch = fakeFetch((url, init, n) => {
    if (init.headers['X-Inspector-Csrf'] === 'old') return jsonResponse(403, { error: 'missing or wrong X-Inspector-Csrf header; reload the page' });
    return jsonResponse(200, { message: 'ok' });
  });
  let refreshed = 0;
  const c = api.createClient({ fetch, csrf: () => token, refresh: async () => { refreshed++; token = 'new'; } });
  assert.deepEqual(await c.post('/api/x', {}), { message: 'ok' });
  assert.equal(refreshed, 1);
  assert.equal(fetch.calls.length, 2);
});

test('action confirms first with the exact body, then sends the same params', async () => {
  const fetch = fakeFetch((url) => {
    if (url === '/api/confirm') return jsonResponse(200, { nonce: 'N1', expires: 'x' });
    return jsonResponse(200, { message: 'Purged 148 messages' });
  });
  const c = api.createClient({ fetch, csrf: () => 't' });
  const res = await c.action({ kind: 'rabbitmq', action: 'purge-queue', target: 'async.operations.all', params: { vhost: '/', n: 3 }, typed: 'async.operations.all', confirm: 'typed' });
  assert.deepEqual(res, { message: 'Purged 148 messages' });
  assert.equal(fetch.calls[0].url, '/api/confirm');
  assert.deepEqual(JSON.parse(fetch.calls[0].init.body), { kind: 'rabbitmq', action: 'purge-queue', target: 'async.operations.all', params: { n: '3', vhost: '/' }, typed: 'async.operations.all' });
  assert.equal(fetch.calls[1].url, '/api/actions/rabbitmq/purge-queue');
  assert.deepEqual(JSON.parse(fetch.calls[1].init.body), { target: 'async.operations.all', params: { n: '3', vhost: '/' }, nonce: 'N1' });
});

test('action without a confirm level skips the nonce', async () => {
  const fetch = fakeFetch(() => jsonResponse(200, { message: 'ok' }));
  const c = api.createClient({ fetch, csrf: () => 't' });
  await c.action({ kind: 'varnish', action: 'refresh', target: '', confirm: 'none' });
  assert.equal(fetch.calls.length, 1);
  assert.deepEqual(JSON.parse(fetch.calls[0].init.body), { target: '', params: {}, nonce: '' });
});

test('action encodes kind and name in the path', async () => {
  const fetch = fakeFetch(() => jsonResponse(200, { message: 'ok' }));
  const c = api.createClient({ fetch, csrf: () => 't' });
  await c.action({ kind: 'redis', action: 'a/b', target: 'x', confirm: 'none' });
  assert.equal(fetch.calls[0].url, '/api/actions/redis/a%2Fb');
});

class FakeEventSource {
  constructor(url) {
    this.url = url;
    this.readyState = 0;
    this.listeners = {};
    this.closed = false;
    FakeEventSource.all.push(this);
  }
  addEventListener(type, fn) {
    (this.listeners[type] = this.listeners[type] || []).push(fn);
  }
  close() {
    this.closed = true;
    this.readyState = 2;
  }
  fire(type, data, id) {
    for (const fn of this.listeners[type] || []) fn({ data: JSON.stringify(data), lastEventId: id || '' });
  }
}
FakeEventSource.all = [];

function tick() {
  return new Promise((r) => setTimeout(r, 5));
}

test('stream opens one EventSource for the union and routes events by sub', async () => {
  FakeEventSource.all = [];
  const s = api.createStream({ EventSource: FakeEventSource, delay: 0 });
  const a = [];
  const b = [];
  const offA = s.subscribe(['varnish:log'], (ev) => a.push(ev));
  const offB = s.subscribe(['timeline', 'redis:monitor'], (ev) => b.push(ev));
  await tick();
  assert.equal(FakeEventSource.all.length, 1);
  const es = FakeEventSource.all[0];
  assert.equal(es.url, '/api/stream?sub=redis%3Amonitor&sub=timeline&sub=varnish%3Alog');
  es.fire('varnish.req', { sub: 'varnish:log', type: 'varnish.req', kind: 'varnish', data: {} });
  es.fire('timeline', { sub: 'timeline', type: 'timeline', data: { key: 'k' } });
  es.fire('redis.cmd', { sub: 'redis:monitor', type: 'redis.cmd', kind: 'redis', data: {} });
  assert.equal(a.length, 1);
  assert.deepEqual(b.map((e) => e.type), ['timeline', 'redis.cmd']);
  offA();
  await tick();
  assert.equal(es.closed, true);
  assert.equal(FakeEventSource.all.length, 2);
  assert.equal(FakeEventSource.all[1].url, '/api/stream?sub=redis%3Amonitor&sub=timeline');
  offB();
  await tick();
  assert.equal(FakeEventSource.all[1].closed, true);
  assert.equal(FakeEventSource.all.length, 2);
});

test('stream does not reopen when the union stays the same', async () => {
  FakeEventSource.all = [];
  const s = api.createStream({ EventSource: FakeEventSource, delay: 0 });
  const off1 = s.subscribe(['varnish:log'], () => {});
  await tick();
  const off2 = s.subscribe(['varnish:log'], () => {});
  await tick();
  off1();
  await tick();
  assert.equal(FakeEventSource.all.length, 1);
  off2();
  await tick();
  assert.equal(FakeEventSource.all[0].closed, true);
});

test('gap events reach every consumer and errors reach the matching one', async () => {
  FakeEventSource.all = [];
  const s = api.createStream({ EventSource: FakeEventSource, delay: 0 });
  const a = [];
  const b = [];
  s.subscribe(['search:capture'], (ev) => a.push(ev.type));
  s.subscribe(['varnish:log'], (ev) => b.push(ev.type));
  await tick();
  const es = FakeEventSource.all[0];
  es.fire('gap', {});
  es.fire('stream.error', { sub: 'search:capture', error: 'nothing to capture' });
  assert.deepEqual(a, ['gap', 'stream.error']);
  assert.deepEqual(b, ['gap']);
});

test('a closed stream probes the URL for the reason', async () => {
  FakeEventSource.all = [];
  const statuses = [];
  const fetch = fakeFetch(() => jsonResponse(423, { error: 'capture streams change the service: LAN' }));
  const s = api.createStream({ EventSource: FakeEventSource, fetch, delay: 0, onStatus: (st) => statuses.push(st) });
  s.subscribe(['search:capture'], () => {});
  await tick();
  const es = FakeEventSource.all[0];
  es.readyState = 1;
  for (const fn of es.listeners.open || []) fn({});
  es.readyState = 2;
  for (const fn of es.listeners.error || []) fn({});
  await tick();
  assert.deepEqual(statuses.map((x) => x.state), ['connecting', 'open', 'closed']);
  assert.equal(statuses[2].error, 'capture streams change the service: LAN');
});

test('a closed stream behind a 502 reconnects with backoff', async () => {
  FakeEventSource.all = [];
  const statuses = [];
  const fetch = fakeFetch(() => jsonResponse(502, { error: 'bad gateway' }));
  const s = api.createStream({ EventSource: FakeEventSource, fetch, delay: 0, retryDelay: 1, onStatus: (st) => statuses.push(st) });
  s.subscribe(['timeline'], () => {});
  await tick();
  const es = FakeEventSource.all[0];
  es.readyState = 2;
  for (const fn of es.listeners.error || []) fn({});
  await tick();
  await tick();
  assert.ok(FakeEventSource.all.length >= 2);
  assert.ok(statuses.some((x) => x.state === 'reconnecting'));
  assert.ok(!statuses.some((x) => x.state === 'closed'));
});

test('a 400 probe keeps the stream closed', async () => {
  FakeEventSource.all = [];
  const statuses = [];
  const fetch = fakeFetch(() => jsonResponse(400, { error: 'bad sub' }));
  const s = api.createStream({ EventSource: FakeEventSource, fetch, delay: 0, retryDelay: 1, onStatus: (st) => statuses.push(st) });
  s.subscribe(['timeline'], () => {});
  await tick();
  const es = FakeEventSource.all[0];
  es.readyState = 2;
  for (const fn of es.listeners.error || []) fn({});
  await tick();
  await tick();
  assert.equal(FakeEventSource.all.length, 1);
  assert.equal(statuses[statuses.length - 1].state, 'closed');
});

test('the stream listens for benchmark progress', () => {
  assert.ok(api.EVENT_TYPES.includes('benchmark'), 'an event type missing here never reaches the page');
});
