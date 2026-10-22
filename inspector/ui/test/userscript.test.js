const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const us = require('../../clients/userscript/roll-inspector.user.js');

const source = fs.readFileSync(path.join(__dirname, '../../clients/userscript/roll-inspector.user.js'), 'utf8');

const page = {
  match: 'url',
  handling: 'MISS',
  request: { key: 'v:7', duration: 980e6 },
  trace: { id: 'abc', route: 'catalog/category/view', duration: 961e6, counts: { sql: 56, cache: 120 }, times: { sql: 120, cache: 40, search: 0, http: 0, php: 801 } },
  hot: [
    { rank: 1, spanId: 's1', name: 'magento.template.render', resource: 'Magento_Catalog::product/list.phtml', kind: 'template', selfMs: 312.4, share: 0.32, calls: 1 },
    { rank: 2, spanId: 's/2', name: 'PDO.execute', resource: 'SELECT `e`.* FROM `catalog_product_entity` AS `e` WHERE (`e`.`entity_id` IN (?))', kind: 'sql', selfMs: 45.1, share: 0.047, calls: 118 },
    { rank: 3, spanId: 's3', name: 'magento.block.render', resource: 'magento.block.render', kind: 'block', selfMs: 9, share: 0.009, calls: 1 },
    { rank: 4, spanId: 's4', name: 'x', resource: '', kind: 'php', selfMs: 1, share: 0.001, calls: 1 },
  ],
  repeats: [{ key: 'SELECT ?', kind: 'sql', count: 118, totalMs: 45.1, example: 'SELECT 1' }],
  repeatCount: 3,
  links: { bottlenecks: 'https://inspector.shop.test/#/traces/abc', trace: 'https://inspector.shop.test/#/traces/abc/waterfall', request: 'https://inspector.shop.test/#/overview?req=v%3A7' },
};

test('the template carries the domain placeholder Inspector fills in', () => {
  assert.equal(us.DOMAIN, '__ROLL_DOMAIN__');
  for (const line of ['// @match        https://*.__ROLL_DOMAIN__/*', '// @connect      inspector.__ROLL_DOMAIN__', '// @grant        GM_xmlhttpRequest', '// @downloadURL  https://inspector.__ROLL_DOMAIN__/userscript/roll-inspector.user.js', '// @updateURL    https://inspector.__ROLL_DOMAIN__/userscript/roll-inspector.user.js']) {
    assert.ok(source.includes(line), line);
  }
});

test('the userscript builds its DOM without markup strings', () => {
  assert.doesNotMatch(source, /innerHTML|outerHTML|insertAdjacentHTML|document\.write|\beval\(|new Function/);
});

test('it never runs on Inspector itself', () => {
  assert.equal(us.isInspectorHost('inspector.shop.test', 'shop.test'), true);
  assert.equal(us.isInspectorHost('INSPECTOR.shop.test', 'shop.test'), true);
  assert.equal(us.isInspectorHost('app.shop.test', 'shop.test'), false);
});

test('the page lookup drops the fragment and passes navigation start', () => {
  assert.equal(us.pageURL('https://inspector.shop.test', 'https://app.shop.test/women.html?p=2#reviews', 1792526556123.4), 'https://inspector.shop.test/api/v1/page?url=https%3A%2F%2Fapp.shop.test%2Fwomen.html%3Fp%3D2&since=1792526556123');
  assert.equal(us.pageURL('https://inspector.shop.test', 'https://app.shop.test/', 0), 'https://inspector.shop.test/api/v1/page?url=https%3A%2F%2Fapp.shop.test%2F');
});

test('polling backs off 250 ms, 500 ms, 1 s and 2 s over five tries', () => {
  assert.deepEqual(us.delays(5), [250, 500, 1000, 2000, 2000]);
  assert.equal(us.delays(5).length, 5);
});

test('the badge reads like the spec example', () => {
  assert.equal(us.badgeText(page), 'MISS · 961 ms · 56 SQL · N+1 ×3');
  assert.equal(us.badgeText({ handling: 'HIT', request: { duration: 2.5e6 } }), 'HIT · 2.5 ms');
  assert.equal(us.badgeText({ handling: '', trace: { duration: 1833.9e6, counts: {} }, repeats: [] }), 'PHP · 1.83 s');
  assert.equal(us.badgeText({ handling: 'hitpass', request: { duration: 0 } }), 'PASS');
});

test('the Varnish result picks the tone', () => {
  assert.equal(us.toneOf({ handling: 'HIT' }), 'hit');
  assert.equal(us.toneOf({ handling: 'MISS' }), 'miss');
  assert.equal(us.toneOf({ handling: 'HITMISS' }), 'miss');
  assert.equal(us.toneOf({ handling: 'PASS' }), 'pass');
  assert.equal(us.toneOf({ handling: '' }), 'none');
  assert.equal(us.toneOf(null), 'none');
});

test('the cost strip keeps category order and drops empty ones', () => {
  const segs = us.segments(page.trace.times);
  assert.deepEqual(segs.map((s) => s.key), ['sql', 'cache', 'php']);
  assert.ok(Math.abs(segs.reduce((n, s) => n + s.share, 0) - 1) < 1e-9);
  assert.equal(us.segmentTitle(segs[0]), 'SQL: 120 ms (12%)');
  assert.deepEqual(us.segments({}), []);
  assert.ok(us.segments({ sql: 0.001, php: 1000 })[0].width >= 0.6);
});

test('the top three items link to the bottleneck view with the span focused', () => {
  const items = us.topItems(page, 3);
  assert.equal(items.length, 3);
  assert.equal(items[0].href, 'https://inspector.shop.test/#/traces/abc?focus=s1');
  assert.equal(items[1].href, 'https://inspector.shop.test/#/traces/abc?focus=s%2F2');
  assert.equal(items[0].label, 'magento.template.render Magento_Catalog::product/list.phtml');
  assert.match(items[1].label, /^SELECT /);
  assert.equal(items[2].label, 'magento.block.render');
  assert.equal(items[1].calls, '×118');
  assert.equal(items[0].heat, 4);
  assert.equal(items[0].share, '32%');
  assert.deepEqual(us.topItems({ hot: [page.hot[0]] }, 3)[0].href, '');
});

test('hiding is remembered per host', () => {
  assert.equal(us.hideKey('App.Shop.test'), 'hidden:app.shop.test');
  assert.notEqual(us.hideKey('app.shop.test'), us.hideKey('admin.shop.test'));
});

test('durations read as ms below a second and s above', () => {
  assert.equal(us.formatMs(0.42), '0.4 ms');
  assert.equal(us.formatMs(961.2), '961 ms');
  assert.equal(us.formatMs(1833.9), '1.83 s');
  assert.equal(us.formatMs(12345), '12.3 s');
  assert.equal(us.formatMs(-1), '');
});
