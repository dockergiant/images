(function (root, factory) {
  const mod = factory();
  if (typeof module === 'object' && module.exports) {
    module.exports = mod;
  } else {
    root.Inspector = root.Inspector || {};
    root.Inspector.flame = mod;
  }
})(this, function () {
  const SVG_NS = 'http://www.w3.org/2000/svg';
  const UNIT_NS = { nanoseconds: 1, microseconds: 1e3, milliseconds: 1e6, seconds: 1e9 };
  const CHAR_W = 6.6;
  const PAD = 4;

  function makeNode(name, file, parent) {
    return { name, file: file || '', value: 0, self: 0, start: 0, children: [], parent, kids: new Map() };
  }

  function childOf(n, index, frames) {
    let c = n.kids.get(index);
    if (!c) {
      const f = frames[index] || {};
      c = makeNode(f.name || '(anonymous ' + index + ')', f.file, n);
      n.kids.set(index, c);
      n.children.push(c);
    }
    return c;
  }

  function finalize(root, factor) {
    const stack = [root];
    while (stack.length) {
      const n = stack.pop();
      delete n.kids;
      if (factor !== 1) {
        n.value *= factor;
        n.self *= factor;
      }
      n.children.sort((a, b) => b.value - a.value);
      stack.push(...n.children);
    }
    const order = [root];
    for (let i = 0; i < order.length; i++) {
      const n = order[i];
      let at = n.start;
      let kids = 0;
      for (const c of n.children) {
        c.start = at;
        at += c.value;
        kids += c.value;
        order.push(c);
      }
      if (n.selfFromValue) {
        n.self = Math.max(0, n.value - kids);
        delete n.selfFromValue;
      }
    }
  }

  function sampled(p, frames, root) {
    const samples = Array.isArray(p.samples) ? p.samples : [];
    const weights = Array.isArray(p.weights) && p.weights.length === samples.length ? p.weights : null;
    for (let i = 0; i < samples.length; i++) {
      const w = weights ? Number(weights[i]) || 0 : 1;
      let n = root;
      n.value += w;
      for (const f of samples[i] || []) {
        n = childOf(n, f, frames);
        n.value += w;
      }
      n.self += w;
    }
    return samples.length;
  }

  function evented(p, frames, root) {
    const events = Array.isArray(p.events) ? p.events : [];
    const open = [{ node: root, frame: -1, at: Number(p.startValue) || 0 }];
    const all = new Set();
    for (const ev of events) {
      const top = open[open.length - 1];
      if (ev.type === 'O') {
        const c = childOf(top.node, ev.frame, frames);
        all.add(c);
        open.push({ node: c, frame: ev.frame, at: Number(ev.at) || 0 });
      } else if (ev.type === 'C' && open.length > 1) {
        let i = open.length - 1;
        while (i > 0 && open[i].frame !== ev.frame) i--;
        if (i === 0) continue;
        while (open.length > i) {
          const o = open.pop();
          o.node.value += Math.max(0, (Number(ev.at) || 0) - o.at);
        }
      }
    }
    const end = Number(p.endValue) || 0;
    while (open.length > 1) {
      const o = open.pop();
      o.node.value += Math.max(0, end - o.at);
    }
    let kids = 0;
    for (const c of root.children) kids += c.value;
    root.value = Math.max(kids, end - (Number(p.startValue) || 0));
    root.selfFromValue = true;
    for (const n of all) n.selfFromValue = true;
    return events.length;
  }

  function fromSpeedscope(doc) {
    if (!doc || !doc.shared || !Array.isArray(doc.shared.frames) || !Array.isArray(doc.profiles) || !doc.profiles.length) {
      throw new Error('This is not a speedscope profile.');
    }
    const frames = doc.shared.frames;
    const idx = Number.isInteger(doc.activeProfileIndex) && doc.profiles[doc.activeProfileIndex] ? doc.activeProfileIndex : 0;
    const p = doc.profiles[idx] || {};
    const root = makeNode('all', '', null);
    let samples;
    if (p.type === 'sampled') samples = sampled(p, frames, root);
    else if (p.type === 'evented') samples = evented(p, frames, root);
    else throw new Error('Unknown speedscope profile type ' + JSON.stringify(p.type) + '.');
    const factor = UNIT_NS[p.unit];
    finalize(root, factor || 1);
    return { root, unit: factor ? 'ns' : 'samples', samples, type: p.type };
  }

  function layout(root, focus, width, opts) {
    opts = opts || {};
    const rowH = opts.rowH || 18;
    const minW = opts.minW === undefined ? 1 : opts.minW;
    focus = focus || root;
    const anc = [];
    for (let p = focus.parent; p; p = p.parent) anc.unshift(p);
    const rects = anc.map((n, d) => ({ node: n, x: 0, w: width, y: d * rowH, depth: d, ancestor: true }));
    const base = anc.length;
    const scale = focus.value > 0 ? width / focus.value : 0;
    let deepest = base;
    const stack = [[focus, 0]];
    while (stack.length) {
      const [n, d] = stack.pop();
      let x = (n.start - focus.start) * scale;
      let w = n.value * scale;
      if (x < 0) {
        w += x;
        x = 0;
      }
      if (x + w > width) w = width - x;
      if (w < minW) continue;
      const depth = base + d;
      if (depth > deepest) deepest = depth;
      rects.push({ node: n, x, w, y: depth * rowH, depth, ancestor: false });
      for (let i = n.children.length - 1; i >= 0; i--) stack.push([n.children[i], d + 1]);
    }
    return { rects, height: (deepest + 1) * rowH, rowH, width };
  }

  function shortName(name) {
    const m = /([^\\/]+(?:::.+)?)$/.exec(name);
    return m ? m[1] : name;
  }

  function label(name, w) {
    const room = Math.floor((w - 2 * PAD) / CHAR_W);
    if (room < 3) return '';
    name = String(name || '');
    if (name.length <= room) return name;
    const short = shortName(name);
    if (short.length <= room) return short;
    return short.slice(0, room - 1) + '…';
  }

  function component(n) {
    if (n.component) return n.component;
    const name = n.name || '';
    if (/^(PDO|mysqli)/.test(name)) return 'sql';
    if (/^(Redis|Credis|Cm_Cache|Predis|Memcached)/.test(name)) return 'cache';
    if (/^curl_/.test(name) || /GuzzleHttp\\/.test(name)) return 'http';
    if (/(PhpAmqpLib\\|^AMQP)/.test(name)) return 'queue';
    if (/(Elasticsearch|OpenSearch)\\/.test(name)) return 'search';
    return 'php';
  }

  // Only frames wide enough for a label take part in keyboard navigation: a
  // profile can draw hundreds of sub-pixel frames nobody can read anyway.
  function navigable(r) {
    return label(r.node.name, r.w) !== '';
  }

  // step is the frame the arrow key moves to from rects[i], or -1:
  // left and right walk the row, up goes to the parent, down to the first child.
  function step(lay, i, key) {
    const rects = lay.rects;
    const cur = rects[i];
    if (!cur) return -1;
    let best = -1;
    for (let j = 0; j < rects.length; j++) {
      const r = rects[j];
      if (j === i || !navigable(r)) continue;
      switch (key) {
        case 'ArrowUp':
          if (r.node === cur.node.parent) return j;
          break;
        case 'ArrowDown':
          if (r.node.parent === cur.node && (best < 0 || r.x < rects[best].x)) best = j;
          break;
        case 'ArrowLeft':
          if (r.depth === cur.depth && r.x < cur.x && (best < 0 || r.x > rects[best].x)) best = j;
          break;
        case 'ArrowRight':
          if (r.depth === cur.depth && r.x > cur.x && (best < 0 || r.x < rects[best].x)) best = j;
          break;
        default:
          return -1;
      }
    }
    return best;
  }

  function render(doc, svg, lay, opts) {
    opts = opts || {};
    while (svg.firstChild) svg.removeChild(svg.firstChild);
    const width = opts.width || lay.width || 0;
    svg.setAttribute('width', width);
    svg.setAttribute('height', lay.height);
    svg.setAttribute('viewBox', '0 0 ' + width + ' ' + lay.height);
    const frag = doc.createDocumentFragment();
    const h = Math.max(1, lay.rowH - 1);
    const groups = [];
    let stop = lay.rects.findIndex((r) => r.node === opts.current && navigable(r));
    if (stop < 0) stop = lay.rects.findIndex(navigable);
    const moveTo = (from, to) => {
      if (to < 0 || !groups[to]) return;
      groups[from].setAttribute('tabindex', -1);
      groups[to].setAttribute('tabindex', 0);
      if (typeof groups[to].focus === 'function') groups[to].focus();
    };
    for (const [i, r] of lay.rects.entries()) {
      const g = doc.createElementNS(SVG_NS, 'g');
      g.setAttribute('class', 'flame-frame');
      const rect = doc.createElementNS(SVG_NS, 'rect');
      rect.setAttribute('x', r.x);
      rect.setAttribute('y', r.y);
      rect.setAttribute('width', Math.max(0.5, r.w - 1));
      rect.setAttribute('height', h);
      rect.setAttribute('rx', 2);
      if (opts.classFor) rect.setAttribute('class', opts.classFor(r.node, r));
      g.appendChild(rect);
      const title = doc.createElementNS(SVG_NS, 'title');
      title.textContent = r.node.name;
      g.appendChild(title);
      const text = label(r.node.name, r.w);
      if (text) {
        const t = doc.createElementNS(SVG_NS, 'text');
        t.setAttribute('x', r.x + PAD);
        t.setAttribute('y', r.y + lay.rowH - 5);
        if (opts.labelClass) t.setAttribute('class', opts.labelClass);
        t.textContent = text;
        g.appendChild(t);
      }
      if (opts.onClick) g.addEventListener('click', () => opts.onClick(r.node, r));
      if (opts.onHover) {
        g.addEventListener('mouseenter', () => opts.onHover(r.node, r));
        g.addEventListener('mouseleave', () => opts.onHover(null, null));
      }
      if (text) {
        g.setAttribute('role', 'button');
        g.setAttribute('tabindex', i === stop ? 0 : -1);
        g.setAttribute('aria-label', opts.ariaLabel ? opts.ariaLabel(r.node) : r.node.name);
        g.addEventListener('keydown', (ev) => {
          if (ev.key === 'Enter' || ev.key === ' ') {
            ev.preventDefault();
            if (opts.onClick) opts.onClick(r.node, r, true);
            return;
          }
          const to = step(lay, i, ev.key);
          if (to < 0) return;
          ev.preventDefault();
          moveTo(i, to);
        });
        if (opts.onHover) {
          g.addEventListener('focus', () => opts.onHover(r.node, r));
          g.addEventListener('blur', () => opts.onHover(null, null));
        }
      }
      groups[i] = g;
      frag.appendChild(g);
    }
    svg.appendChild(frag);
    if (opts.refocus && stop >= 0 && typeof groups[stop].focus === 'function') groups[stop].focus();
  }

  return { fromSpeedscope, layout, label, shortName, component, step, render };
});
