const test = require('node:test');
const assert = require('node:assert/strict');

// A DOM just wide enough for the kit. It refuses markup injection and style
// attributes, so a test fails the same way the CSP would.
class FakeNode {
  constructor(doc, tag, ns) {
    this.ownerDocument = doc;
    this.tagName = tag;
    this.namespaceURI = ns || 'http://www.w3.org/1999/xhtml';
    this.attrs = new Map();
    this.children = [];
    this.parentNode = null;
    this.text = '';
    this.listeners = new Map();
    this.style = {};
    this.clientWidth = 0;
    const self = this;
    this.classList = {
      add(...c) {
        const s = self.classes();
        c.forEach((x) => s.add(x));
        self.attrs.set('class', [...s].join(' '));
      },
      remove(...c) {
        const s = self.classes();
        c.forEach((x) => s.delete(x));
        self.attrs.set('class', [...s].join(' '));
      },
      toggle(c, on) {
        const want = on === undefined ? !self.classes().has(c) : !!on;
        if (want) this.add(c);
        else this.remove(c);
        return want;
      },
      contains(c) {
        return self.classes().has(c);
      },
    };
  }
  classes() {
    return new Set((this.attrs.get('class') || '').split(/\s+/).filter(Boolean));
  }
  set className(v) {
    this.attrs.set('class', String(v));
  }
  get className() {
    return this.attrs.get('class') || '';
  }
  set innerHTML(v) {
    throw new Error('innerHTML is not allowed');
  }
  set outerHTML(v) {
    throw new Error('outerHTML is not allowed');
  }
  insertAdjacentHTML() {
    throw new Error('insertAdjacentHTML is not allowed');
  }
  setAttribute(k, v) {
    if (k === 'style') throw new Error('style attributes are blocked by the CSP');
    if (/^on/i.test(k)) throw new Error('inline handlers are blocked by the CSP');
    this.attrs.set(k, String(v));
  }
  getAttribute(k) {
    return this.attrs.has(k) ? this.attrs.get(k) : null;
  }
  appendChild(c) {
    if (c.parentNode) c.parentNode.removeChild(c);
    c.parentNode = this;
    this.children.push(c);
    return c;
  }
  removeChild(c) {
    const i = this.children.indexOf(c);
    if (i >= 0) this.children.splice(i, 1);
    c.parentNode = null;
    return c;
  }
  replaceChildren(...nodes) {
    for (const c of this.children) c.parentNode = null;
    this.children = [];
    nodes.forEach((n) => this.appendChild(n));
  }
  set textContent(v) {
    this.replaceChildren();
    this.text = String(v);
  }
  get textContent() {
    return this.text + this.children.map((c) => c.textContent).join('');
  }
  addEventListener(type, fn) {
    if (!this.listeners.has(type)) this.listeners.set(type, new Set());
    this.listeners.get(type).add(fn);
  }
  removeEventListener(type, fn) {
    if (this.listeners.has(type)) this.listeners.get(type).delete(fn);
  }
  listenerCount() {
    let n = 0;
    for (const s of this.listeners.values()) n += s.size;
    return n;
  }
  fire(type, evt) {
    for (const fn of [...(this.listeners.get(type) || [])]) fn(Object.assign({ type, preventDefault() {} }, evt));
  }
  getBoundingClientRect() {
    return { left: 0, top: 0, width: this.clientWidth, height: 300 };
  }
}

function makeDoc() {
  const doc = {
    createElement: (tag) => new FakeNode(doc, tag),
    createElementNS: (ns, tag) => new FakeNode(doc, tag, ns),
  };
  return doc;
}

function host(width) {
  const el = makeDoc().createElement('div');
  el.clientWidth = width === undefined ? 640 : width;
  return el;
}

function all(node, pred, out) {
  out = out || [];
  if (pred(node)) out.push(node);
  for (const c of node.children) all(c, pred, out);
  return out;
}

const byClass = (cls) => (n) => n.classList.contains(cls);
const byTag = (tag) => (n) => n.tagName === tag;

function everyNode(node, fn) {
  fn(node);
  node.children.forEach((c) => everyNode(c, fn));
}

const observers = [];
global.ResizeObserver = class {
  constructor(cb) {
    this.cb = cb;
    this.targets = [];
    this.disconnected = false;
    observers.push(this);
  }
  observe(t) {
    this.targets.push(t);
  }
  disconnect() {
    this.disconnected = true;
  }
};

const charts = require('../src/lib/charts.js');

function series(n, start, step, fn) {
  const out = [];
  for (let i = 0; i < n; i++) out.push([start + i * step, fn(i)]);
  return out;
}

const T0 = 1792000000;

test('niceTicks start at zero and end on a clean step above the max', () => {
  assert.deepEqual(charts.niceTicks(87, 4), [0, 25, 50, 75, 100]);
  assert.deepEqual(charts.niceTicks(0, 4), [0, 0.25, 0.5, 0.75, 1]);
  assert.deepEqual(charts.niceTicks(1200, 3), [0, 500, 1000, 1500]);
  const b = charts.yTicks('bytes', 300 * 1024 * 1024, 3);
  assert.equal(b[0], 0);
  assert.ok(b.every((t) => (t / (1024 * 1024)) % 50 === 0));
  assert.deepEqual(charts.yTicks('pct', 0.93, 4), [0, 0.25, 0.5, 0.75, 1]);
});

test('compact numbers, durations and bytes read like the axis should', () => {
  assert.equal(charts.compact(0), '0');
  assert.equal(charts.compact(1284), '1.3K');
  assert.equal(charts.compact(12900), '13K');
  assert.equal(charts.compact(2500000), '2.5M');
  assert.equal(charts.compact(0.38), '0.38');
  assert.equal(charts.duration(961), '961 ms');
  assert.equal(charts.duration(1833.9), '1.8 s');
  assert.equal(charts.bytes(256 * 1024 * 1024), '256 MB');
  assert.equal(charts.formatValue('rate', 42), '42/s');
  assert.equal(charts.formatValue('pct', 0.924), '92.4%');
  assert.equal(charts.formatValue('pct', null), '–');
});

test('timeTicks pick a step that fits the width and stay inside the window', () => {
  const ticks = charts.timeTicks(T0 - 900, T0, 600);
  assert.ok(ticks.length >= 2 && ticks.length <= 8);
  assert.ok(ticks.every((t) => t.t >= T0 - 900 && t.t <= T0));
  const step = ticks[1].t - ticks[0].t;
  assert.equal(step % 60, 0);
  assert.ok(charts.timeTicks(T0 - 300, T0, 200).length <= 3);
});

test('prepTimeseries joins series on one axis and cuts it to the window', () => {
  const data = {
    step: 10,
    series: {
      hit: series(100, T0 - 990, 10, (i) => i),
      miss: series(5, T0 - 40, 10, () => 2),
    },
  };
  const defs = [{ key: 'hit' }, { key: 'miss' }];
  const p = charts.prepTimeseries(data, defs, { window: '5m' });
  assert.equal(p.end, T0);
  assert.equal(p.start, T0 - 300);
  assert.equal(p.times[0], T0 - 300);
  assert.equal(p.times.length, 31);
  assert.equal(p.values[1][0], null);
  assert.equal(p.values[1][30], 2);
});

test('stackValues accumulates and treats gaps as zero', () => {
  const s = charts.stackValues([
    [1, 2, null],
    [3, null, 4],
  ]);
  assert.deepEqual(s.lower[1], [1, 2, 0]);
  assert.deepEqual(s.upper[1], [4, 2, 4]);
  assert.deepEqual(s.totals, [4, 2, 4]);
});

test('bar paths are square at the baseline and rounded at the data end', () => {
  const h = charts.barPathH(0, 0, 100, 10);
  assert.match(h, /^M0 0H96A4 4 0 0 1 100 4V6A4 4 0 0 1 96 10H0Z$/);
  const v = charts.barPathV(0, 10, 20, 50);
  assert.ok(v.startsWith('M0 60V14A4 4'));
  assert.ok(v.endsWith('V60Z'));
  assert.equal(charts.barPathH(0, 0, 0, 10), '');
  assert.match(charts.barPathH(0, 0, 3, 10), /A3 3/);
});

test('linePath breaks at gaps instead of drawing through them', () => {
  assert.equal(charts.linePath([[0, 1], [1, 2], [2, null], [3, 4], [4, 5]]), 'M0 1L1 2M3 4L4 5');
});

test('placeLabels drops colliding labels rather than nudging them', () => {
  const kept = charts.placeLabels([{ y: 50, priority: 3 }, { y: 56, priority: 9 }, { y: 120, priority: 1 }], 14);
  assert.deepEqual(kept, [1, 2]);
});

test('foldRows ranks, keeps limit - 1 and folds the tail into Other', () => {
  const rows = Array.from({ length: 11 }, (_, i) => ({ key: 'q' + i, label: 'q' + i, value: i }));
  const out = charts.foldRows(rows, 8);
  assert.equal(out.length, 8);
  assert.equal(out[0].value, 10);
  assert.ok(out[7].other);
  assert.equal(out[7].value, 0 + 1 + 2 + 3);
});

test('ringDash clamps and flags the full and empty ends', () => {
  assert.deepEqual(charts.ringDash(1, 100), { len: 100, full: true, none: false });
  assert.deepEqual(charts.ringDash(0, 100), { len: 0, full: false, none: true });
  assert.equal(charts.ringDash(1.4, 100).len, 100);
  assert.equal(charts.ringDash(0.25, 100).len, 25);
});

test('slot follows the series, not its position, so a filter never repaints', () => {
  const el = host();
  const defs = [
    { key: 'hit', label: 'hit', slot: 1 },
    { key: 'miss', label: 'miss', slot: 2 },
    { key: 'pass', label: 'pass', slot: 3 },
  ];
  const data = { series: { hit: series(30, T0 - 290, 10, () => 40), miss: series(30, T0 - 290, 10, () => 4), pass: series(30, T0 - 290, 10, () => 1) } };
  const c = charts.timeseries(el, { series: defs, unit: 'rate', mode: 'stack', window: '5m' });
  c.update(data);
  const filteredEl = host();
  const filtered = charts.timeseries(filteredEl, { series: [defs[0], defs[2]], unit: 'rate', mode: 'stack', window: '5m' });
  filtered.update(data);
  const bands = (root) => all(root, byClass('chart-band')).map((n) => n.getAttribute('class'));
  assert.deepEqual(bands(el), ['chart-band chart-s1', 'chart-band chart-s2', 'chart-band chart-s3']);
  assert.deepEqual(bands(filteredEl), ['chart-band chart-s1', 'chart-band chart-s3']);
  c.destroy();
  filtered.destroy();
});

test('timeseries draws lines, a legend, direct labels and a crosshair readout', () => {
  const el = host(720);
  const c = charts.timeseries(el, {
    title: 'Client requests against backend fetches',
    series: [
      { key: 'client', label: 'client requests', slot: 1 },
      { key: 'backend', label: 'backend fetches', slot: 2 },
    ],
    unit: 'rate',
    window: '15m',
  });
  c.update({ series: { client: series(90, T0 - 890, 10, (i) => 30 + (i % 7)), backend: series(90, T0 - 890, 10, (i) => 4 + (i % 3)) } });
  assert.equal(all(el, byClass('chart-legend-item')).length, 2);
  const lines = all(el, (n) => n.classList.contains('chart-line'));
  assert.equal(lines.length, 2);
  assert.equal(all(el, byClass('chart-area')).length, 0, 'multi-line charts get no area wash');
  assert.equal(all(el, byClass('chart-direct-label')).length, 2);
  const svgEl = all(el, byTag('svg'))[0];
  assert.equal(svgEl.getAttribute('tabindex'), '0');
  assert.match(svgEl.getAttribute('aria-label'), /client requests/);
  svgEl.fire('focus');
  const tip = all(el, byClass('chart-tip'))[0];
  assert.ok(tip.classList.contains('is-open'));
  assert.equal(all(tip, byClass('chart-tip-row')).length, 2);
  assert.match(tip.style.left, /^\d+px$/);
  svgEl.fire('keydown', { key: 'Home' });
  assert.match(all(el, (n) => n.getAttribute('aria-live') === 'polite')[0].textContent, /client requests/);
  svgEl.fire('keydown', { key: 'Escape' });
  assert.ok(!tip.classList.contains('is-open'));
  c.destroy();
});

test('a single series gets an area wash and no legend box', () => {
  const el = host();
  const c = charts.timeseries(el, { series: [{ key: 'ops', label: 'ops', slot: 1 }], unit: 'rate', window: '5m' });
  c.update({ series: { ops: series(30, T0 - 290, 10, (i) => i) } });
  assert.equal(all(el, byClass('chart-legend-item')).length, 0);
  assert.equal(all(el, byClass('chart-area')).length, 1);
  c.destroy();
});

test('fewer than two points shows the collecting state instead of a flat line', () => {
  const el = host();
  const c = charts.timeseries(el, { series: [{ key: 'ops', slot: 1 }], unit: 'rate' });
  c.update({ series: { ops: [[T0, 4]] } });
  assert.equal(all(el, byTag('svg')).length, 0);
  assert.equal(all(el, byClass('chart-empty'))[0].textContent, charts.EMPTY_TEXT);
  assert.ok(el.classList.contains('is-empty'));
  c.update({ series: { ops: [[T0 - 10, 3], [T0, 4]] } });
  assert.equal(all(el, byTag('svg')).length, 1);
  c.destroy();
});

test('where end dots overlap, the labelled series is drawn on top', () => {
  const el = host();
  const c = charts.timeseries(el, {
    series: [
      { key: 'a', label: 'published', slot: 1 },
      { key: 'b', label: 'delivered', slot: 2 },
    ],
    unit: 'rate',
    window: '5m',
  });
  c.update({ series: { a: series(10, T0 - 90, 10, () => 0), b: series(10, T0 - 90, 10, () => 0) } });
  assert.equal(all(el, byClass('chart-direct-label')).length, 1);
  const key = all(el, byClass('chart-key-stroke'))[0];
  const marks = all(el, byClass('chart-marks'))[0];
  const dots = marks.children.filter((n) => n.classList.contains('chart-dot'));
  const top = dots[dots.length - 1];
  assert.ok(['chart-s1', 'chart-s2'].some((s) => key.classList.contains(s) && top.classList.contains(s)));
  c.destroy();
});

test('the owner can say why a chart is empty, and fall back to collecting', () => {
  const el = host();
  const c = charts.donut(el, { label: 'from cache' });
  c.update({ value: null }, { emptyText: 'No cache lookups in this window.' });
  assert.equal(all(el, byClass('chart-empty'))[0].textContent, 'No cache lookups in this window.');
  c.update({ value: null }, { emptyText: undefined });
  assert.equal(all(el, byClass('chart-empty'))[0].textContent, charts.EMPTY_TEXT);
  c.destroy();
});

test('the Table toggle swaps the chart for a table, or asks the owner when controlled', () => {
  const el = host();
  const c = charts.timeseries(el, { series: [{ key: 'ops', label: 'ops', slot: 1 }], unit: 'rate', window: '5m' });
  c.update({ series: { ops: series(3, T0 - 20, 10, (i) => i * 10) } });
  const btn = all(el, byClass('chart-toggle'))[0];
  assert.equal(btn.textContent, 'Table');
  btn.fire('click');
  assert.equal(all(el, byTag('svg')).length, 0);
  const rows = all(el, byTag('tr'));
  assert.equal(rows.length, 4);
  assert.equal(rows[1].children[1].textContent, '20/s', 'newest row first');
  assert.equal(all(el, byClass('chart-toggle'))[0].getAttribute('aria-pressed'), 'true');
  c.destroy();

  const asked = [];
  const el2 = host();
  const c2 = charts.bars(el2, { onTableToggle: (next) => asked.push(next) });
  c2.update([{ key: 'a', label: 'a', value: 1 }]);
  all(el2, byClass('chart-toggle'))[0].fire('click');
  assert.deepEqual(asked, [true]);
  assert.equal(all(el2, byTag('table')).length, 0, 'a controlled chart waits for the route');
  c2.update([{ key: 'a', label: 'a', value: 1 }], { table: true });
  assert.equal(all(el2, byTag('table')).length, 1);
  c2.destroy();
});

test('donut shows the value, its label and the delta against the window start', () => {
  const el = host(320);
  const c = charts.donut(el, { title: 'How much is served from cache?', label: 'served from cache', goodWhen: 'up' });
  c.update({ value: 0.924, start: 0.903, startLabel: '15 min ago', sub: 'Lifetime 91.2%' });
  assert.equal(all(el, byClass('chart-hero'))[0].textContent, '92.4%');
  const delta = all(el, (n) => n.classList.contains('chart-delta'))[0];
  assert.equal(delta.textContent, '▲ 2.1 pts');
  assert.ok(delta.classList.contains('chart-delta-good'));
  const ring = all(el, byClass('chart-ring'))[0];
  assert.match(ring.getAttribute('stroke-dasharray'), /^\d+(\.\d)? \d+(\.\d)?$/);
  const mark = all(el, byClass('chart-mark'))[0];
  assert.equal(mark.getAttribute('tabindex'), '0');
  assert.match(mark.getAttribute('aria-label'), /92.4%/);
  assert.equal(all(el, byClass('chart-caption'))[0].textContent, 'Lifetime 91.2%');
  c.update({ value: 1 });
  assert.equal(all(el, byClass('chart-ring'))[0].getAttribute('stroke-dasharray'), null, 'a full ring is one circle');
  c.update({ value: null });
  assert.equal(all(el, byClass('chart-empty')).length, 1);
  c.destroy();
});

test('ranked bars sort, use one slot, label the tip and fold the tail', () => {
  const el = host(480);
  const picked = [];
  const rows = Array.from({ length: 10 }, (_, i) => ({ key: 'idx' + i, label: 'magento2_product_1_v' + i, value: (i + 1) * 1000 }));
  const c = charts.bars(el, { unit: 'count', mono: true, onSelect: (r) => picked.push(r.key) });
  c.update(rows);
  const groups = all(el, byClass('chart-row'));
  assert.equal(groups.length, 8);
  assert.match(groups[0].getAttribute('aria-label'), /v9: 10K/);
  const fills = all(el, byClass('chart-bar')).map((n) => n.getAttribute('class'));
  assert.ok(fills.slice(0, 7).every((c) => c === 'chart-bar chart-s1'));
  assert.equal(fills[7], 'chart-bar chart-sother');
  assert.equal(all(el, byClass('chart-legend-item')).length, 0);
  groups[0].fire('keydown', { key: 'Enter' });
  assert.deepEqual(picked, ['idx9']);
  groups[7].fire('click');
  assert.deepEqual(picked, ['idx9'], 'the Other row opens nothing');
  groups[1].fire('click');
  assert.deepEqual(picked, ['idx9', 'idx8']);
  c.destroy();
});

test('stacked bars keep a 2px gap between segments and round only the end', () => {
  const el = host(600);
  const c = charts.stackedBars(el, {
    unit: 'count',
    series: [
      { key: 'ready', label: 'ready', slot: 1 },
      { key: 'unacked', label: 'unacked', slot: 2 },
    ],
  });
  c.update([
    { key: 'a', label: 'async.operations.all', values: { ready: 120, unacked: 30 }, flag: 'no consumer' },
    { key: 'b', label: 'product_action_attribute.update', values: { ready: 10, unacked: 0 } },
  ]);
  assert.equal(all(el, byClass('chart-legend-item')).length, 2);
  const row = all(el, byClass('chart-row'))[0];
  const segs = all(row, byClass('chart-bar'));
  assert.equal(segs.length, 2);
  assert.ok(!/A/.test(segs[0].getAttribute('d')), 'inner segment is square');
  assert.ok(/A4 4/.test(segs[1].getAttribute('d')), 'end segment is rounded');
  const end0 = Number(/H([\d.]+)/.exec(segs[0].getAttribute('d'))[1]);
  const start1 = Number(/^M([\d.]+)/.exec(segs[1].getAttribute('d'))[1]);
  assert.ok(Math.abs(start1 - end0 - 2) < 0.2);
  assert.equal(all(row, byClass('chart-flag'))[0].textContent, 'no consumer');
  assert.equal(all(row, byClass('chart-flag-icon')).length, 1);
  c.table(true);
  const cells = all(el, byTag('td')).map((n) => n.textContent);
  assert.deepEqual(cells.slice(0, 4), ['async.operations.all (no consumer)', '120', '30', '150']);
  c.destroy();
});

test('histogram draws one column per bucket and labels only the peak', () => {
  const el = host(560);
  const c = charts.histogram(el, { unit: 'count', valueLabel: 'requests' });
  const buckets = [
    { label: '≤50', value: 2 },
    { label: '100', value: 9 },
    { label: '200', value: 31 },
    { label: '500', value: 18 },
    { label: '1 s', value: 6 },
    { label: '2 s', value: 1 },
    { label: '5 s', value: 0 },
    { label: '>5 s', value: 0 },
  ];
  c.update(buckets);
  assert.equal(all(el, byClass('chart-col')).length, 8);
  assert.equal(all(el, byClass('chart-bar')).length, 6);
  const values = all(el, byClass('chart-bar-value'));
  assert.equal(values.length, 1);
  assert.equal(values[0].textContent, '31');
  for (const p of all(el, byClass('chart-bar'))) {
    const w = Number(/H([\d.]+)/.exec(p.getAttribute('d'))[1]) + 4 - Number(/^M([\d.]+)/.exec(p.getAttribute('d'))[1]);
    assert.ok(w <= 24.2, 'columns are capped at 24px');
  }
  c.update([{ label: '≤50', value: 0 }]);
  assert.equal(all(el, byClass('chart-empty')).length, 1);
  c.destroy();
});

test('sparkline has no axes or toggle and labels the last value', () => {
  const el = host(200);
  const c = charts.sparkline(el, { unit: 'rate', label: 'hits', window: '15m' });
  c.update(series(20, T0 - 190, 10, (i) => i));
  assert.equal(all(el, byClass('chart-axis')).length, 0);
  assert.equal(all(el, byClass('chart-toggle')).length, 0);
  assert.equal(all(el, byClass('chart-direct-value'))[0].textContent, '19/s');
  assert.match(all(el, byTag('svg'))[0].getAttribute('aria-label'), /now 19\/s/);
  c.destroy();
});

test('nothing is drawn with style attributes or markup', () => {
  const el = host();
  const c = charts.timeseries(el, { series: [{ key: 'a', slot: 1 }, { key: 'b', slot: 2 }], mode: 'stack' });
  c.update({ series: { a: series(10, T0 - 90, 10, () => 1), b: series(10, T0 - 90, 10, () => 2) } });
  everyNode(el, (n) => assert.equal(n.getAttribute('style'), null));
  c.destroy();
});

test('destroy releases the observer, listeners, crosshair group and DOM', () => {
  const el = host();
  const before = observers.length;
  const a = charts.timeseries(el, { series: [{ key: 'x', slot: 1 }], group: 'varnish', window: '5m' });
  a.update({ series: { x: series(10, T0 - 90, 10, (i) => i) } });
  const el2 = host();
  const b = charts.timeseries(el2, { series: [{ key: 'x', slot: 1 }], group: 'varnish', window: '5m' });
  b.update({ series: { x: series(10, T0 - 90, 10, (i) => i * 2) } });
  assert.equal(charts.groupSize('varnish'), 2);
  const ro = observers[before];
  assert.deepEqual(ro.targets, [el]);
  const svgA = all(el, byTag('svg'))[0];
  const btn = all(el, byClass('chart-toggle'))[0];
  svgA.fire('focus');
  assert.ok(all(el2, byClass('chart-cross'))[0].classList.contains('is-on'), 'hovering one chart moves the crosshair on its group');
  svgA.fire('blur');
  assert.ok(!all(el2, byClass('chart-cross'))[0].classList.contains('is-on'));
  a.destroy();
  assert.ok(ro.disconnected);
  assert.equal(svgA.listenerCount(), 0);
  assert.equal(btn.listenerCount(), 0);
  assert.equal(el.children.length, 0);
  assert.ok(!el.classList.contains('chart'));
  assert.equal(charts.groupSize('varnish'), 1);
  b.destroy();
  assert.equal(charts.groupSize('varnish'), 0);
  a.update({ series: {} });
  assert.equal(el.children.length, 0, 'a destroyed chart ignores updates');
});

test('a resize redraws at the new width', () => {
  const el = host(400);
  const c = charts.timeseries(el, { series: [{ key: 'x', slot: 1 }], window: '5m' });
  c.update({ series: { x: series(10, T0 - 90, 10, (i) => i) } });
  assert.equal(all(el, byTag('svg'))[0].getAttribute('width'), '400');
  el.clientWidth = 800;
  observers[observers.length - 1].cb();
  assert.equal(all(el, byTag('svg'))[0].getAttribute('width'), '800');
  c.destroy();
});

test('a hidden chart waits for a width before drawing', () => {
  const el = host(0);
  const c = charts.bars(el, {});
  c.update([{ key: 'a', label: 'a', value: 3 }]);
  assert.equal(all(el, byTag('svg')).length, 0);
  el.clientWidth = 300;
  observers[observers.length - 1].cb();
  assert.equal(all(el, byTag('svg')).length, 1);
  c.destroy();
});

test('a narrow chart keeps end values but drops names that would be cut', () => {
  const el = host(380);
  const c = charts.timeseries(el, {
    series: [
      { key: 'client', label: 'client requests', slot: 1 },
      { key: 'backend', label: 'backend fetches', slot: 2 },
    ],
    unit: 'rate',
    window: '5m',
  });
  c.update({ series: { client: series(30, T0 - 290, 10, () => 40), backend: series(30, T0 - 290, 10, () => 5) } });
  assert.equal(all(el, byClass('chart-direct-name')).length, 0);
  assert.deepEqual(all(el, byClass('chart-direct-value')).map((n) => n.textContent), ['40/s', '5/s']);
  assert.ok(!all(el, byTag('tspan')).some((n) => n.textContent.includes('…')));
  c.destroy();
});

test('ranges draw a min-max range, every sample and the median on one axis', () => {
  const el = host(520);
  const rows = [
    { key: 'off', label: 'Tracing off', samples: [400, 410, 420], min: 400, max: 420, median: 410 },
    { key: 'full', label: 'Full', samples: [480, 500, 530], min: 480, max: 530, median: 500, note: '+22%' },
    { key: 'lean', label: 'Lean', samples: [], min: 0, max: 0, median: 0 },
  ];
  const c = charts.ranges(el, { unit: 'ms', title: 'How much slower?' });
  c.update(rows);
  const groups = all(el, byClass('chart-row'));
  assert.equal(groups.length, 3, 'a row per arm, also one still waiting for samples');
  assert.equal(all(el, byClass('chart-range')).length, 2);
  assert.equal(all(el, byClass('chart-sample')).length, 6);
  const medians = all(el, byClass('chart-median'));
  assert.equal(medians.length, 2);
  assert.ok(medians.every((m) => m.getAttribute('class').includes('chart-s1')), 'one measure, one slot');
  assert.match(groups[1].getAttribute('aria-label'), /Full: median 500 ms, 480 ms to 530 ms, 3 samples, \+22%/);
  assert.match(groups[2].getAttribute('aria-label'), /no samples yet/);
  const values = all(el, byClass('chart-bar-value')).map((n) => n.textContent);
  assert.deepEqual(values, ['410 ms', '500 ms · +22%', '–']);
  assert.ok(all(el, byClass('chart-axis')).length >= 2, 'the shared x axis has ticks');
  assert.equal(all(el, byClass('chart-legend-item')).length, 0);
  groups[1].fire('focus');
  const tip = all(el, byClass('chart-tip'))[0];
  assert.ok(tip.classList.contains('is-open'));
  assert.match(tip.textContent, /500 ms/);
  everyNode(el, (n) => assert.equal(n.getAttribute('style'), null));
  c.table(true);
  const cells = all(el, byTag('td')).map((n) => n.textContent);
  assert.deepEqual(cells.slice(0, 5), ['Tracing off', '410 ms', '400 ms', '420 ms', '']);
  assert.equal(cells[9], '+22%');
  c.destroy();
});

test('ranges with no samples at all show the empty text', () => {
  const el = host(400);
  const c = charts.ranges(el, { unit: 'ms', emptyText: 'Run the benchmark to fill this' });
  c.update([{ key: 'off', label: 'Off', samples: [] }]);
  assert.match(el.textContent, /Run the benchmark/);
  c.destroy();
});

test('count axes step in whole numbers', () => {
  assert.deepEqual(charts.yTicks('count', 0, 4), [0, 1]);
  assert.deepEqual(charts.yTicks('count', 2.2, 4), [0, 1, 2, 3]);
  assert.deepEqual(charts.yTicks('count', 87, 4), [0, 25, 50, 75, 100]);
  assert.deepEqual(charts.yTicks('rate', 0, 4), [0, 0.25, 0.5, 0.75, 1]);
});
