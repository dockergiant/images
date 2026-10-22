const test = require('node:test');
const assert = require('node:assert/strict');
const flame = require('../src/lib/flame.js');

const frames = [
  { name: '/var/www/html/pub/index.php', file: '/var/www/html/pub/index.php' },
  { name: 'Magento\\Framework\\App\\Http::launch', file: 'Http.php' },
  { name: 'PDOStatement::execute' },
  { name: 'Redis::hGet' },
];

function sampled(unit, weights) {
  return {
    shared: { frames },
    profiles: [
      {
        type: 'sampled',
        unit,
        startValue: 0,
        endValue: 20,
        samples: [[0, 1, 2], [0, 1, 2], [0, 1, 3], [0, 1], [0]],
        weights,
      },
    ],
  };
}

test('fromSpeedscope merges sampled stacks into an icicle tree in nanoseconds', () => {
  const { root, unit, samples } = flame.fromSpeedscope(sampled('microseconds', [5, 5, 5, 5, 5]));
  assert.equal(unit, 'ns');
  assert.equal(samples, 5);
  assert.equal(root.value, 25000);
  const index = root.children[0];
  assert.equal(index.name, '/var/www/html/pub/index.php');
  assert.equal(index.value, 25000);
  assert.equal(index.self, 5000);
  const launch = index.children[0];
  assert.equal(launch.file, 'Http.php');
  assert.deepEqual(launch.children.map((c) => c.name), ['PDOStatement::execute', 'Redis::hGet']);
  assert.equal(launch.children[0].value, 10000);
  assert.equal(launch.children[0].start, 0);
  assert.equal(launch.children[1].start, 10000);
  assert.equal(launch.children[1].parent, launch);
});

test('fromSpeedscope counts samples when the unit or weights are missing', () => {
  const { root, unit } = flame.fromSpeedscope(sampled('none'));
  assert.equal(unit, 'samples');
  assert.equal(root.value, 5);
});

test('fromSpeedscope reads evented profiles', () => {
  const p = {
    shared: { frames },
    profiles: [
      {
        type: 'evented',
        unit: 'nanoseconds',
        startValue: 0,
        endValue: 100,
        events: [
          { type: 'O', frame: 0, at: 0 },
          { type: 'O', frame: 1, at: 10 },
          { type: 'O', frame: 2, at: 20 },
          { type: 'C', frame: 2, at: 50 },
          { type: 'O', frame: 2, at: 60 },
          { type: 'C', frame: 2, at: 70 },
          { type: 'C', frame: 1, at: 90 },
          { type: 'C', frame: 0, at: 100 },
        ],
      },
    ],
  };
  const { root } = flame.fromSpeedscope(p);
  assert.equal(root.value, 100);
  const launch = root.children[0].children[0];
  assert.equal(launch.value, 80);
  assert.equal(launch.children[0].value, 40);
  assert.equal(launch.self, 40);
});

test('fromSpeedscope rejects other documents', () => {
  assert.throws(() => flame.fromSpeedscope({}), /speedscope/);
  assert.throws(() => flame.fromSpeedscope({ shared: { frames: [] }, profiles: [{ type: 'other' }] }), /profile type/);
});

test('layout scales the focus to the width and stacks its ancestors above it', () => {
  const { root } = flame.fromSpeedscope(sampled('nanoseconds', [1, 1, 1, 1, 1]));
  const lay = flame.layout(root, root, 500, { rowH: 18 });
  assert.equal(lay.rowH, 18);
  assert.equal(lay.rects[0].node, root);
  assert.equal(lay.rects[0].w, 500);
  const pdo = lay.rects.find((r) => r.node.name === 'PDOStatement::execute');
  assert.equal(pdo.x, 0);
  assert.equal(pdo.w, 200);
  assert.equal(pdo.y, 3 * 18);
  assert.equal(lay.height, 4 * 18);

  const launch = root.children[0].children[0];
  const zoomed = flame.layout(root, launch, 500, { rowH: 18 });
  const anc = zoomed.rects.filter((r) => r.ancestor);
  assert.deepEqual(anc.map((r) => r.node), [root, root.children[0]]);
  assert.ok(anc.every((r) => r.w === 500 && r.x === 0));
  const redis = zoomed.rects.find((r) => r.node.name === 'Redis::hGet');
  assert.equal(redis.x, 500 * (2 / 4));
  assert.equal(redis.w, 500 * (1 / 4));
  assert.equal(redis.y, 3 * 18);
});

test('layout culls frames narrower than the minimum width', () => {
  const root = { name: 'r', value: 1000, start: 0, children: [], parent: null };
  const big = { name: 'big', value: 999, start: 0, children: [], parent: root };
  const tiny = { name: 'tiny', value: 1, start: 999, children: [], parent: root };
  root.children.push(big, tiny);
  const lay = flame.layout(root, root, 100, { rowH: 10, minW: 1 });
  assert.deepEqual(lay.rects.map((r) => r.node.name), ['r', 'big']);
});

test('label fits a frame name into the box, preferring the short name', () => {
  assert.equal(flame.label('Magento\\Framework\\App\\Http::launch', 1000), 'Magento\\Framework\\App\\Http::launch');
  assert.equal(flame.label('Magento\\Framework\\App\\Http::launch', 100), 'Http::launch');
  assert.equal(flame.label('Magento\\Framework\\App\\Http::launch', 66), 'Http::l…');
  assert.equal(flame.label('anything', 15), '');
});

test('component guesses the service of a profile frame', () => {
  assert.equal(flame.component({ name: 'PDOStatement::execute' }), 'sql');
  assert.equal(flame.component({ name: 'Redis::hGet' }), 'cache');
  assert.equal(flame.component({ name: 'Credis_Client::__call' }), 'cache');
  assert.equal(flame.component({ name: 'curl_exec' }), 'http');
  assert.equal(flame.component({ name: 'PhpAmqpLib\\Channel\\AMQPChannel::basic_publish' }), 'queue');
  assert.equal(flame.component({ name: 'Magento\\Foo::bar' }), 'php');
  assert.equal(flame.component({ name: 'x', component: 'search' }), 'search');
});

function fakeDoc() {
  function el(tag) {
    return {
      tag,
      attrs: {},
      children: [],
      listeners: {},
      textContent: '',
      setAttribute(k, v) {
        this.attrs[k] = String(v);
      },
      appendChild(c) {
        if (c.tag === '#fragment') this.children.push(...c.children);
        else this.children.push(c);
        return c;
      },
      removeChild(c) {
        this.children.splice(this.children.indexOf(c), 1);
      },
      get firstChild() {
        return this.children[0] || null;
      },
      addEventListener(type, fn) {
        this.listeners[type] = fn;
      },
    };
  }
  return {
    created: [],
    createElementNS(ns, tag) {
      assert.equal(ns, 'http://www.w3.org/2000/svg');
      const e = el(tag);
      this.created.push(e);
      return e;
    },
    createDocumentFragment() {
      return el('#fragment');
    },
  };
}

test('render paints rects and labels with createElementNS and textContent', () => {
  const { root } = flame.fromSpeedscope(sampled('nanoseconds', [1, 1, 1, 1, 1]));
  const lay = flame.layout(root, root, 800, { rowH: 18 });
  const doc = fakeDoc();
  const svg = doc.createElementNS('http://www.w3.org/2000/svg', 'svg');
  svg.appendChild({ tag: 'old' });
  const clicks = [];
  const hovers = [];
  flame.render(doc, svg, lay, {
    width: 800,
    classFor: (n) => 'c-' + flame.component(n),
    labelClass: 'lbl',
    onClick: (n) => clicks.push(n),
    onHover: (n) => hovers.push(n),
  });
  assert.equal(svg.attrs.width, '800');
  assert.equal(svg.attrs.height, String(lay.height));
  assert.equal(svg.children.length, lay.rects.length);
  assert.ok(!svg.children.some((c) => c.tag === 'old'));
  const g = svg.children.find((c) => c.children.some((x) => x.tag === 'text' && x.textContent === 'PDOStatement::execute'));
  assert.ok(g, 'a labelled group for PDOStatement::execute');
  const rect = g.children.find((x) => x.tag === 'rect');
  assert.equal(rect.attrs.class, 'c-sql');
  const title = g.children.find((x) => x.tag === 'title');
  assert.equal(title.textContent, 'PDOStatement::execute');
  g.listeners.click();
  g.listeners.mouseenter();
  g.listeners.mouseleave();
  assert.equal(clicks[0].name, 'PDOStatement::execute');
  assert.equal(hovers[0].name, 'PDOStatement::execute');
  assert.equal(hovers[1], null);
});

test('layout of a large tree stays fast', () => {
  const root = { name: 'r', value: 0, start: 0, children: [], parent: null };
  let start = 0;
  for (let i = 0; i < 3000; i++) {
    const c = { name: 'c' + i, value: 10, start, children: [], parent: root };
    c.children.push({ name: 'g' + i, value: 5, start, children: [], parent: c });
    root.children.push(c);
    start += 10;
  }
  root.value = start;
  const t0 = process.hrtime.bigint();
  const lay = flame.layout(root, root, 1200, { rowH: 18 });
  const took = Number(process.hrtime.bigint() - t0) / 1e6;
  assert.ok(lay.rects.length < 3000);
  // Wall-clock bounds fail on a loaded machine, so they only run as a bench.
  if (process.env.INSPECTOR_BENCH) assert.ok(took < 100, 'took ' + took + ' ms');
});

test('step moves between labelled frames: siblings, parent and first child', () => {
  const { root } = flame.fromSpeedscope(sampled('nanoseconds', [1, 1, 1, 1, 1]));
  const lay = flame.layout(root, root, 800, { rowH: 18 });
  const at = (name) => lay.rects.findIndex((r) => r.node.name === name);
  const pdo = at('PDOStatement::execute');
  const redis = at('Redis::hGet');
  const launch = at('Magento\\Framework\\App\\Http::launch');
  assert.equal(flame.step(lay, pdo, 'ArrowRight'), redis);
  assert.equal(flame.step(lay, redis, 'ArrowLeft'), pdo);
  assert.equal(flame.step(lay, redis, 'ArrowRight'), -1);
  assert.equal(flame.step(lay, pdo, 'ArrowUp'), launch);
  assert.equal(flame.step(lay, launch, 'ArrowDown'), pdo);
  assert.equal(flame.step(lay, pdo, 'x'), -1);
});

test('render gives one labelled frame the tab stop and lets the keyboard zoom', () => {
  const { root } = flame.fromSpeedscope(sampled('nanoseconds', [1, 1, 1, 1, 1]));
  const lay = flame.layout(root, root, 800, { rowH: 18 });
  const doc = fakeDoc();
  const svg = doc.createElementNS('http://www.w3.org/2000/svg', 'svg');
  const clicks = [];
  const hovers = [];
  flame.render(doc, svg, lay, { width: 800, ariaLabel: (n) => 'frame ' + n.name, onClick: (n) => clicks.push(n), onHover: (n) => hovers.push(n), current: root });
  const stops = svg.children.filter((g) => g.attrs.tabindex === '0');
  assert.equal(stops.length, 1);
  assert.equal(stops[0].attrs['aria-label'], 'frame ' + root.name);
  assert.equal(stops[0].attrs.role, 'button');
  const g = svg.children.find((c) => c.attrs['aria-label'] === 'frame PDOStatement::execute');
  assert.equal(g.attrs.tabindex, '-1');
  let prevented = false;
  g.listeners.keydown({ key: 'Enter', preventDefault: () => (prevented = true) });
  assert.equal(prevented, true);
  assert.equal(clicks[0].name, 'PDOStatement::execute');
  g.listeners.focus();
  assert.equal(hovers[hovers.length - 1].name, 'PDOStatement::execute');
});
