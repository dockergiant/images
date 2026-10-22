const test = require('node:test');
const assert = require('node:assert/strict');
const wf = require('../src/lib/waterfall.js');

const MS = 1e6;
const BASE = Date.UTC(2026, 9, 20, 22, 32, 29);

function iso(offsetNs) {
  const total = Math.round(offsetNs);
  const ms = Math.floor(total / MS);
  const frac = total - ms * MS;
  const d = new Date(BASE + ms);
  const whole = d.toISOString().slice(0, 19);
  const nanos = String(d.getUTCMilliseconds() * MS + frac).padStart(9, '0');
  return whole + '.' + nanos + 'Z';
}

function span(id, parent, name, startMs, durMs, extra) {
  return Object.assign(
    {
      traceId: 't1',
      spanId: id,
      parentId: parent,
      name,
      resource: name,
      service: 'env',
      type: 'web',
      component: 'magento',
      start: iso(startMs * MS),
      duration: durMs * MS,
      error: false,
      attrs: {},
    },
    extra || {},
  );
}

function trace(spans, extra) {
  return Object.assign({ id: 't1', duration: spans[0].duration, spans, times: {}, counts: {}, nPlusOne: [] }, extra || {});
}

function sql(id, parent, startMs, durMs, resource) {
  return span(id, parent, 'PDOStatement.execute', startMs, durMs, { type: 'sql', component: 'sql', resource: resource || 'SELECT 1' });
}

test('offsetNs keeps nanosecond precision across seconds', () => {
  assert.equal(wf.offsetNs('2026-10-20T22:32:29.000000500Z', '2026-10-20T22:32:29.000000000Z'), 500);
  assert.equal(wf.offsetNs('2026-10-20T22:32:30.000000100Z', '2026-10-20T22:32:29.999999900Z'), 200);
  assert.equal(wf.offsetNs('2026-10-20T22:32:29.5Z', '2026-10-20T22:32:29Z'), 500 * MS);
  assert.equal(wf.offsetNs('garbage', '2026-10-20T22:32:29Z'), 0);
});

test('buildTree links parents, attaches orphans to the root and computes self time', () => {
  const t = trace([
    span('r', '', 'magento.request', 0, 100),
    span('a', 'r', 'magento.launch', 1, 60),
    sql('b', 'a', 2, 10),
    span('o', 'missing', 'magento.result.render', 70, 20),
  ]);
  const tree = wf.buildTree(t);
  assert.equal(tree.root.span.spanId, 'r');
  assert.deepEqual(tree.root.children.map((n) => n.span.spanId), ['a', 'o']);
  assert.equal(tree.root.self, 20 * MS);
  assert.equal(tree.nodes[1].self, 50 * MS);
  assert.equal(tree.nodes[2].start, 2 * MS);
  assert.equal(tree.total, 100 * MS);
  assert.equal(wf.buildTree(trace([span('r', '', 'x', 0, 1)], { spans: [] })), null);
});

test('rows hide plugin and observer spans but keep their children', () => {
  const t = trace([
    span('r', '', 'magento.request', 0, 100),
    span('p', 'r', 'magento.plugin.around', 1, 50),
    span('e', 'p', 'magento.event.execute', 2, 40, { resource: 'Vendor\\Observer' }),
    sql('q', 'e', 3, 5),
    span('c', 'r', 'magento.controller.execute', 60, 30),
  ]);
  const tree = wf.buildTree(t);
  assert.equal(tree.hidden, 2);
  const rows = wf.rows(tree, {});
  assert.deepEqual(rows.map((r) => r.key), ['r', 'q', 'c']);
  assert.equal(rows[1].depth, 1);
  assert.equal(rows[1].component, 'sql');

  const all = wf.rows(tree, { showHidden: true });
  assert.deepEqual(all.map((r) => r.key), ['r', 'p', 'e', 'q', 'c']);
  assert.deepEqual(all.map((r) => r.depth), [0, 1, 2, 3, 1]);
});

test('rows draw tree guides for the visible tree', () => {
  const t = trace([
    span('r', '', 'magento.request', 0, 100),
    span('a', 'r', 'magento.launch', 1, 60),
    span('a1', 'a', 'magento.dispatch', 2, 10),
    span('a2', 'a', 'magento.result.render', 20, 10),
    span('b', 'r', 'magento.response.send', 80, 10),
    span('b1', 'b', 'magento.block.render', 81, 1),
  ]);
  const rows = wf.rows(wf.buildTree(t), {});
  assert.deepEqual(
    rows.map((r) => r.prefix),
    ['', '├ ', '│ ├ ', '│ └ ', '└ ', '  └ '],
  );
  assert.equal(rows[0].name, 'magento.request');
  assert.equal(rows[1].name, 'launch');
});

test('rows fold runs of same-kind leaves and expand them on request', () => {
  const spans = [span('r', '', 'magento.request', 0, 100), span('c', 'r', 'magento.controller.execute', 1, 90)];
  for (let i = 0; i < 20; i++) {
    const name = i % 2 ? 'PDOStatement.execute' : 'PDO.prepare';
    spans.push(span('s' + i, 'c', name, 10 + i, 0.5, { type: 'sql', component: 'sql', resource: 'SELECT ' + (i % 3) }));
  }
  spans.push(span('x', 'c', 'magento.block.render', 50, 5));
  const tree = wf.buildTree(trace(spans));
  const rows = wf.rows(tree, {});
  assert.deepEqual(rows.map((r) => r.kind), ['span', 'span', 'fold', 'span']);
  const fold = rows[2];
  assert.equal(fold.name, 'PDO × 20');
  assert.equal(fold.count, 20);
  assert.equal(fold.duration, 20 * 0.5 * MS);
  assert.equal(fold.open, false);
  assert.equal(fold.toggle, true);
  assert.ok(Math.abs(fold.left - 0.1) < 1e-9);
  assert.ok(Math.abs(fold.width - 0.195) < 1e-9);
  assert.equal(fold.key, 'fold:c:s0');
  assert.equal(fold.members.length, 20);

  const open = wf.rows(tree, { expanded: new Set([fold.key]) });
  assert.equal(open.length, 4 + 20);
  assert.equal(open[2].open, true);
  assert.equal(open[3].depth, fold.depth + 1);
  assert.equal(open[3].key, 's0');

  const top = wf.foldSummary(tree, fold.members);
  assert.equal(top[0].count, 7);
  assert.equal(top.length, 3);
});

test('rows do not fold short runs or mixed kinds', () => {
  const spans = [span('r', '', 'magento.request', 0, 100)];
  for (let i = 0; i < 3; i++) spans.push(sql('s' + i, 'r', 10 + i, 0.5));
  spans.push(span('k', 'r', 'Redis.get', 20, 1, { type: 'redis', component: 'cache' }));
  const rows = wf.rows(wf.buildTree(trace(spans)), {});
  assert.deepEqual(rows.map((r) => r.kind), ['span', 'span', 'span', 'span', 'span']);
});

test('collapsing a span hides its subtree', () => {
  const t = trace([
    span('r', '', 'magento.request', 0, 100),
    span('a', 'r', 'magento.launch', 1, 60),
    span('a1', 'a', 'magento.dispatch', 2, 10),
  ]);
  const tree = wf.buildTree(t);
  const rows = wf.rows(tree, { collapsed: new Set(['a']) });
  assert.deepEqual(rows.map((r) => r.key), ['r', 'a']);
  assert.equal(rows[1].toggle, true);
  assert.equal(rows[1].open, false);
  assert.equal(rows[0].open, true);
});

test('describe picks the useful part of a span', () => {
  assert.equal(wf.describe({ name: 'Redis.mGet', resource: 'Redis.mGet', attrs: { 'redis.raw_command': 'mGet 69d_:RESOLVED_STORES' } }), '69d_:RESOLVED_STORES');
  assert.equal(wf.describe({ name: 'Redis.exec', resource: 'Redis.exec', attrs: { 'redis.raw_command': 'exec' } }), '');
  assert.equal(wf.describe({ name: 'curl_exec', resource: 'http://opensearch/magento2_product_1/_search', attrs: {} }), 'opensearch/magento2_product_1/_search');
  assert.equal(wf.describe({ name: 'PDO.query', resource: "SET SQL_MODE=''", attrs: {} }), "SET SQL_MODE=''");
  assert.equal(wf.describe({ name: 'magento.bootstrap', resource: 'magento.bootstrap', attrs: {} }), '');
  assert.equal(wf.describe({ name: 'PDO.query', resource: 'x'.repeat(500), attrs: {} }).length, 200);
});

test('byType sorts components by self time and keeps counted ones without time', () => {
  const t = trace([span('r', '', 'magento.request', 0, 100)], {
    duration: 100 * MS,
    times: { magento: 50 * MS, sql: 30 * MS, cache: 20 * MS },
    counts: { sql: 10, cache: 5, search: 0, queue: 1, http: 0, magento: 3 },
  });
  const rows = wf.byType(t);
  assert.deepEqual(rows.map((r) => r.component), ['magento', 'sql', 'cache', 'queue']);
  assert.equal(rows[1].count, 10);
  assert.equal(rows[1].share, 0.3);
  assert.equal(rows[3].self, 0);
});

test('blocks and topNames summarise the tree', () => {
  const t = trace([
    span('r', '', 'magento.request', 0, 100),
    span('b', 'r', 'magento.block.render', 10, 30),
    span('t', 'b', 'magento.template.render', 11, 20),
    sql('q', 't', 12, 5),
    sql('q2', 't', 20, 5),
  ]);
  const tree = wf.buildTree(t);
  assert.deepEqual(wf.blocks(tree), { blocks: 1, templates: 1, self: 10 * MS + 10 * MS });
  const top = wf.topNames(tree, 2);
  assert.equal(top[0].name, 'magento.request');
  assert.equal(top[1].name, 'PDOStatement.execute');
  const all = wf.topNames(tree, 10);
  const pdo = all.find((r) => r.name === 'PDOStatement.execute');
  assert.equal(pdo.count, 2);
  assert.equal(pdo.self, 10 * MS);
});

test('toFlame turns the span tree into icicle nodes positioned by start', () => {
  const t = trace([span('r', '', 'magento.request', 0, 100), span('a', 'r', 'magento.launch', 10, 60), sql('q', 'a', 20, 5)]);
  const root = wf.toFlame(wf.buildTree(t));
  assert.equal(root.name, 'magento.request');
  assert.equal(root.value, 100 * MS);
  assert.equal(root.start, 0);
  const a = root.children[0];
  assert.equal(a.start, 10 * MS);
  assert.equal(a.parent, root);
  assert.equal(a.children[0].component, 'sql');
  assert.equal(a.children[0].detail, 'SELECT 1');
});

test('attrList sorts attributes and drops internal dd keys', () => {
  assert.deepEqual(wf.attrList({ attrs: { b: '2', a: '1', '_dd.p.tid': 'x' } }), [
    { k: 'a', v: '1' },
    { k: 'b', v: '2' },
  ]);
});

test('self time subtracts the union of the children, clipped to the parent', () => {
  const tree = wf.buildTree(
    trace([
      span('r', '', 'magento.request', 0, 100),
      span('a', 'r', 'magento.launch', 10, 40),
      span('b', 'r', 'magento.dispatch', 30, 40),
      span('c', 'r', 'magento.late', 90, 30),
    ]),
  );
  // a and b overlap (10..70); c runs past the root and only 90..100 counts.
  assert.equal(tree.root.self, 30 * MS);
  assert.equal(tree.byKey.get('b').depth, 1);
});

test('kindOf follows the server classification', () => {
  assert.equal(wf.kindOf({ component: 'sql', name: 'PDO.query' }), 'sql');
  assert.equal(wf.kindOf({ component: 'magento', name: 'magento.plugin.around' }), 'plugin');
  assert.equal(wf.kindOf({ component: 'magento', name: 'magento.event.execute' }), 'observer');
  assert.equal(wf.kindOf({ component: 'magento', name: 'magento.event.dispatch' }), 'event');
  assert.equal(wf.kindOf({ component: 'magento', name: 'magento.block.render' }), 'block');
  assert.equal(wf.kindOf({ component: 'magento', name: 'magento.template.render' }), 'template');
  assert.equal(wf.kindOf({ component: 'queue', name: 'amqp.publish' }), 'php');
});

function filterTree() {
  return wf.buildTree(
    trace([
      span('r', '', 'magento.request', 0, 100),
      span('a', 'r', 'magento.launch', 1, 60),
      sql('q1', 'a', 2, 10, 'SELECT * FROM catalog_product_entity'),
      sql('q2', 'a', 20, 1, 'SELECT * FROM store'),
      span('b', 'r', 'magento.result.render', 70, 20),
      span('t', 'b', 'magento.template.render', 71, 15, { resource: 'Magento_Catalog::list.phtml' }),
    ]),
  );
}

test('keepSet keeps matches and their ancestors', () => {
  const tree = filterTree();
  assert.equal(wf.keepSet(tree, {}), null);
  const k = wf.keepSet(tree, { text: 'catalog_product' });
  assert.deepEqual(Array.from(k).map((n) => n.key).sort(), ['a', 'q1', 'r']);
  assert.equal(k.matches, 1);
  assert.deepEqual(wf.rows(tree, { keep: k }).map((r) => r.key), ['r', 'a', 'q1']);
  const slow = wf.keepSet(tree, { minSelf: 5 * MS });
  assert.ok(slow.has(tree.byKey.get('q1')) && !slow.has(tree.byKey.get('q2')));
  const none = wf.keepSet(tree, { text: 'nothing like this' });
  assert.equal(none.matches, 0);
  assert.deepEqual(wf.rows(tree, { keep: none }).map((r) => r.key), ['r']);
});

test('hot path follows the slowest child and intersects with a filter', () => {
  const tree = filterTree();
  assert.deepEqual(Array.from(wf.hotPath(tree)).map((n) => n.key), ['r', 'a', 'q1']);
  const k = wf.keepSet(tree, { hot: true, text: 'template' });
  assert.deepEqual(Array.from(k).map((n) => n.key), ['r']);
  assert.equal(k.matches, 0);
  assert.equal(wf.keepSet(tree, { hot: true, text: 'catalog' }).matches, 1);
  assert.equal(wf.keepSet(tree, { hot: true }).matches, 3);
});

test('reveal opens collapsed ancestors, hidden spans and folds', () => {
  const spans = [span('r', '', 'magento.request', 0, 100), span('c', 'r', 'magento.controller.execute', 1, 90), span('p', 'c', 'magento.plugin.around', 2, 80)];
  for (let i = 0; i < 6; i++) spans.push(sql('s' + i, 'p', 10 + i, 0.5));
  const tree = wf.buildTree(trace(spans));
  const opts = { collapsed: new Set(['c']) };
  const res = wf.reveal(tree, 's3', opts);
  assert.equal(opts.collapsed.has('c'), false);
  assert.ok(!opts.showHidden);
  assert.ok(res.index > 0);
  assert.equal(res.rows[res.index].key, 's3');
  assert.equal(opts.expanded.size, 1);
  const hidden = {};
  const ph = wf.reveal(tree, 'p', hidden);
  assert.equal(hidden.showHidden, true);
  assert.equal(ph.rows[ph.index].key, 'p');
  assert.equal(wf.reveal(tree, 'missing', {}).index, -1);
});

test('spanTable sorts, caps and filters the flat list', () => {
  const tree = filterTree();
  const all = wf.spanTable(tree, { sort: 'self', limit: 2 });
  assert.equal(all.total, 6);
  assert.equal(all.rows.length, 2);
  assert.ok(all.rows[0].self >= all.rows[1].self);
  const byName = wf.spanTable(tree, { sort: 'name' });
  assert.equal(byName.rows[0].name, 'PDOStatement.execute');
  assert.equal(wf.spanTable(tree, { sort: 'name', dir: 'asc' }).rows[0].name, 'magento.template.render');
  const k = wf.keepSet(tree, { text: 'store' });
  assert.deepEqual(wf.spanTable(tree, { keep: k }).rows.map((r) => r.key), ['q2']);
  assert.equal(wf.spanTable(tree, {}).rows.find((r) => r.key === 'q1').resource, 'SELECT * FROM catalog_product_entity');
});

test('childGroups and kindTotals group by span kind', () => {
  const tree = filterTree();
  const g = wf.childGroups(tree.byKey.get('a'));
  assert.deepEqual(g, [{ kind: 'sql', count: 2, total: 11 * MS }]);
  const kinds = wf.kindTotals(tree);
  assert.equal(kinds.find((k) => k.kind === 'template').count, 1);
});

test('costSegments keeps stack order and drops empty categories from the strip', () => {
  const segs = wf.costSegments({ php: 50, sql: 30, cache: 0, search: 20, http: 0 }, 100);
  assert.deepEqual(segs.map((s) => s.key), ['sql', 'cache', 'search', 'http', 'php']);
  assert.deepEqual(segs.filter((s) => s.shown).map((s) => s.key), ['sql', 'search', 'php']);
  assert.equal(segs[0].width, '30.000%');
  const st = wf.strip({ sql: 0.4, php: 99.6 }, 100);
  assert.equal(st.segments.length, 2);
  assert.equal(st.segments[0].cls, 'cost-seg cat-sql');
  assert.deepEqual(st.segments[0].style, { width: '0.400%' });
  assert.equal(st.segments[0].title, 'SQL · 0.40 ms · 0.4%');
  assert.equal(st.label, 'Where the time went: SQL 0.4%, PHP 100%');
  assert.equal(wf.strip(null, 0).segments.length, 0);
});

test('a 7,000 span trace builds and flattens', () => {
  const spans = [span('r', '', 'magento.request', 0, 2000)];
  let n = 0;
  for (let b = 0; b < 500; b++) {
    const bid = 'b' + b;
    spans.push(span(bid, 'r', 'magento.block.render', b * 3, 2.5));
    for (let p = 0; p < 5; p++) {
      const pid = bid + 'p' + p;
      spans.push(span(pid, bid, 'magento.plugin.around', b * 3 + p * 0.4, 0.3));
      spans.push(span(pid + 'e', pid, 'magento.event.execute', b * 3 + p * 0.4, 0.2));
      spans.push(sql(pid + 'q', pid + 'e', b * 3 + p * 0.4 + 0.01, 0.1));
      n += 3;
    }
  }
  assert.ok(spans.length > 7000);
  const tree = wf.buildTree(trace(spans));
  const rows = wf.rows(tree, {});
  const all = wf.rows(tree, { showHidden: true });
  wf.toFlame(tree);
  assert.equal(tree.hidden, 5000);
  assert.ok(rows.length < 1100, 'folded rows: ' + rows.length);
  assert.equal(all.length, spans.length);
});

test('7,000 span trace timing', { skip: !process.env.INSPECTOR_BENCH }, () => {
  const spans = [span('r', '', 'magento.request', 0, 2000)];
  let n = 0;
  for (let b = 0; b < 500; b++) {
    const bid = 'b' + b;
    spans.push(span(bid, 'r', 'magento.block.render', b * 3, 2.5));
    for (let p = 0; p < 5; p++) {
      const pid = bid + 'p' + p;
      spans.push(span(pid, bid, 'magento.plugin.around', b * 3 + p * 0.4, 0.3));
      spans.push(span(pid + 'e', pid, 'magento.event.execute', b * 3 + p * 0.4, 0.2));
      spans.push(sql(pid + 'q', pid + 'e', b * 3 + p * 0.4 + 0.01, 0.1));
      n += 3;
    }
  }
  assert.ok(spans.length > 7000);
  const t0 = process.hrtime.bigint();
  const tree = wf.buildTree(trace(spans));
  const rows = wf.rows(tree, {});
  const all = wf.rows(tree, { showHidden: true });
  wf.toFlame(tree);
  const took = Number(process.hrtime.bigint() - t0) / 1e6;
  assert.equal(tree.hidden, 5000);
  assert.ok(rows.length < 1100, 'folded rows: ' + rows.length);
  assert.equal(all.length, spans.length);
  assert.ok(took < 500, 'took ' + took + ' ms');
});
