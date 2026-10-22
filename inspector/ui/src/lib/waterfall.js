(function (root, factory) {
  const mod = factory();
  if (typeof module === 'object' && module.exports) {
    module.exports = mod;
  } else {
    root.Inspector = root.Inspector || {};
    root.Inspector.waterfall = mod;
  }
})(this, function () {
  const NS_PER_MS = 1e6;
  const FOLD_MIN = 4;
  const DETAIL_MAX = 200;
  const SERVICE = { sql: true, cache: true, search: true, queue: true, http: true };
  const COMPONENT_LABEL = { sql: 'SQL', cache: 'Cache', search: 'Search', queue: 'Queue', http: 'HTTP', magento: 'Magento', php: 'PHP' };
  const TIME_RE = /^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(?:\.(\d{1,9}))?(Z|[+-]\d{2}:\d{2})$/;

  // Date.parse drops everything below a millisecond, which would give sub-ms spans no width and no order.
  function parseTime(s) {
    if (typeof s !== 'string') return null;
    const m = TIME_RE.exec(s);
    if (!m) return null;
    const ms = Date.parse(m[1] + m[3]);
    if (Number.isNaN(ms)) return null;
    return { ms, ns: m[2] ? Number(m[2].padEnd(9, '0')) : 0 };
  }

  function diff(a, b) {
    if (!a || !b) return 0;
    return (a.ms - b.ms) * NS_PER_MS + (a.ns - b.ns);
  }

  function offsetNs(t, base) {
    return diff(parseTime(t), parseTime(base));
  }

  function isHidden(s) {
    const name = s.name || '';
    return name.startsWith('magento.plugin') || name === 'magento.event.execute';
  }

  function cut(s, n) {
    s = String(s || '');
    return s.length > n ? s.slice(0, n - 1) + '…' : s;
  }

  function urlTail(u) {
    const m = /^[a-z][a-z0-9+.-]*:\/\/([^/?#]*)([^?#]*)/i.exec(u);
    if (!m) return '';
    return m[1] + m[2];
  }

  function describe(s) {
    const a = s.attrs || {};
    const raw = a['redis.raw_command'];
    if (raw !== undefined) {
      const words = String(raw).split(' ');
      return cut(words.length > 1 ? words[1] : '', DETAIL_MAX);
    }
    const res = s.resource || '';
    if (res && res !== s.name) {
      if (/^https?:\/\//i.test(res)) return cut(urlTail(res), DETAIL_MAX);
      return cut(res, DETAIL_MAX);
    }
    const u = a['http.url'] || a['url.full'];
    if (u) return cut(urlTail(u), DETAIL_MAX);
    return '';
  }

  function buildTree(trace) {
    const spans = (trace && trace.spans) || [];
    if (!spans.length) return null;
    const base = parseTime(spans[0].start);
    const nodes = new Array(spans.length);
    const byId = new Map();
    for (let i = 0; i < spans.length; i++) {
      const s = spans[i];
      const key = s.spanId && !byId.has(s.spanId) ? s.spanId : 'span-' + i;
      const n = {
        i,
        key,
        span: s,
        start: diff(parseTime(s.start), base),
        dur: s.duration > 0 ? s.duration : 0,
        self: 0,
        children: [],
        parent: null,
        hidden: i > 0 && isHidden(s),
      };
      nodes[i] = n;
      if (s.spanId && !byId.has(s.spanId)) byId.set(s.spanId, n);
    }
    const root = nodes[0];
    let hidden = 0;
    for (let i = 1; i < nodes.length; i++) {
      const n = nodes[i];
      let p = byId.get(n.span.parentId);
      if (!p || p === n || isDescendant(p, n)) p = root;
      n.parent = p;
      p.children.push(n);
      if (n.hidden) hidden++;
    }
    for (const n of nodes) n.self = selfTime(n);
    setDepth(root);
    const byKey = new Map();
    for (const n of nodes) byKey.set(n.key, n);
    const total = trace.duration > 0 ? trace.duration : root.dur || 1;
    return { root, nodes, total, hidden, byKey };
  }

  // Self time is the span's window minus the union of its children's
  // windows, clipped to it, as the server's analysis computes it, so the
  // Bottlenecks numbers and the waterfall agree when children overlap.
  function selfTime(n) {
    if (!n.children.length) return n.dur;
    const lo = n.start;
    const hi = n.start + n.dur;
    const iv = [];
    for (const c of n.children) {
      const s = Math.max(c.start, lo);
      const e = Math.min(c.start + c.dur, hi);
      if (e > s) iv.push([s, e]);
    }
    iv.sort((a, b) => a[0] - b[0]);
    let covered = 0;
    let cs = 0;
    let ce = -Infinity;
    for (const [s, e] of iv) {
      if (s > ce) {
        if (ce > cs) covered += ce - cs;
        cs = s;
        ce = e;
      } else if (e > ce) {
        ce = e;
      }
    }
    if (ce > cs) covered += ce - cs;
    return Math.max(0, n.dur - covered);
  }

  function setDepth(root) {
    root.depth = 0;
    const stack = [root];
    while (stack.length) {
      const n = stack.pop();
      for (const c of n.children) {
        c.depth = n.depth + 1;
        stack.push(c);
      }
    }
  }

  // Parents are linked in span order, so a parent that is already below n would close a cycle.
  function isDescendant(p, n) {
    for (let q = p; q; q = q.parent) if (q === n) return true;
    return false;
  }

  // keep, when set, is closed under ancestors (see keepSet), so skipping a
  // node outside it never orphans a kept descendant.
  function visibleChildren(node, showHidden, out, keep) {
    out = out || [];
    for (const c of node.children) {
      if (keep && !keep.has(c)) continue;
      if (c.hidden && !showHidden) visibleChildren(c, showHidden, out, keep);
      else out.push(c);
    }
    return out;
  }

  function foldKey(n) {
    const c = n.span.component;
    return SERVICE[c] ? 'c:' + c : 'n:' + n.span.name;
  }

  function commonName(members) {
    let p = members[0].span.name || '';
    for (const m of members) {
      const name = m.span.name || '';
      let i = 0;
      while (i < p.length && i < name.length && p[i] === name[i]) i++;
      p = p.slice(0, i);
      if (!p) break;
    }
    p = p.replace(/[^A-Za-z0-9]+$/, '');
    if (p.length >= 2) return p;
    return COMPONENT_LABEL[members[0].span.component] || members[0].span.name;
  }

  function displayName(n, depth) {
    const name = n.span.name || '(unnamed)';
    return depth > 0 && name.startsWith('magento.') ? name.slice(8) : name;
  }

  function frac(v, total) {
    return Math.min(1, Math.max(0, v / total));
  }

  function rows(tree, opts) {
    if (!tree) return [];
    opts = opts || {};
    const showHidden = !!opts.showHidden;
    const collapsed = opts.collapsed || new Set();
    const expanded = opts.expanded || new Set();
    const keep = opts.keep || null;
    const total = tree.total;
    const out = [];
    const kidsOf = new Map();

    function kids(n) {
      let k = kidsOf.get(n);
      if (!k) {
        k = visibleChildren(n, showHidden, [], keep);
        kidsOf.set(n, k);
      }
      return k;
    }

    function spanRow(n, depth, prefix) {
      const left = frac(n.start, total);
      const toggle = kids(n).length > 0;
      return {
        key: n.key,
        kind: 'span',
        depth,
        prefix,
        name: displayName(n, depth),
        detail: describe(n.span),
        component: n.span.component || 'php',
        spanKind: kindOf(n.span),
        error: !!n.span.error,
        left,
        width: Math.min(frac(n.dur, total), 1 - left),
        duration: n.dur,
        self: n.self,
        count: 1,
        toggle,
        open: toggle && !collapsed.has(n.key),
        index: n.i,
        members: null,
      };
    }

    function group(list) {
      const items = [];
      let i = 0;
      while (i < list.length) {
        const n = list[i];
        if (kids(n).length === 0) {
          const k = foldKey(n);
          let j = i + 1;
          while (j < list.length && kids(list[j]).length === 0 && foldKey(list[j]) === k) j++;
          if (j - i >= FOLD_MIN) {
            items.push(list.slice(i, j));
            i = j;
            continue;
          }
        }
        items.push(n);
        i++;
      }
      return items;
    }

    function foldRow(parent, members, depth, prefix) {
      let lo = Infinity;
      let hi = -Infinity;
      let sum = 0;
      for (const m of members) {
        if (m.start < lo) lo = m.start;
        if (m.start + m.dur > hi) hi = m.start + m.dur;
        sum += m.dur;
      }
      const key = 'fold:' + parent.key + ':' + members[0].key;
      const open = expanded.has(key);
      const left = frac(lo, total);
      return {
        key,
        kind: 'fold',
        depth,
        prefix,
        name: commonName(members) + ' × ' + members.length,
        detail: open ? '' : 'collapsed',
        component: members[0].span.component || 'php',
        spanKind: kindOf(members[0].span),
        error: members.some((m) => m.span.error),
        left,
        width: Math.min(frac(hi - lo, total), 1 - left),
        duration: sum,
        self: sum,
        count: members.length,
        toggle: true,
        open,
        index: -1,
        members: members.map((m) => m.i),
      };
    }

    function walkChildren(parent, depth, guide) {
      const items = group(kids(parent));
      for (let i = 0; i < items.length; i++) {
        const last = i === items.length - 1;
        const prefix = guide + (last ? '└ ' : '├ ');
        const next = guide + (last ? '  ' : '│ ');
        const it = items[i];
        if (Array.isArray(it)) {
          const row = foldRow(parent, it, depth, prefix);
          out.push(row);
          if (row.open) {
            for (let j = 0; j < it.length; j++) {
              out.push(spanRow(it[j], depth + 1, next + (j === it.length - 1 ? '└ ' : '├ ')));
            }
          }
        } else {
          walk(it, depth, prefix, next);
        }
      }
    }

    function walk(n, depth, prefix, guide) {
      const row = spanRow(n, depth, prefix);
      out.push(row);
      if (row.open) walkChildren(n, depth + 1, guide);
    }

    walk(tree.root, 0, '', '');
    return out;
  }

  function foldSummary(tree, members) {
    const by = new Map();
    for (const i of members || []) {
      const n = tree.nodes[i];
      if (!n) continue;
      const text = describe(n.span) || n.span.name;
      const e = by.get(text) || { text, count: 0, total: 0, max: 0 };
      e.count++;
      e.total += n.dur;
      if (n.dur > e.max) e.max = n.dur;
      by.set(text, e);
    }
    return Array.from(by.values()).sort((a, b) => b.count - a.count || b.total - a.total);
  }

  function byType(trace) {
    const times = (trace && trace.times) || {};
    const counts = (trace && trace.counts) || {};
    const total = trace && trace.duration > 0 ? trace.duration : 1;
    const keys = new Set(Object.keys(times).concat(Object.keys(counts)));
    const out = [];
    for (const c of keys) {
      const self = times[c] > 0 ? times[c] : 0;
      const count = counts[c] > 0 ? counts[c] : 0;
      if (!self && !count) continue;
      out.push({ component: c, self, count, share: self / total });
    }
    return out.sort((a, b) => b.self - a.self || b.count - a.count);
  }

  function blocks(tree) {
    const out = { blocks: 0, templates: 0, self: 0 };
    if (!tree) return out;
    for (const n of tree.nodes) {
      const name = n.span.name;
      if (name === 'magento.block.render') out.blocks++;
      else if (name === 'magento.template.render') out.templates++;
      else continue;
      out.self += n.self;
    }
    return out;
  }

  function topNames(tree, limit) {
    if (!tree) return [];
    const by = new Map();
    for (const n of tree.nodes) {
      const name = n.span.name || '(unnamed)';
      const e = by.get(name) || { name, component: n.span.component || 'php', count: 0, self: 0, max: 0 };
      e.count++;
      e.self += n.self;
      if (n.dur > e.max) e.max = n.dur;
      by.set(name, e);
    }
    return Array.from(by.values())
      .sort((a, b) => b.self - a.self || b.count - a.count)
      .slice(0, limit || 15);
  }

  function toFlame(tree) {
    if (!tree) return null;
    const made = new Map();
    for (const n of tree.nodes) {
      made.set(n, {
        key: n.key,
        name: n.span.name || '(unnamed)',
        detail: describe(n.span),
        component: n.span.component || 'php',
        value: n === tree.root ? Math.max(tree.total, n.dur) : n.dur,
        self: n.self,
        start: n.start,
        children: [],
        parent: null,
      });
    }
    for (const n of tree.nodes) {
      const f = made.get(n);
      for (const c of n.children) {
        const fc = made.get(c);
        fc.parent = f;
        f.children.push(fc);
      }
    }
    return made.get(tree.root);
  }

  function attrList(span) {
    const a = (span && span.attrs) || {};
    return Object.keys(a)
      .filter((k) => !k.startsWith('_dd.') && k !== '_sampling_priority_v1')
      .sort()
      .map((k) => ({ k, v: String(a[k]) }));
  }

  // The same classification the server's analysis uses (apm.kindOf), so a
  // span's type reads the same in Bottlenecks, Hotspots and the Spans table.
  function kindOf(span) {
    const c = (span && span.component) || '';
    if (c === 'sql' || c === 'cache' || c === 'search' || c === 'http') return c;
    if (c === 'magento') {
      const name = span.name || '';
      if (name.startsWith('magento.plugin')) return 'plugin';
      if (name === 'magento.event.execute') return 'observer';
      if (name.startsWith('magento.event')) return 'event';
      if (name.startsWith('magento.block')) return 'block';
      if (name.startsWith('magento.template')) return 'template';
    }
    return 'php';
  }

  function addAncestors(keep, n) {
    for (let q = n; q && !keep.has(q); q = q.parent) keep.add(q);
  }

  // The chain of the slowest child at each level, from the root down.
  function hotPath(tree) {
    const out = new Set();
    if (!tree) return out;
    let n = tree.root;
    while (n) {
      out.add(n);
      let best = null;
      for (const c of n.children) if (!best || c.dur > best.dur) best = c;
      n = best;
    }
    return out;
  }

  // Which nodes the waterfall keeps for a text filter, a minimum self time
  // (ns) and the hot path: the matches plus their ancestors, or null when
  // nothing narrows the tree.
  function keepSet(tree, opts) {
    if (!tree) return null;
    const text = String((opts && opts.text) || '').trim().toLowerCase();
    const minSelf = opts && opts.minSelf > 0 ? opts.minSelf : 0;
    const hot = !!(opts && opts.hot);
    let keep = null;
    let matched = null;
    if (text || minSelf) {
      keep = new Set();
      matched = [];
      for (const n of tree.nodes) {
        if (minSelf && n.self < minSelf) continue;
        if (text && !matchText(n.span, text)) continue;
        matched.push(n);
        addAncestors(keep, n);
      }
    }
    if (hot) {
      const path = hotPath(tree);
      if (keep) {
        for (const n of Array.from(keep)) if (!path.has(n)) keep.delete(n);
      } else {
        keep = path;
      }
    }
    if (keep) {
      // The tree view needs the ancestors; a flat list wants the matches only.
      keep.matched = matched ? new Set(matched.filter((n) => keep.has(n))) : keep;
      keep.matches = keep.matched.size;
    }
    return keep;
  }

  function matchText(span, text) {
    return String(span.name || '').toLowerCase().includes(text) || String(span.resource || '').toLowerCase().includes(text);
  }

  // Opens the way to one span: its ancestors are uncollapsed, a hidden span
  // switches hidden spans on, and the fold run that swallows it is expanded.
  // Mutates opts and returns the rows plus the span's row index (-1 when a
  // filter in opts.keep excludes it).
  function reveal(tree, key, opts) {
    const n = tree && tree.byKey.get(key);
    if (!n) return { rows: rows(tree, opts), index: -1 };
    opts.collapsed = opts.collapsed || new Set();
    opts.expanded = opts.expanded || new Set();
    for (let q = n.parent; q; q = q.parent) opts.collapsed.delete(q.key);
    if (n.hidden && !opts.showHidden) opts.showHidden = true;
    let list = rows(tree, opts);
    let index = list.findIndex((r) => r.key === n.key);
    if (index < 0) {
      const fold = list.find((r) => r.kind === 'fold' && r.members.includes(n.i));
      if (fold) {
        opts.expanded.add(fold.key);
        list = rows(tree, opts);
        index = list.findIndex((r) => r.key === n.key);
      }
    }
    return { rows: list, index };
  }

  const SPAN_SORT = {
    self: (a, b) => b.self - a.self || b.total - a.total,
    total: (a, b) => b.total - a.total || b.self - a.self,
    name: (a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : b.self - a.self),
    kind: (a, b) => (a.kind < b.kind ? -1 : a.kind > b.kind ? 1 : b.self - a.self),
    depth: (a, b) => a.depth - b.depth || b.self - a.self,
  };

  // The flat Spans table: every kept span, sorted, with the top `limit`
  // returned so a 7,000-span trace never renders 7,000 rows.
  function spanTable(tree, opts) {
    if (!tree) return { rows: [], total: 0 };
    opts = opts || {};
    const keep = opts.keep ? opts.keep.matched || opts.keep : null;
    const sortKey = SPAN_SORT[opts.sort] ? opts.sort : 'self';
    const list = [];
    for (const n of tree.nodes) {
      if (keep && !keep.has(n)) continue;
      list.push({ key: n.key, index: n.i, name: n.span.name || '(unnamed)', resource: resourceText(n.span), self: n.self, total: n.dur, kind: kindOf(n.span), depth: n.depth || 0 });
    }
    list.sort(SPAN_SORT[sortKey]);
    if (opts.dir === 'asc') list.reverse();
    const limit = opts.limit > 0 ? opts.limit : list.length;
    return { rows: list.slice(0, limit), total: list.length };
  }

  // The full statement or call a span made: the SQL text for queries, else
  // the resource when it says more than the name.
  function resourceText(span) {
    const a = (span && span.attrs) || {};
    const st = a['db.statement'] || a['db.query.text'];
    if (st) return String(st);
    const raw = a['redis.raw_command'];
    if (raw) return String(raw);
    const res = (span && span.resource) || '';
    return res && res !== span.name ? res : '';
  }

  // Self time and count per span kind, heaviest first.
  function kindTotals(tree) {
    if (!tree) return [];
    const by = new Map();
    for (const n of tree.nodes) {
      const k = kindOf(n.span);
      const e = by.get(k) || { kind: k, self: 0, count: 0 };
      e.self += n.self;
      e.count++;
      by.set(k, e);
    }
    return Array.from(by.values()).sort((a, b) => b.self - a.self || b.count - a.count);
  }

  // A span's direct children grouped by kind, for the side panel.
  function childGroups(node) {
    if (!node) return [];
    const by = new Map();
    for (const c of node.children) {
      const k = kindOf(c.span);
      const e = by.get(k) || { kind: k, count: 0, total: 0 };
      e.count++;
      e.total += c.dur;
      by.set(k, e);
    }
    return Array.from(by.values()).sort((a, b) => b.total - a.total);
  }

  const COST_ORDER = ['sql', 'cache', 'search', 'http', 'php'];
  const COST_LABEL = { sql: 'SQL', cache: 'Cache', search: 'Search', http: 'HTTP', php: 'PHP' };

  // The cost strip: one entry per category in stack order, from ms values.
  // A category without time gets no segment, because the strip's minimum
  // segment width would otherwise paint time that was never spent.
  function costSegments(times, totalMs) {
    const t = times || {};
    let sum = 0;
    for (const k of COST_ORDER) sum += t[k] > 0 ? t[k] : 0;
    const total = totalMs > 0 ? Math.max(totalMs, sum) : sum;
    return COST_ORDER.map((k) => {
      const ms = t[k] > 0 ? t[k] : 0;
      const share = total > 0 ? ms / total : 0;
      return { key: k, label: COST_LABEL[k], ms, share, width: (share * 100).toFixed(3) + '%', shown: ms > 0 && share > 0 };
    });
  }

  function msLabel(ms) {
    if (!(ms > 0)) return '0 ms';
    if (ms < 0.005) return '<0.01 ms';
    if (ms < 1) return ms.toFixed(2) + ' ms';
    if (ms < 10) return ms.toFixed(1) + ' ms';
    if (ms < 1000) return Math.round(ms) + ' ms';
    return (ms / 1000).toFixed(2) + ' s';
  }

  function shareLabel(share) {
    if (!(share > 0)) return '0%';
    if (share < 0.001) return '<0.1%';
    return (share * 100).toFixed(share < 0.1 ? 1 : 0) + '%';
  }

  // What a view binds for the strip: classes, an object for :style (the CSP
  // blocks style strings) and the hover text, plus one label for the whole.
  function strip(times, totalMs) {
    const segs = costSegments(times, totalMs).filter((s) => s.shown);
    return {
      segments: segs.map((s) => ({ key: s.key, cls: 'cost-seg cat-' + s.key, style: { width: s.width }, title: s.label + ' · ' + msLabel(s.ms) + ' · ' + shareLabel(s.share) })),
      label: segs.length ? 'Where the time went: ' + segs.map((s) => s.label + ' ' + shareLabel(s.share)).join(', ') : 'No timing yet',
    };
  }

  function findFlame(root, key) {
    if (!root || !key) return null;
    const stack = [root];
    while (stack.length) {
      const f = stack.pop();
      if (f.key === key) return f;
      for (const c of f.children) stack.push(c);
    }
    return null;
  }

  return {
    COST_ORDER,
    COST_LABEL,
    kindOf,
    hotPath,
    keepSet,
    reveal,
    spanTable,
    resourceText,
    kindTotals,
    childGroups,
    costSegments,
    strip,
    msLabel,
    shareLabel,
    findFlame,
    FOLD_MIN,
    parseTime,
    offsetNs,
    isHidden,
    describe,
    buildTree,
    rows,
    foldSummary,
    byType,
    blocks,
    topNames,
    toFlame,
    attrList,
  };
});
