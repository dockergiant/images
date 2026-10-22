const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const core = require('../../clients/extension/lib/core.js');

const dir = path.join(__dirname, '../../clients/extension');

function entry(type, url, headers, extra) {
  return Object.assign(
    {
      _resourceType: type,
      startedDateTime: '2026-10-21T08:32:42.966Z',
      time: 81.6,
      request: { method: 'GET', url },
      response: { status: 200, headers: headers || [] },
    },
    extra,
  );
}

test('the manifest is MV3 with a DevTools page, options and https://*.test', () => {
  const m = JSON.parse(fs.readFileSync(path.join(dir, 'manifest.json'), 'utf8'));
  assert.equal(m.manifest_version, 3);
  assert.equal(m.devtools_page, 'devtools.html');
  assert.equal(m.options_page, 'options.html');
  assert.deepEqual(m.host_permissions, ['https://*.test/*']);
  assert.deepEqual(m.optional_host_permissions, ['<all_urls>']);
  assert.equal(m.background, undefined);
  for (const f of [m.devtools_page, m.options_page, 'panel.html']) assert.ok(fs.existsSync(path.join(dir, f)), f);
});

test('the extension pages load only local scripts and build no markup from strings', () => {
  for (const f of fs.readdirSync(dir, { recursive: true })) {
    const p = path.join(dir, f);
    if (fs.statSync(p).isDirectory()) continue;
    const src = fs.readFileSync(p, 'utf8');
    if (f.endsWith('.html')) {
      assert.doesNotMatch(src, /<script(?![^>]*\bsrc=)/, f + ' has an inline script');
      assert.doesNotMatch(src, /<script[^>]*src="(https?:)?\/\//, f + ' loads remote code');
      assert.doesNotMatch(src, /\sstyle=|\son[a-z]+=/, f + ' has an inline style or handler');
    }
    // inspectedWindow.eval runs a fixed expression in the inspected page, not
    // code in the extension, and is the only way to read its main frame URL.
    if (f.endsWith('.js')) assert.doesNotMatch(src, /innerHTML|outerHTML|insertAdjacentHTML|document\.write|(?<!inspectedWindow\.)\beval\(|new Function/, f);
  }
});

test('X-Varnish gives the client id first and the stored object second', () => {
  assert.deepEqual(core.parseXVarnish('426144 1442055'), ['426144', '1442055']);
  assert.deepEqual(core.parseXVarnish('32770'), ['32770']);
  assert.deepEqual(core.parseXVarnish(''), []);
  assert.deepEqual(core.parseXVarnish('abc 12'), ['12']);
  assert.equal(core.header([{ name: 'X-Varnish', value: '7' }], 'x-varnish'), '7');
});

test('only documents and XHR/fetch to the shop become rows', () => {
  assert.equal(core.tracked(entry('document', 'https://app.shop.test/women.html')), true);
  assert.equal(core.tracked(entry('xhr', 'https://app.shop.test/customer/section/load/')), true);
  assert.equal(core.tracked(entry('fetch', 'https://app.shop.test/graphql')), true);
  assert.equal(core.tracked(entry('script', 'https://app.shop.test/static/a.js')), false);
  assert.equal(core.tracked(entry('image', 'https://app.shop.test/media/a.jpg')), false);
  assert.equal(core.tracked(entry('fetch', 'https://inspector.shop.test/api/v1/page?url=x')), false);
  assert.equal(core.tracked(entry('document', 'data:text/html,hi')), false);
  assert.equal(core.tracked({}), false);
});

test('without _resourceType the response type decides', () => {
  const bare = (mime) => ({ startedDateTime: '2026-10-21T08:32:42.966Z', request: { method: 'GET', url: 'https://app.shop.test/x' }, response: { status: 200, headers: [], content: { mimeType: mime } } });
  assert.equal(core.tracked(bare('text/html; charset=UTF-8')), true);
  assert.equal(core.tracked(bare('application/json')), true);
  assert.equal(core.tracked(bare('text/css')), false);
  assert.equal(core.rowFromEntry(bare('text/html'), 1).type, 'document');
});

test('a row keeps what the join needs', () => {
  const r = core.rowFromEntry(entry('document', 'https://app.shop.test/women.html?p=2#top', [{ name: 'x-varnish', value: '426144 1442055' }, { name: 'X-Cache', value: 'hit' }]), 3);
  assert.equal(r.id, 'r3');
  assert.equal(r.path, '/women.html?p=2');
  assert.deepEqual(r.xids, ['426144', '1442055']);
  assert.equal(r.xCache, 'HIT');
  assert.equal(r.started, Date.parse('2026-10-21T08:32:42.966Z'));
  assert.equal(
    core.pageURL('https://inspector.shop.test', r),
    'https://inspector.shop.test/api/v1/page?url=' + encodeURIComponent('https://app.shop.test/women.html?p=2') + '&xid=426144%201442055&since=' + r.started,
  );
});

test('Inspector is looked for one label up first, then on the host itself', () => {
  assert.deepEqual(core.candidates('app.inspector-e2e.test'), ['https://inspector.inspector-e2e.test', 'https://inspector.app.inspector-e2e.test']);
  assert.deepEqual(core.candidates('shop.test'), ['https://inspector.shop.test']);
  assert.deepEqual(core.candidates('APP.shop.test:443'), ['https://inspector.shop.test', 'https://inspector.app.shop.test']);
  assert.deepEqual(core.candidates('inspector.shop.test'), []);
  assert.deepEqual(core.candidates(''), []);
});

test('the options override beats the cache, which beats discovery', () => {
  assert.deepEqual(core.plan({ host: 'app.shop.test', override: 'inspector.other.test/', cached: 'https://inspector.shop.test' }), { source: 'override', bases: ['https://inspector.other.test'] });
  assert.deepEqual(core.plan({ host: 'app.shop.test', cached: 'https://inspector.shop.test' }), { source: 'cache', bases: ['https://inspector.shop.test', 'https://inspector.app.shop.test'] });
  assert.deepEqual(core.plan({ host: 'app.shop.test' }), { source: 'discovery', bases: ['https://inspector.shop.test', 'https://inspector.app.shop.test'] });
  assert.equal(core.normalizeBase('javascript:alert(1)'), '');
  assert.equal(core.normalizeBase('ftp://x.test'), '');
});

test('discovery only accepts an Inspector that names its own domain', () => {
  assert.equal(core.confirms('https://inspector.shop.test', { api: 1, domain: 'shop.test' }), true);
  assert.equal(core.confirms('https://inspector.app.shop.test', { api: 1, domain: 'shop.test' }), false);
  assert.equal(core.confirms('https://inspector.shop.test', { api: 2, domain: 'shop.test' }), false);
  assert.equal(core.confirms('https://inspector.shop.test', null), false);
});

test('the response headers fill in HIT or MISS when Inspector has no Varnish row', () => {
  assert.equal(core.handlingOf({ xCache: 'HIT', xids: ['1', '2'] }, { handling: '' }), 'HIT');
  assert.equal(core.handlingOf({ xCache: '', xids: ['1', '2'] }, null), 'HIT');
  assert.equal(core.handlingOf({ xCache: 'MISS', xids: ['1'] }, null), 'MISS');
  assert.equal(core.handlingOf({ xCache: 'HIT', xids: [] }, { handling: 'pass' }), 'PASS');
  assert.equal(core.handlingOf({ xids: ['1'] }, null), '');
  assert.equal(core.toneOf('HIT'), 'hit');
  assert.equal(core.toneOf(''), 'none');
});

test('a list row summarises the page answer', () => {
  const page = {
    handling: 'MISS',
    trace: { duration: 961e6, route: 'catalog/category/view', counts: { sql: 56 }, times: { sql: 100, cache: 0, search: 0, http: 0, php: 861 } },
    hot: [{ rank: 1, spanId: 'a b', name: 'PDO.execute', resource: 'SELECT 1', kind: 'sql', selfMs: 45, share: 0.3, calls: 118, path: ['launch', 'block product_list'] }],
    repeats: [{ key: 'SELECT ?', kind: 'sql', count: 118, totalMs: 45.1, avgMs: 0.38, example: 'SELECT 1' }],
    repeatCount: 3,
    links: { bottlenecks: 'https://inspector.shop.test/#/traces/t1' },
  };
  const s = core.summary({ xids: ['1'] }, page);
  assert.equal(s.handling, 'MISS');
  assert.equal(s.tone, 'miss');
  assert.equal(s.php, '961 ms');
  assert.equal(s.sql, '56');
  assert.equal(s.nplus, 'N+1 ×3');
  assert.deepEqual(s.segments.map((x) => x.key), ['sql', 'php']);
  const hot = core.hotRows(page, 5);
  assert.equal(hot[0].href, 'https://inspector.shop.test/#/traces/t1?focus=a%20b');
  assert.equal(hot[0].path, 'launch › block product_list');
  assert.equal(hot[0].label, 'SELECT 1');
  assert.equal(hot[0].heat, 4);
  assert.equal(hot[0].bar, 30);
  assert.deepEqual(core.repeatRows(page)[0], { kind: 'sql', count: '×118', text: 'SELECT 1', total: '45 ms', avg: '0.4 ms' });
  assert.equal(core.summary({}, null).handling, '–');
  assert.equal(core.summary({}, null).php, '');
});

test('static and media files are not looked up', () => {
  assert.equal(core.isAsset('/static/frontend/Magento/luma/en_US/js-translation.json'), true);
  assert.equal(core.isAsset('/pub/media/catalog/a.jpg'), true);
  assert.equal(core.isAsset('/customer/section/load/'), false);
  assert.equal(core.isAsset('/staticpage.html'), false);
});

test('backoff matches the userscript', () => {
  assert.deepEqual(core.BACKOFF, [250, 500, 1000, 2000, 2000]);
});

test('only requests on the confirmed domain are sent to Inspector', () => {
  assert.equal(core.onDomain('https://app.shop.test/women.html', 'shop.test'), true);
  assert.equal(core.onDomain('https://shop.test/', 'shop.test'), true);
  assert.equal(core.onDomain('https://static.shop.test/x.js', 'SHOP.test'), true);
  assert.equal(core.onDomain('https://api.vendor.com/track?u=1', 'shop.test'), false);
  assert.equal(core.onDomain('https://evilshop.test/', 'shop.test'), false);
  assert.equal(core.onDomain('https://app.shop.test/', ''), false);
});

test('the panel finds Inspector from the inspected page itself, not from the HAR', () => {
  const src = fs.readFileSync(path.join(__dirname, '../../clients/extension/panel.js'), 'utf8');
  assert.match(src, /inspectedWindow\.eval\('location\.hostname'/);
  assert.doesNotMatch(src, /entries\[0\]/);
  assert.doesNotMatch(src, /row\.type === 'document'\) find/);
});
