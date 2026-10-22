(function (root, factory) {
  const isNode = typeof module === 'object' && module.exports;
  const deps = isNode
    ? { fmt: require('../lib/format.js'), route: require('../lib/route.js'), waterfall: require('../lib/waterfall.js'), flame: require('../lib/flame.js') }
    : root.Inspector;
  const mod = factory(deps);
  if (isNode) {
    module.exports = mod;
  } else {
    root.Inspector.traces = mod;
    document.addEventListener('alpine:init', () => mod.register(window.Alpine, root.Inspector));
  }
})(this, function (deps) {
  const fmt = deps.fmt;
  const route = deps.route;
  const wf = deps.waterfall;
  const flame = deps.flame;

  const SUB = 'php:traces';
  const LIST_LIMIT = 500;
  const HOTSPOT_LIMIT = 100;
  const HOTSPOT_MS = 1500;
  const OPS_SHOWN = 15;
  const HOT_SHOWN = 10;
  const PATH_TAIL = 3;
  const SPAN_ROWS = 200;
  const ROW_H = 22;
  const WF_MIN_H = 320;
  const OVERSCAN = 12;
  const FLAME_ROW = 18;
  const FLAME_HOT = 0.05;
  const FLAME_WARM = 0.02;
  const PROFILE_CACHE = 20;
  const TYPE_MS = 250;
  const NS_PER_MS = 1e6;

  const KIND_LABEL = {
    sql: 'SQL',
    cache: 'Cache',
    search: 'Search',
    http: 'HTTP',
    plugin: 'Plugin',
    observer: 'Observer',
    event: 'Event',
    block: 'Block',
    template: 'Template',
    php: 'PHP',
  };
  const TABS = [
    { id: 'bottlenecks', label: 'Bottlenecks' },
    { id: 'waterfall', label: 'Waterfall' },
    { id: 'flame', label: 'Flame graph' },
    { id: 'types', label: 'By type' },
    { id: 'spans', label: 'Spans' },
  ];
  const ROUTE_COLS = [
    { id: 'route', label: 'Route', num: false, cls: 'w-[40%]' },
    { id: 'count', label: 'Requests', num: true, cls: 'hidden sm:table-cell' },
    { id: 'p50', label: 'p50', num: true, cls: 'hidden md:table-cell' },
    { id: 'p95', label: 'p95', num: true, cls: '' },
    { id: 'max', label: 'Max', num: true, cls: 'hidden md:table-cell' },
  ];
  const SPAN_COLS = [
    { id: 'name', label: 'Span', num: false, cls: '' },
    { id: 'self', label: 'Self', num: true, cls: '' },
    { id: 'total', label: 'Total', num: true, cls: '' },
    { id: 'kind', label: 'Type', num: false, cls: 'hidden sm:table-cell' },
    { id: 'depth', label: 'Depth', num: true, cls: 'hidden sm:table-cell' },
  ];
  const MIN_CHIPS = [
    { id: '100', label: '> 100 ms' },
    { id: '500', label: '> 500 ms' },
    { id: '1000', label: '> 1 s' },
  ];
  const SELF_STEPS = [
    { id: '', label: 'Any self time' },
    { id: '0.5', label: 'Self ≥ 0.5 ms' },
    { id: '1', label: 'Self ≥ 1 ms' },
    { id: '5', label: 'Self ≥ 5 ms' },
    { id: '25', label: 'Self ≥ 25 ms' },
    { id: '100', label: 'Self ≥ 100 ms' },
  ];
  // Waterfall and flame bars: the service calls take their cost-category
  // colour so they stand out against the muted PHP code around them.
  const BAR = { sql: 'cat-sql', cache: 'cat-cache', search: 'cat-search', http: 'cat-http' };
  const FILL = { sql: 'fill-cat-sql', cache: 'fill-cat-cache', search: 'fill-cat-search', http: 'fill-cat-http', queue: 'fill-rabbitmq-bar' };
  const FLAME_RECT = 'cursor-pointer hover:opacity-80 ';
  const FLAME_ANCESTOR = 'cursor-pointer opacity-50 hover:opacity-70 ';
  const FLAME_LABEL = 'pointer-events-none fill-chalk font-mono text-[11px]';
  const NO_PANEL = Object.freeze({ title: '', kind: '', kindLabel: '', code: '', isSql: false, facts: [], groups: [], attrs: [], selfStyle: { width: '0%' }, selfText: '' });
  const profiles = new Map();

  // ---- formatting ----

  const msText = wf.msLabel;
  const pctText = wf.shareLabel;

  function nsText(ns) {
    return msText(ns / NS_PER_MS);
  }

  // Heat marks cost and nothing else: the rank badge, the share bar and the
  // hottest rows take their level from the share of time.
  function heatOf(share) {
    if (share >= 0.15) return '4';
    if (share >= 0.08) return '3';
    if (share >= 0.03) return '2';
    return share > 0 ? '1' : '0';
  }

  function barWidth(share) {
    return { width: (Math.max(0, Math.min(1, share || 0)) * 100).toFixed(2) + '%' };
  }

  // Ranked lists heat against their own leader: shares of all traced time are
  // a few percent at most, which would leave every row but the first cold.
  function heatOfRank(rel) {
    if (rel >= 0.6) return '4';
    if (rel >= 0.35) return '3';
    if (rel >= 0.15) return '2';
    return rel > 0 ? '1' : '0';
  }

  // The nearest ancestors say most about a span, so a long chain keeps its
  // root and its tail and drops the middle.
  function shortPath(path) {
    const p = path || [];
    if (p.length <= PATH_TAIL + 2) return p.join(' › ');
    return [p[0], '…'].concat(p.slice(-PATH_TAIL)).join(' › ');
  }

  function costKey() {
    return wf.COST_ORDER.map((k) => ({ key: k, label: wf.COST_LABEL[k], swatch: 'cost-swatch cat-' + k }));
  }

  // PHP frames are one colour, so the frames that spend real time in their
  // own code get a heat outline: that is where a flame graph's answer is. An
  // outline, not a fill, because heat orange next to the cache orange would
  // read as a cache call.
  function flameHeat(n, root) {
    const share = root && root.value > 0 ? (n.self || 0) / root.value : 0;
    if (share >= FLAME_HOT) return 'fill-php-bar stroke-heat-4 stroke-2';
    if (share >= FLAME_WARM) return 'fill-php-bar stroke-heat-3 stroke-1';
    return 'fill-php-bar';
  }

  function shortName(name) {
    const s = String(name || '');
    return s.startsWith('magento.') ? s.slice(8) : s;
  }

  function kindLabel(kind) {
    return KIND_LABEL[kind] || kind || 'PHP';
  }

  function urlPath(u) {
    try {
      const x = new URL(u);
      return x.pathname + x.search;
    } catch (e) {
      return u || '';
    }
  }

  function minSelfNs(v) {
    const n = Number(v);
    return n > 0 ? n * NS_PER_MS : 0;
  }

  // ---- cost strip ----

  function strip(times, totalMs) {
    return wf.strip(times, totalMs).segments;
  }

  function stripLabel(times, totalMs) {
    return wf.strip(times, totalMs).label;
  }

  function legend(times, totalMs) {
    return wf.costSegments(times, totalMs).map((s) => ({
      key: s.key,
      label: s.label,
      swatch: 'cost-swatch cat-' + s.key,
      ms: msText(s.ms),
      pct: pctText(s.share),
      zero: s.shown ? 'false' : 'true',
    }));
  }

  function timesOfCategories(categories) {
    const out = {};
    for (const c of categories || []) out[c.key] = c.ms;
    return out;
  }

  // ---- hash ----

  function withQuery(r, patch, base) {
    const query = Object.assign({}, base === undefined ? r.query : base, patch);
    for (const k of Object.keys(query)) if (query[k] === '' || query[k] === null || query[k] === undefined) delete query[k];
    return route.format({ tab: r.tab, section: r.section, rest: r.rest, query });
  }

  function sortHeaders(cols, sort, dir, defaults, hrefFor) {
    return cols.map((c) => {
      const cur = c.id === sort;
      const d = cur ? dir : defaults(c.id);
      const next = cur ? (dir === 'asc' ? 'desc' : 'asc') : defaults(c.id);
      return {
        id: c.id,
        label: c.label,
        cls: (c.num ? 'num ' : '') + c.cls,
        ariaSort: cur ? (d === 'asc' ? 'ascending' : 'descending') : 'none',
        arrow: cur ? (d === 'asc' ? '↑' : '↓') : '',
        href: hrefFor(c.id, next),
      };
    });
  }

  // ---- hotspots ----

  function routeDefaultDir(id) {
    return id === 'route' ? 'asc' : 'desc';
  }

  function sortRoutes(list, sort, dir) {
    const key = { count: 'count', p50: 'p50Ms', p95: 'p95Ms', max: 'maxMs' }[sort];
    const out = list.slice();
    if (sort === 'route') out.sort((a, b) => (a.route < b.route ? -1 : a.route > b.route ? 1 : 0));
    else if (key) out.sort((a, b) => a[key] - b[key] || (a.route < b.route ? 1 : -1));
    else return out;
    if ((dir || routeDefaultDir(sort)) === 'desc') out.reverse();
    return out;
  }

  function routeRows(hs, sort, dir) {
    const sorted = sortRoutes((hs && hs.routes) || [], sort || 'p95', dir);
    return sorted.map((r) => {
      const t = r.times || {};
      let total = 0;
      for (const k of wf.COST_ORDER) total += t[k] > 0 ? t[k] : 0;
      return {
        key: r.route,
        route: r.route || '(no route)',
        href: route.format({ tab: 'traces', section: 'requests', query: { route: r.route } }),
        worstHref: r.worstId ? route.format({ tab: 'traces', section: r.worstId }) : '',
        count: fmt.num(r.count),
        p50: msText(r.p50Ms),
        p95: msText(r.p95Ms),
        max: msText(r.maxMs),
        strip: strip(t, total),
        stripLabel: stripLabel(t, total),
      };
    });
  }

  function operationRows(hs) {
    const list = (hs && hs.operations) || [];
    const lead = list.reduce((m, o) => Math.max(m, o.selfMs || 0), 0);
    return list.map((o, i) => {
      const rel = lead > 0 ? (o.selfMs || 0) / lead : 0;
      const heat = heatOfRank(rel);
      return {
        key: o.kind + ':' + o.key + ':' + i,
        rank: String(i + 1),
        heat,
        kind: o.kind,
        kindLabel: kindLabel(o.kind),
        text: o.key,
        title: o.example || o.key,
        self: msText(o.selfMs),
        share: pctText(o.share),
        shareStyle: barWidth(rel),
        calls: fmt.num(o.calls),
        traces: fmt.num(o.traces),
        href: o.worstId ? route.format({ tab: 'traces', section: o.worstId, rest: o.worstSpanId ? 'waterfall' : '', query: { focus: o.worstSpanId } }) : '',
      };
    });
  }

  function repeatRows(hs) {
    return ((hs && hs.repeats) || []).map((r, i) => ({
      key: r.kind + ':' + r.key + ':' + i,
      kind: r.kind,
      kindLabel: kindLabel(r.kind),
      text: r.key,
      perRequest: '×' + fmt.num(Math.round(r.perRequest)),
      msPerRequest: msText(r.msPerRequest),
      traces: fmt.num(r.traces),
      routes: (r.routes || []).map((x) => ({ key: x, label: x || '(no route)', href: route.format({ tab: 'traces', section: 'requests', query: { route: x } }) })),
      href: r.worstId ? route.format({ tab: 'traces', section: r.worstId }) : '',
    }));
  }

  // ---- requests ----

  function requestFilter(q) {
    q = q || {};
    return { route: q.route || '', url: q.url || '', min: Number(q.min) > 0 ? Number(q.min) : 0, errors: q.errors === '1', sort: q.sort === 'time' ? 'time' : 'duration' };
  }

  function filterRequests(items, f) {
    const text = String(f.url || '').toLowerCase();
    const minNs = f.min * NS_PER_MS;
    const out = items.filter((s) => {
      if (f.route && s.route !== f.route) return false;
      if (minNs && !(s.duration > minNs)) return false;
      if (f.errors && !(s.status >= 500)) return false;
      if (text && !String(s.url || '').toLowerCase().includes(text)) return false;
      return true;
    });
    if (f.sort === 'time') out.sort((a, b) => wf.offsetNs(b.start, a.start));
    else out.sort((a, b) => b.duration - a.duration);
    return out;
  }

  function requestRow(s) {
    const c = s.counts || {};
    const status = s.status || 0;
    const totalMs = s.duration / NS_PER_MS;
    return {
      id: s.id,
      href: route.format({ tab: 'traces', section: s.id }),
      time: fmt.clock(s.start).slice(0, 8),
      method: s.method || '',
      route: s.route || '(no route)',
      path: urlPath(s.url),
      title: s.url || '',
      status: status ? String(status) : '–',
      statusCls: status >= 500 ? 'tone-error' : status >= 400 ? 'tone-warn' : 'text-fog',
      duration: msText(totalMs),
      strip: strip(s.times, totalMs),
      stripLabel: stripLabel(s.times, totalMs),
      sql: fmt.num(c.sql || 0),
      cache: fmt.num(c.cache || 0),
      search: fmt.num(c.search || 0),
      nPlusOne: !!s.nPlusOne,
      profile: !!s.hasProfile,
    };
  }

  function mergeSummary(items, s, sort) {
    const next = items.filter((x) => x.id !== s.id);
    next.push(s);
    if (sort === 'time') next.sort((a, b) => wf.offsetNs(b.start, a.start));
    else next.sort((a, b) => b.duration - a.duration);
    return next.slice(0, LIST_LIMIT);
  }

  // The list loads every stored trace, so its routes are all the routes
  // Hotspots could name; fetching Hotspots for them would only cost.
  function routeOptions(current, items) {
    const set = new Set();
    for (const s of items || []) if (s.route) set.add(s.route);
    if (current) set.add(current);
    const list = Array.from(set).sort();
    return [{ value: '', label: 'Every route', selected: !current }].concat(list.map((r) => ({ value: r, label: r, selected: r === current })));
  }

  // ---- bottlenecks ----

  function showHref(id, spanId, extra) {
    return route.format({ tab: 'traces', section: id, rest: 'waterfall', query: Object.assign({ focus: spanId }, extra || {}) });
  }

  function hotRows(a, focus) {
    if (!a) return [];
    return (a.hot || []).map((h) => ({
      key: h.rank + ':' + h.spanId,
      rank: String(h.rank),
      heat: heatOf(h.share),
      kind: h.kind,
      kindLabel: kindLabel(h.kind),
      name: shortName(h.name),
      resource: h.resource && h.resource !== h.name ? h.resource : '',
      self: msText(h.selfMs),
      total: msText(h.totalMs),
      share: pctText(h.share),
      shareStyle: barWidth(h.share),
      calls: h.calls > 1 ? '×' + fmt.num(h.calls) : '',
      path: shortPath(h.path),
      fullPath: (h.path || []).join(' › '),
      hasPath: (h.path || []).length > 0,
      focused: focus && (h.spanIds || [h.spanId]).includes(focus) ? 'true' : 'false',
      href: showHref(a.id, h.spanId),
    }));
  }

  function slowestMember(tree, ids, fallback) {
    let best = null;
    for (const id of ids || []) {
      const n = tree && tree.byKey.get(id);
      if (n && (!best || n.dur > best.dur)) best = n;
    }
    return best ? best.key : fallback;
  }

  function repeatList(a, tree, focus) {
    if (!a) return [];
    return (a.repeats || []).map((r, i) => {
      const spanId = slowestMember(tree, r.spanIds, (r.spanIds || [])[0] || '');
      return {
        key: r.kind + ':' + i,
        kind: r.kind,
        kindLabel: kindLabel(r.kind),
        text: r.key,
        example: r.example || '',
        count: '×' + fmt.num(r.count),
        formula: fmt.num(r.count) + ' × ' + msText(r.avgMs) + ' = ' + msText(r.totalMs),
        total: msText(r.totalMs),
        share: a.duration > 0 ? pctText(r.totalMs / a.duration) : '',
        heat: heatOf(a.duration > 0 ? r.totalMs / a.duration : 0),
        focused: focus && (r.spanIds || []).includes(focus) ? 'true' : 'false',
        href: spanId ? showHref(a.id, spanId) : '',
      };
    });
  }

  function slowestGroups(a, focus) {
    if (!a) return [];
    const by = new Map();
    for (const s of a.slowest || []) {
      let g = by.get(s.kind);
      if (!g) {
        g = { key: s.kind, kind: s.kind, label: kindLabel(s.kind), items: [] };
        by.set(s.kind, g);
      }
      g.items.push({
        key: s.spanId,
        resource: s.resource || '(no resource)',
        ms: msText(s.ms),
        focused: s.spanId === focus ? 'true' : 'false',
        label: 'Show in waterfall: ' + (s.resource || s.kind),
        href: showHref(a.id, s.spanId),
      });
    }
    return Array.from(by.values());
  }

  // ---- trace detail ----

  function tabList(r, mode) {
    return TABS.map((t) => ({
      id: t.id,
      label: t.label,
      href: route.format({ tab: 'traces', section: r.section, rest: t.id === 'bottlenecks' ? '' : t.id, query: r.query }),
      current: t.id === mode ? 'page' : null,
    }));
  }

  function spanDefaultDir(id) {
    return id === 'name' || id === 'kind' || id === 'depth' ? 'asc' : 'desc';
  }

  // A row is clickable as a whole, but a link inside it goes where it says.
  function fromLink(ev) {
    return !!(ev && ev.target && ev.target.closest && ev.target.closest('a'));
  }

  function typing(target) {
    if (!target || !target.tagName) return false;
    const tag = target.tagName;
    return tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT' || !!target.isContentEditable;
  }

  function register(Alpine, I) {
    const store = () => Alpine.store('app');

    function go(h, replace) {
      if (replace) {
        window.history.replaceState(null, '', h);
        store().route = route.parse(h);
      } else {
        window.location.hash = h;
      }
    }

    function setQuery(patch, replace) {
      go(withQuery(store().route, patch), replace);
    }

    function blocked() {
      return Alpine.store('palette').open || !!Alpine.store('confirm').request || !!Alpine.store('drawer').pending;
    }

    async function copy(text, what) {
      try {
        await navigator.clipboard.writeText(text);
        I.toast('Copied ' + what, 'ok');
      } catch (e) {
        I.toast('Copy failed. Select the text and copy it by hand.', 'warn');
      }
    }

    Alpine.data('traces', () => ({
      get page() {
        return route.traceView(store().route).page;
      },
      isPage(p) {
        return this.page === p;
      },
    }));

    Alpine.data('traceHotspots', () => {
      let off = null;
      let timer = null;
      let last = 0;
      return {
        data: null,
        state: 'loading',
        error: '',

        init() {
          this.load();
          off = I.stream.subscribe([SUB], (ev) => this.receive(ev));
        },

        destroy() {
          if (off) off();
          clearTimeout(timer);
        },

        async load() {
          last = Date.now();
          try {
            this.data = await I.client.get('/api/traces/hotspots', { query: { limit: HOTSPOT_LIMIT }, quiet: true });
            this.state = 'ready';
            this.error = '';
          } catch (e) {
            if (!this.data) this.state = 'error';
            this.error = e.message;
          }
        },

        // The server recomputes hotspots at most once a second, so a burst of
        // traces costs one reload.
        receive(ev) {
          if ((ev.type !== 'trace' && ev.type !== 'gap') || timer) return;
          timer = setTimeout(() => {
            timer = null;
            this.load();
          }, Math.max(250, HOTSPOT_MS - (Date.now() - last)));
        },

        get loading() {
          return this.state === 'loading';
        },
        get failed() {
          return this.state === 'error';
        },
        get empty() {
          return this.state === 'ready' && !(this.data && this.data.traces > 0);
        },
        get hasData() {
          return this.state === 'ready' && !this.empty;
        },
        get summary() {
          const d = this.data;
          if (!d || !d.traces) return '';
          const since = d.since ? ' since ' + fmt.clock(d.since).slice(0, 8) : '';
          return 'Across ' + fmt.num(d.traces) + (d.traces === 1 ? ' traced request' : ' traced requests') + since + '.';
        },
        get sort() {
          return store().route.query.sort || 'p95';
        },
        get dir() {
          return store().route.query.dir || '';
        },
        get routeHeaders() {
          const r = store().route;
          return sortHeaders(ROUTE_COLS, this.sort, this.dir || routeDefaultDir(this.sort), routeDefaultDir, (id, d) =>
            withQuery(r, { sort: id === 'p95' ? '' : id, dir: d === routeDefaultDir(id) ? '' : d }),
          );
        },
        get routes() {
          return routeRows(this.data, this.sort, this.dir);
        },
        get allOperations() {
          return operationRows(this.data);
        },
        get opsAll() {
          return store().route.query.ops === 'all';
        },
        get operations() {
          const all = this.allOperations;
          return this.opsAll ? all : all.slice(0, OPS_SHOWN);
        },
        get opsMore() {
          return this.allOperations.length > OPS_SHOWN;
        },
        get opsMoreText() {
          return this.opsAll ? 'Show the top ' + OPS_SHOWN : 'Show all ' + fmt.num(this.allOperations.length) + ' operations';
        },
        toggleOps() {
          setQuery({ ops: this.opsAll ? '' : 'all' }, true);
        },
        get repeats() {
          return repeatRows(this.data);
        },
        get jumps() {
          return [
            { id: 'hs-routes', label: 'Routes', count: fmt.num(((this.data && this.data.routes) || []).length) },
            { id: 'hs-ops', label: 'Expensive operations', count: fmt.num(this.allOperations.length) },
            { id: 'hs-repeats', label: 'N+1', count: fmt.num(this.repeats.length) },
          ];
        },
        jump(id) {
          const el = document.getElementById(id);
          if (el) el.scrollIntoView({ block: 'start' });
        },
        get costKey() {
          return costKey();
        },
        get hasOperations() {
          return this.allOperations.length > 0;
        },
        get hasRepeats() {
          return this.repeats.length > 0;
        },
        get requestsHref() {
          return route.format({ tab: 'traces', section: 'requests' });
        },
        openRoute(r, ev) {
          if (fromLink(ev) || !r.href) return;
          window.location.hash = r.href;
        },
      };
    });

    Alpine.data('traceRequests', () => {
      let off = null;
      let onKey = null;
      let fx = null;
      let urlTimer = null;
      return {
        items: [],
        state: 'loading',
        error: '',
        urlDraft: '',

        get costKey() {
          return costKey();
        },

        init() {
          this.load();
          off = I.stream.subscribe([SUB], (ev) => this.receive(ev));
          onKey = (e) => this.key(e);
          window.addEventListener('keydown', onKey);
          fx = Alpine.effect(() => {
            const u = store().route.query.url || '';
            if (document.activeElement !== this.$refs.url) this.urlDraft = u;
          });
        },

        destroy() {
          if (off) off();
          if (onKey) window.removeEventListener('keydown', onKey);
          if (fx) Alpine.release(fx);
          clearTimeout(urlTimer);
        },

        async load() {
          try {
            const list = await I.client.get('/api/traces', { query: { limit: LIST_LIMIT, sort: 'recent' }, quiet: true });
            this.items = Array.isArray(list) ? list : [];
            this.state = 'ready';
            this.error = '';
          } catch (e) {
            if (!this.items.length) this.state = 'error';
            this.error = e.message;
          }
        },

        receive(ev) {
          if (ev.type === 'gap') {
            this.load();
            return;
          }
          if (ev.type !== 'trace' || !ev.data || !ev.data.id) return;
          this.items = mergeSummary(this.items, ev.data, 'time');
          this.state = 'ready';
        },

        get filter() {
          return requestFilter(store().route.query);
        },
        get filtered() {
          return filterRequests(this.items, this.filter);
        },
        get rows() {
          return this.filtered.map(requestRow);
        },
        get loading() {
          return this.state === 'loading';
        },
        get failed() {
          return this.state === 'error';
        },
        get noTraces() {
          return this.state === 'ready' && this.items.length === 0;
        },
        get noMatch() {
          return this.state === 'ready' && this.items.length > 0 && this.filtered.length === 0;
        },
        get hasRows() {
          return this.rows.length > 0;
        },
        get countText() {
          const n = this.filtered.length;
          const all = this.items.length;
          if (!all) return '';
          return n === all ? fmt.num(all) + (all === 1 ? ' request' : ' requests') : fmt.num(n) + ' of ' + fmt.num(all) + ' requests';
        },
        get routeOptions() {
          return routeOptions(this.filter.route, this.items);
        },
        get minChips() {
          const cur = String(this.filter.min || '');
          return MIN_CHIPS.map((c) => ({ id: c.id, label: c.label, pressed: c.id === cur ? 'true' : 'false' }));
        },
        get errorsPressed() {
          return this.filter.errors ? 'true' : 'false';
        },
        get sortButtons() {
          const s = this.filter.sort;
          return [
            { id: 'duration', label: 'Slowest' },
            { id: 'time', label: 'Newest' },
          ].map((b) => ({ id: b.id, label: b.label, pressed: b.id === s ? 'true' : 'false' }));
        },
        get isFiltered() {
          const f = this.filter;
          return !!(f.route || f.url || f.min || f.errors);
        },
        get clearHref() {
          return route.format({ tab: 'traces', section: 'requests' });
        },
        setRoute(ev) {
          setQuery({ route: ev.target.value });
        },
        onUrl(ev) {
          this.urlDraft = ev.target.value;
          clearTimeout(urlTimer);
          urlTimer = setTimeout(() => setQuery({ url: this.urlDraft.trim() }, true), TYPE_MS);
        },
        setMin(c) {
          setQuery({ min: c.pressed === 'true' ? '' : c.id });
        },
        toggleErrors() {
          setQuery({ errors: this.filter.errors ? '' : '1' });
        },
        setSort(b) {
          setQuery({ sort: b.id === 'duration' ? '' : b.id });
        },
        clearFilters() {
          this.urlDraft = '';
        },
        open(r, ev) {
          if (fromLink(ev)) return;
          window.location.hash = r.href;
        },

        // j and k move the keyboard focus through the row links; Enter is the
        // link's own activation.
        key(e) {
          if (e.defaultPrevented || e.metaKey || e.ctrlKey || e.altKey) return;
          if (e.key !== 'j' && e.key !== 'k') return;
          if (typing(e.target) || blocked()) return;
          const links = Array.from(this.$el.querySelectorAll('a[data-row]'));
          if (!links.length) return;
          e.preventDefault();
          let i = links.indexOf(document.activeElement);
          if (e.key === 'j') i = i < 0 ? 0 : Math.min(links.length - 1, i + 1);
          else i = i < 0 ? 0 : Math.max(0, i - 1);
          links[i].focus();
          links[i].scrollIntoView({ block: 'nearest' });
        },
      };
    });

    Alpine.data('traceDetail', () => {
      let trace = null;
      let tree = null;
      let analysis = null;
      let rows = [];
      let rowByKey = new Map();
      let collapsed = new Set();
      let expanded = new Set();
      let keep = null;
      let showHidden = false;
      let curId = '';
      let filterSig = null;
      let lastFocus = null;
      let lastMode = '';
      let lastFold = '';
      let flameSig = '';
      let refocusFlame = false;
      let spanFlame = null;
      let flameRoot = null;
      let flameFocus = null;
      let flameUnit = 'ns';
      let fx = null;
      let off = null;
      let ctlTrace = null;
      let ctlAnalysis = null;
      let resize = null;
      let refetchTimer = null;
      let typeTimer = null;
      let frame = 0;
      let dead = false;
      let treeGen = 0;
      let spanMemo = null;

      return {
        traceState: 'loading',
        traceError: '',
        analysisState: 'loading',
        analysisError: '',
        version: 0,
        mode: 'bottlenecks',
        focus: '',
        qDraft: '',
        scrollTop: 0,
        viewportH: 480,
        wfMax: 0,
        flameState: 'idle',
        flameNote: '',
        flameHover: '',
        flameZoomed: false,
        flameSrc: 'spans',

        init() {
          off = I.stream.subscribe([SUB], (ev) => this.receive(ev));
          fx = Alpine.effect(() => {
            const r = store().route;
            // Applied outside the effect so the reactive reads in apply do not
            // become dependencies of it.
            Promise.resolve().then(() => this.apply(r));
          });
          this.$nextTick(() => this.observe());
        },

        destroy() {
          dead = true;
          if (fx) Alpine.release(fx);
          if (off) off();
          if (ctlTrace) ctlTrace.abort();
          if (ctlAnalysis) ctlAnalysis.abort();
          if (resize) resize.disconnect();
          clearTimeout(refetchTimer);
          clearTimeout(typeTimer);
          cancelAnimationFrame(frame);
        },

        observe() {
          if (typeof ResizeObserver !== 'function') return;
          resize = new ResizeObserver(() => {
            const vp = this.$refs.wf;
            if (vp && vp.clientHeight) this.viewportH = vp.clientHeight;
            if (this.mode === 'flame') this.schedulePaint();
          });
          if (this.$refs.wf) resize.observe(this.$refs.wf);
          if (this.$refs.flameBox) resize.observe(this.$refs.flameBox);
        },

        apply(r) {
          if (dead) return;
          const tv = route.traceView(r);
          if (tv.page !== 'trace') return;
          const q = r.query || {};
          this.mode = tv.mode;
          this.focus = q.focus || '';
          if (document.activeElement !== this.$refs.filter) this.qDraft = q.q || '';
          if (tv.id !== curId) {
            this.select(tv.id);
            return;
          }
          this.refresh();
        },

        refresh() {
          if (!tree || dead) return;
          // The shell clears the breadcrumb item whenever the path changes,
          // a tab switch included.
          const item = trace.route || urlPath(trace.url) || '';
          if (store().item !== item) store().item = item;
          const q = store().route.query || {};
          const sig = [q.q || '', q.min || '', q.hot || ''].join('|');
          const hidden = q.all === '1';
          if (sig !== filterSig || hidden !== showHidden) {
            if (sig !== filterSig) {
              collapsed = new Set();
              lastFold = '';
            }
            filterSig = sig;
            showHidden = hidden;
            keep = wf.keepSet(tree, { text: q.q, minSelf: minSelfNs(q.min), hot: q.hot === '1' });
            this.rebuild();
          }
          // Expand all and collapse all live in the hash (?fold=), so a reload
          // or a shared link opens the same tree; a single caret stays local.
          // reveal then opens the focused span's ancestors on top of it.
          const fold = q.fold || '';
          const folded = fold !== lastFold;
          if (folded) {
            lastFold = fold;
            if (fold) this.applyFold(fold);
          }
          const modeChanged = this.mode !== lastMode;
          if (this.focus !== lastFocus || modeChanged || (folded && fold)) {
            if (this.mode === 'waterfall') this.revealFocus(modeChanged);
            else if (this.mode === 'bottlenecks' || this.mode === 'spans') this.$nextTick(() => this.scrollFocused());
          }
          lastFocus = this.focus;
          lastMode = this.mode;
          if (this.mode === 'flame') {
            const src = this.chooseSource(q.src);
            const fs = this.flameSigFor(src, q);
            if (fs !== flameSig) {
              flameSig = fs;
              this.flameSrc = src;
              this.prepareFlame();
            } else {
              this.schedulePaint();
            }
          }
          this.version++;
        },

        select(id) {
          curId = id;
          trace = null;
          tree = null;
          analysis = null;
          rows = [];
          rowByKey = new Map();
          collapsed = new Set();
          expanded = new Set();
          keep = null;
          filterSig = null;
          lastFocus = null;
          lastMode = '';
          lastFold = '';
          flameSig = '';
          spanFlame = null;
          flameRoot = null;
          flameFocus = null;
          this.flameState = 'idle';
          this.scrollTop = 0;
          if (this.$refs.wf) this.$refs.wf.scrollTop = 0;
          clearTimeout(refetchTimer);
          this.version++;
          if (!id) return;
          this.loadTrace(id, false);
          this.loadAnalysis(id, false);
        },

        receive(ev) {
          if (ev.type !== 'trace' || !ev.data || ev.data.id !== curId) return;
          clearTimeout(refetchTimer);
          refetchTimer = setTimeout(() => {
            if (!curId) return;
            this.loadTrace(curId, true);
            this.loadAnalysis(curId, true);
          }, 300);
        },

        async loadTrace(id, again) {
          if (ctlTrace) ctlTrace.abort();
          const mine = typeof AbortController === 'function' ? new AbortController() : null;
          ctlTrace = mine;
          if (!again) {
            this.traceState = 'loading';
            this.traceError = '';
          }
          try {
            const t = await I.client.get('/api/traces/' + encodeURIComponent(id), { quiet: true, signal: mine ? mine.signal : undefined });
            if (curId !== id || dead) return;
            trace = t;
            tree = wf.buildTree(t);
            treeGen++;
            spanFlame = null;
            filterSig = null;
            lastFocus = null;
            flameSig = '';
            this.traceState = tree ? 'ready' : 'error';
            this.traceError = tree ? '' : 'This trace has no spans.';
            this.version++;
            this.refresh();
          } catch (e) {
            if ((e && e.name === 'AbortError') || curId !== id || again) return;
            this.traceState = 'error';
            this.traceError = e.message;
          } finally {
            if (ctlTrace === mine) ctlTrace = null;
          }
        },

        async loadAnalysis(id, again) {
          if (ctlAnalysis) ctlAnalysis.abort();
          const mine = typeof AbortController === 'function' ? new AbortController() : null;
          ctlAnalysis = mine;
          if (!again) {
            this.analysisState = 'loading';
            this.analysisError = '';
          }
          try {
            const a = await I.client.get('/api/traces/' + encodeURIComponent(id) + '/analysis', { quiet: true, signal: mine ? mine.signal : undefined });
            if (curId !== id || dead) return;
            analysis = a;
            this.analysisState = 'ready';
            this.version++;
            if (this.mode === 'bottlenecks') this.$nextTick(() => this.scrollFocused());
          } catch (e) {
            if ((e && e.name === 'AbortError') || curId !== id || again) return;
            this.analysisState = 'error';
            this.analysisError = e.message;
          } finally {
            if (ctlAnalysis === mine) ctlAnalysis = null;
          }
        },

        rebuild() {
          rows = wf.rows(tree, { showHidden, collapsed, expanded, keep });
          rowByKey = new Map(rows.map((r) => [r.key, r]));
          this.version++;
        },

        // ---- states ----

        get loading() {
          return this.traceState === 'loading';
        },
        get failed() {
          return this.traceState === 'error';
        },
        get ready() {
          return this.traceState === 'ready';
        },
        get hotspotsHref() {
          return route.format({ tab: 'traces' });
        },
        isMode(id) {
          return this.mode === id;
        },
        get showToolbar() {
          return this.mode === 'waterfall' || this.mode === 'spans';
        },

        // ---- header ----

        get method() {
          void this.version;
          return trace ? trace.method || '' : '';
        },
        get path() {
          void this.version;
          return trace ? urlPath(trace.url) : '';
        },
        get fullUrl() {
          void this.version;
          return trace ? trace.url || '' : '';
        },
        get facts() {
          void this.version;
          if (!trace) return [];
          const out = [];
          if (trace.status) out.push({ key: 'status', label: 'Status', value: String(trace.status), cls: trace.status >= 500 ? 'tone-error' : trace.status >= 400 ? 'tone-warn' : '' });
          if (trace.route) out.push({ key: 'route', label: 'Route', value: trace.route, cls: 'mono' });
          if (trace.varnishXid) out.push({ key: 'xid', label: 'Varnish xid', value: trace.varnishXid, cls: 'mono' });
          out.push({ key: 'spans', label: 'Spans', value: fmt.num((trace.spans || []).length), cls: '' });
          out.push({ key: 'start', label: 'Started', value: fmt.clock(trace.start), cls: 'mono' });
          return out;
        },
        get duration() {
          void this.version;
          return trace ? nsText(trace.duration) : '';
        },
        get hasProfile() {
          void this.version;
          return !!(trace && trace.hasProfile);
        },
        get cached() {
          void this.version;
          return !!(trace && trace.cached);
        },
        get overviewHref() {
          return route.format({ tab: 'overview', query: { trace: curId } });
        },
        get costTimes() {
          void this.version;
          return analysis ? timesOfCategories(analysis.categories) : {};
        },
        get costTotal() {
          void this.version;
          return analysis ? analysis.duration : 0;
        },
        get strip() {
          return strip(this.costTimes, this.costTotal);
        },
        get stripLabel() {
          return stripLabel(this.costTimes, this.costTotal);
        },
        get legend() {
          void this.version;
          return analysis ? legend(this.costTimes, this.costTotal) : [];
        },
        get tabs() {
          return tabList(store().route, this.mode);
        },

        // ---- focus ----

        focusNode() {
          void this.version;
          return this.focus && tree ? tree.byKey.get(this.focus) || null : null;
        },
        get focusInfo() {
          const n = this.focusNode();
          if (!n) return { name: '', resource: '', kind: '', kindLabel: '', self: '', total: '' };
          const k = wf.kindOf(n.span);
          return { name: shortName(n.span.name), resource: wf.describe(n.span), kind: k, kindLabel: kindLabel(k), self: nsText(n.self), total: nsText(n.dur) };
        },
        get hasFocus() {
          return !!this.focusNode();
        },
        get missingFocus() {
          void this.version;
          return !!(this.focus && tree && !tree.byKey.has(this.focus));
        },
        get focusWaterfallHref() {
          return showHref(curId, this.focus);
        },
        get showFocusLink() {
          return this.mode !== 'waterfall';
        },
        clearFocus() {
          setQuery({ focus: '' }, true);
        },

        scrollFocused() {
          const el = this.$el.querySelector('[data-focus="true"]');
          if (el && el.offsetParent) el.scrollIntoView({ block: 'nearest' });
        },

        // ---- bottlenecks ----

        get analysisLoading() {
          return this.analysisState === 'loading';
        },
        get analysisFailed() {
          return this.analysisState === 'error';
        },
        get analysisReady() {
          return this.analysisState === 'ready';
        },
        get hot() {
          void this.version;
          return hotRows(analysis, this.focus);
        },
        get repeats() {
          void this.version;
          return repeatList(analysis, tree, this.focus);
        },
        get slowest() {
          void this.version;
          return slowestGroups(analysis, this.focus);
        },
        get hotAll() {
          return store().route.query.more === '1';
        },
        get hotShown() {
          const all = this.hot;
          return this.hotAll ? all : all.slice(0, HOT_SHOWN);
        },
        get hotMore() {
          return this.hot.length > HOT_SHOWN;
        },
        get hotMoreText() {
          return this.hotAll ? 'Show the top ' + HOT_SHOWN : 'Show all ' + fmt.num(this.hot.length) + ' hot spans';
        },
        toggleHotList() {
          setQuery({ more: this.hotAll ? '' : '1' }, true);
        },
        get jumps() {
          return [
            { id: 'bn-hot', label: 'Hot spans', count: fmt.num(this.hot.length) },
            { id: 'bn-repeats', label: 'Repeated calls', count: fmt.num(this.repeats.length) },
            { id: 'bn-slow', label: 'Slowest calls', count: fmt.num(this.slowest.reduce((n, g) => n + g.items.length, 0)) },
          ];
        },
        jump(id) {
          const el = document.getElementById(id);
          if (el) el.scrollIntoView({ block: 'start' });
        },
        get showJumps() {
          return this.mode === 'bottlenecks' && this.analysisReady;
        },
        get hasHot() {
          return this.hot.length > 0;
        },
        get hasRepeats() {
          return this.repeats.length > 0;
        },
        get hasSlowest() {
          return this.slowest.length > 0;
        },

        // ---- filter toolbar (waterfall and spans) ----

        onFilter(ev) {
          this.qDraft = ev.target.value;
          clearTimeout(typeTimer);
          typeTimer = setTimeout(() => setQuery({ q: this.qDraft.trim() }, true), TYPE_MS);
        },
        get selfSteps() {
          const cur = store().route.query.min || '';
          return SELF_STEPS.map((s) => ({ id: s.id, label: s.label, selected: s.id === cur }));
        },
        setMinSelf(ev) {
          setQuery({ min: ev.target.value }, true);
        },
        get hotPressed() {
          return store().route.query.hot === '1' ? 'true' : 'false';
        },
        toggleHot() {
          setQuery({ hot: store().route.query.hot === '1' ? '' : '1' }, true);
        },
        get matchText() {
          void this.version;
          if (!keep) return '';
          const n = keep.matches || 0;
          return n === 0 ? 'No span matches' : fmt.num(n) + (n === 1 ? ' span matches' : ' spans match');
        },
        get hasFilter() {
          void this.version;
          return !!keep;
        },
        clearFilter() {
          this.qDraft = '';
          setQuery({ q: '', min: '', hot: '' }, true);
        },
        expandAll() {
          this.setFold('expanded');
        },
        collapseAll() {
          this.setFold('collapsed');
        },
        // Applied at once as well, so a second click after toggling carets
        // folds again although the hash already says so.
        setFold(mode) {
          this.applyFold(mode);
          lastFold = mode;
          setQuery({ fold: mode }, true);
        },
        applyFold(mode) {
          if (!tree) return;
          collapsed = new Set();
          if (mode === 'collapsed') {
            expanded = new Set();
            for (const r of wf.rows(tree, { showHidden, keep })) if (r.depth === 1 && r.toggle && r.kind === 'span') collapsed.add(r.key);
            this.rebuild();
            return;
          }
          for (const r of rows) if (r.kind === 'fold') expanded.add(r.key);
          this.rebuild();
          // Opening the folds can reveal further folds one level down.
          for (const r of rows) if (r.kind === 'fold') expanded.add(r.key);
          this.rebuild();
        },

        // ---- waterfall ----

        revealFocus(modeChanged) {
          const key = this.focus;
          if (!key || !tree.byKey.has(key)) return;
          const opts = { showHidden, collapsed, expanded, keep };
          const res = wf.reveal(tree, key, opts);
          collapsed = opts.collapsed;
          expanded = opts.expanded;
          rows = res.rows;
          rowByKey = new Map(rows.map((r) => [r.key, r]));
          this.version++;
          if (opts.showHidden && !showHidden) {
            showHidden = true;
            setQuery({ all: '1' }, true);
          }
          if (res.index < 0) return;
          this.$nextTick(() =>
            requestAnimationFrame(() => {
              if (!dead) this.scrollToRow(res.index, modeChanged);
            }),
          );
        },

        scrollToRow(i, intoView) {
          const vp = this.$refs.wf;
          if (!vp) return;
          const fitted = intoView ? this.fitViewport() : false;
          const h = (fitted ? this.wfMax : vp.clientHeight) || this.viewportH;
          this.viewportH = h;
          const top = i * ROW_H;
          if (top < vp.scrollTop || top + ROW_H > vp.scrollTop + h) vp.scrollTop = Math.max(0, top - Math.floor(h / 3));
          this.scrollTop = vp.scrollTop;
          if (intoView && !fitted) vp.scrollIntoView({ block: 'nearest' });
        },

        // Landing on a focused span keeps the header, the tabs and the toolbar
        // on screen: the page goes back to the top and the waterfall gets the
        // height that is left, so only the waterfall scrolls. A window too
        // short for that falls back to scrolling the page.
        fitViewport() {
          const vp = this.$refs.wf;
          const main = this.$el.closest('main');
          if (!vp || !main || window.innerWidth < 1100) return false;
          main.scrollTop = 0;
          const room = Math.floor(window.innerHeight - vp.getBoundingClientRect().top - 44);
          if (room < WF_MIN_H) return false;
          this.wfMax = room;
          return true;
        },
        get wfStyle() {
          return this.wfMax ? { maxHeight: this.wfMax + 'px' } : {};
        },

        get axis() {
          void this.version;
          if (!tree) return [];
          return [0, 0.25, 0.5, 0.75, 1].map((f) => ({ key: String(f), text: nsText(tree.total * f), cls: f === 0 || f === 1 ? '' : 'hidden sm:inline' }));
        },
        winStart() {
          return Math.max(0, Math.floor(this.scrollTop / ROW_H) - OVERSCAN);
        },
        spacerStyle() {
          void this.version;
          return { height: rows.length * ROW_H + 'px' };
        },
        windowStyle() {
          return { transform: 'translateY(' + this.winStart() * ROW_H + 'px)' };
        },
        get visibleRows() {
          void this.version;
          const start = this.winStart();
          const end = Math.min(rows.length, Math.ceil((this.scrollTop + this.viewportH) / ROW_H) + OVERSCAN);
          const sel = this.focus;
          const out = [];
          for (let i = start; i < end; i++) {
            const r = rows[i];
            out.push({
              key: r.key,
              index: i,
              caret: r.toggle ? (r.open ? '▾' : '▸') : '',
              fixed: !r.toggle,
              toggleLabel: r.open ? 'Collapse ' + r.name : 'Expand ' + r.name,
              prefix: r.prefix,
              name: r.name,
              detail: r.detail,
              nameCls: r.error ? 'tone-error' : r.kind === 'fold' ? 'text-fog' : '',
              barCls: 'wf-bar ' + (BAR[r.component] || (r.kind === 'fold' ? 'bg-neutral-bar' : 'bg-php-bar')),
              bar: { left: (r.left * 100).toFixed(3) + '%', width: (r.width * 100).toFixed(3) + '%' },
              ms: nsText(r.duration),
              selected: r.key === sel ? 'true' : 'false',
              title: r.name + (r.detail ? ' ' + r.detail : '') + ' · ' + nsText(r.duration) + (r.kind === 'span' ? ' · self ' + nsText(r.self) : ''),
            });
          }
          return out;
        },
        get rowCount() {
          void this.version;
          return rows.length;
        },
        get noRows() {
          return this.rowCount <= 1 && this.hasFilter;
        },
        onScroll() {
          if (frame) return;
          frame = requestAnimationFrame(() => {
            frame = 0;
            const vp = this.$refs.wf;
            if (!vp) return;
            this.scrollTop = vp.scrollTop;
            if (vp.clientHeight) this.viewportH = vp.clientHeight;
          });
        },
        // The list is virtual, so moving focus first scrolls the next row
        // into the rendered window.
        wfKey(ev) {
          if (ev.key !== 'ArrowDown' && ev.key !== 'ArrowUp') return;
          const at = Number(ev.target && ev.target.dataset ? ev.target.dataset.row : NaN);
          if (!Number.isInteger(at)) return;
          const next = at + (ev.key === 'ArrowDown' ? 1 : -1);
          if (next < 0 || next >= rows.length) return;
          ev.preventDefault();
          this.scrollToRow(next, false);
          this.$nextTick(() => {
            const b = this.$refs.wf && this.$refs.wf.querySelector('[data-row="' + next + '"]');
            if (b) b.focus();
          });
        },
        toggleRow(row) {
          const r = rowByKey.get(row.key);
          if (!r || !r.toggle) return;
          const set = r.kind === 'fold' ? expanded : collapsed;
          if (set.has(r.key)) set.delete(r.key);
          else set.add(r.key);
          this.rebuild();
        },
        pick(row) {
          const r = rowByKey.get(row.key);
          if (!r) return;
          if (r.kind === 'fold') {
            this.toggleRow(row);
            return;
          }
          lastFocus = r.key === this.focus ? '' : r.key;
          setQuery({ focus: lastFocus }, true);
        },
        get hiddenCount() {
          void this.version;
          return tree ? tree.hidden : 0;
        },
        get hiddenLink() {
          const n = this.hiddenCount;
          if (!n) return '';
          return store().route.query.all === '1' ? 'Hide ' + fmt.num(n) + ' plugin and observer spans' : 'Show ' + fmt.num(n) + ' plugin and observer spans';
        },
        toggleHidden() {
          setQuery({ all: store().route.query.all === '1' ? '' : '1' }, true);
        },

        // ---- side panel ----

        get panel() {
          const n = this.focusNode();
          if (!n || !trace) return NO_PANEL;
          const s = n.span;
          const k = wf.kindOf(s);
          const share = tree.total > 0 ? n.self / tree.total : 0;
          const facts = [
            { k: 'Starts at', v: '+' + nsText(n.start) },
            { k: 'Share of the request', v: pctText(share) },
            { k: 'Depth', v: String(n.depth || 0) },
            { k: 'Children', v: fmt.num(n.children.length) },
          ];
          if (s.service) facts.push({ k: 'Service', v: s.service });
          if (s.error) facts.push({ k: 'Error', v: 'yes' });
          return {
            title: s.name || '(unnamed)',
            kind: k,
            kindLabel: kindLabel(k),
            code: wf.resourceText(s),
            isSql: k === 'sql',
            selfText: nsText(n.self) + ' self of ' + nsText(n.dur) + ' total',
            selfStyle: barWidth(n.dur > 0 ? n.self / n.dur : 0),
            facts,
            groups: wf.childGroups(n).map((g) => ({ key: g.kind, kind: g.kind, label: kindLabel(g.kind), count: '×' + fmt.num(g.count), total: nsText(g.total) })),
            attrs: wf.attrList(s).map((a) => ({ key: a.k, k: a.k, v: a.v })),
          };
        },
        get hasPanel() {
          return this.mode === 'waterfall' && this.hasFocus;
        },
        get panelOpen() {
          return this.hasPanel ? 'true' : 'false';
        },
        get hasGroups() {
          return this.panel.groups.length > 0;
        },
        copySql() {
          const p = this.panel;
          if (p.code) copy(p.code, 'the SQL');
        },
        copyResource() {
          const p = this.panel;
          if (p.code) copy(p.code, 'the resource');
        },

        // ---- by type ----

        get categories() {
          void this.version;
          if (!analysis) return [];
          return (analysis.categories || []).map((c) => ({
            key: c.key,
            label: wf.COST_LABEL[c.key] || c.key,
            swatch: 'cost-swatch cat-' + c.key,
            bar: 'cat-' + c.key,
            ms: msText(c.ms),
            share: pctText(c.share),
            style: barWidth(c.share),
            count: c.key === 'php' ? '' : fmt.num(c.count) + (c.key === 'sql' ? ' queries' : ' calls'),
          }));
        },
        get kinds() {
          void this.version;
          if (!tree) return [];
          return wf.kindTotals(tree).map((k) => ({
            key: k.kind,
            kind: k.kind,
            label: kindLabel(k.kind),
            self: nsText(k.self),
            share: pctText(k.self / tree.total),
            style: barWidth(k.self / tree.total),
            count: fmt.num(k.count),
          }));
        },

        // ---- spans ----

        get spanSort() {
          return store().route.query.sort || 'self';
        },
        get spanDir() {
          return store().route.query.dir || spanDefaultDir(this.spanSort);
        },
        get spanHeaders() {
          const r = store().route;
          return sortHeaders(SPAN_COLS, this.spanSort, this.spanDir, spanDefaultDir, (id, d) => withQuery(r, { sort: id === 'self' ? '' : id, dir: d === spanDefaultDir(id) ? '' : d }));
        },
        // spanTable sorts names, kinds and depths ascending and times
        // descending; its dir asks for the reverse of that. Memoised because
        // the rows and the note both read it and a sort of 7,000 spans is not
        // free.
        spanResult() {
          const sort = this.spanSort;
          const flip = this.spanDir !== spanDefaultDir(sort);
          const sig = [treeGen, filterSig, sort, flip].join('|');
          if (!spanMemo || spanMemo.sig !== sig) spanMemo = { sig, res: wf.spanTable(tree, { keep, sort, dir: flip ? 'asc' : '', limit: SPAN_ROWS }) };
          return spanMemo.res;
        },
        get spanRows() {
          void this.version;
          if (this.mode !== 'spans' || !tree) return [];
          const focus = this.focus;
          const total = tree.total;
          return this.spanResult().rows.map((r) => ({
            key: r.key,
            name: shortName(r.name),
            resource: r.resource,
            self: nsText(r.self),
            total: nsText(r.total),
            heat: heatOf(r.self / total),
            kind: r.kind,
            kindLabel: kindLabel(r.kind),
            depth: String(r.depth),
            focused: r.key === focus ? 'true' : 'false',
            href: showHref(curId, r.key),
          }));
        },
        get spanNote() {
          void this.version;
          if (this.mode !== 'spans' || !tree) return '';
          const res = this.spanResult();
          if (res.total <= SPAN_ROWS) return fmt.num(res.total) + (res.total === 1 ? ' span' : ' spans');
          return 'The top ' + fmt.num(SPAN_ROWS) + ' of ' + fmt.num(res.total) + ' spans. Narrow them with the filter.';
        },
        pickSpan(r, ev) {
          if (fromLink(ev)) return;
          setQuery({ focus: r.key === this.focus ? '' : r.key }, true);
        },

        // ---- flame graph ----

        chooseSource(src) {
          if (src === 'spans' || !trace || !trace.hasProfile) return 'spans';
          if (src === 'profile') return 'profile';
          return this.focus ? 'spans' : 'profile';
        },
        get flameSources() {
          void this.version;
          const has = !!(trace && trace.hasProfile);
          return [
            { id: 'profile', label: 'Excimer profile', disabled: !has, pressed: this.flameSrc === 'profile' ? 'true' : 'false' },
            { id: 'spans', label: 'Span tree', disabled: false, pressed: this.flameSrc === 'spans' ? 'true' : 'false' },
          ];
        },
        setFlameSource(b) {
          if (b.disabled || b.pressed === 'true') return;
          setQuery({ src: b.id }, true);
        },
        get flameLoading() {
          return this.flameState === 'loading';
        },
        get flameReady() {
          return this.flameState === 'ready';
        },
        get flameHint() {
          return this.flameHover || 'Click a frame to zoom in, click the top row to zoom out. Hover a frame for its share.';
        },

        async prepareFlame() {
          if (!trace || !tree) return;
          const id = trace.id;
          let root = null;
          let note = '';
          if (this.flameSrc === 'profile' && trace.hasProfile) {
            let p = profiles.get(id);
            if (!p) {
              this.flameState = 'loading';
              try {
                const doc = await I.client.get('/api/traces/' + encodeURIComponent(id) + '/profile', { quiet: true });
                const parsed = flame.fromSpeedscope(typeof doc === 'string' ? JSON.parse(doc) : doc);
                p = { root: parsed.root, unit: parsed.unit, samples: parsed.samples };
              } catch (e) {
                p = { error: e.message };
              }
              profiles.set(id, p);
              if (profiles.size > PROFILE_CACHE) profiles.delete(profiles.keys().next().value);
              if (curId !== id || dead) return;
            }
            if (p.error) {
              note = 'The Excimer profile could not be read (' + p.error + '). Showing the span tree instead.';
            } else {
              root = p.root;
              flameUnit = p.unit;
              note = 'Excimer wall-clock profile, ' + fmt.num(p.samples) + ' samples. Frames are merged by call path and sorted heaviest first.';
            }
          } else if (!trace.hasProfile) {
            note = 'No Excimer profile for this request, so this is the span tree. Set ROLL_INSPECTOR_PROFILE=excimer in .env.roll and run roll env up to sample PHP itself.';
          } else {
            note = 'Span tree: each span is drawn under its parent at its share of the request.';
          }
          let focusFrame = null;
          if (!root) {
            if (!spanFlame) spanFlame = wf.toFlame(tree);
            root = spanFlame;
            flameUnit = 'ns';
            const q = store().route.query || {};
            focusFrame = wf.findFlame(root, q.zoom) || wf.findFlame(root, this.focus);
          }
          flameRoot = root;
          flameFocus = focusFrame || root;
          this.flameZoomed = flameFocus !== flameRoot;
          this.flameHover = '';
          this.flameNote = note;
          this.flameState = 'ready';
          this.$nextTick(() => this.schedulePaint());
        },

        schedulePaint() {
          requestAnimationFrame(() => this.paint());
        },

        paint() {
          const svg = this.$refs.flame;
          const box = this.$refs.flameBox;
          if (dead || !svg || !box || !flameRoot || this.mode !== 'flame') return;
          const width = Math.floor(box.clientWidth);
          if (!width) return;
          const lay = flame.layout(flameRoot, flameFocus, width, { rowH: FLAME_ROW, minW: 1 });
          // Every paint replaces the frames, so one that held focus (a
          // keyboard zoom, then the repaint its hash change causes) takes it
          // back on the new frame.
          const had = svg.contains(document.activeElement);
          flame.render(document, svg, lay, {
            width,
            classFor: (n, r) => (r.ancestor ? FLAME_ANCESTOR : FLAME_RECT) + (FILL[n.component || flame.component(n)] || flameHeat(n, flameRoot)),
            labelClass: FLAME_LABEL,
            ariaLabel: (n) => this.frameText(n),
            current: flameFocus,
            refocus: refocusFlame || had,
            onClick: (n, r, viaKey) => this.zoom(n, viaKey),
            onHover: (n) => this.hover(n),
          });
          refocusFlame = false;
        },

        flameSigFor(src, q) {
          return [curId, src, this.focus, trace && trace.hasProfile ? 1 : 0, q.zoom || ''].join('|');
        },

        flameValue(v) {
          return flameUnit === 'ns' ? nsText(v) : fmt.num(v) + ' samples';
        },

        hover(n) {
          this.flameHover = n ? this.frameText(n) : '';
        },

        frameText(n) {
          const parts = [n.name === 'all' && !n.parent ? 'whole request' : n.name];
          if (n.detail) parts.push(n.detail);
          parts.push(this.flameValue(n.value));
          if (flameRoot && flameRoot.value) parts.push(pctText(n.value / flameRoot.value) + ' of the request');
          if (flameFocus && flameFocus !== flameRoot && flameFocus.value) parts.push(pctText(n.value / flameFocus.value) + ' of the zoomed frame');
          if (n.self) parts.push('self ' + this.flameValue(n.self));
          if (n.file && n.file !== n.name) parts.push(n.file);
          return parts.join(' · ');
        },

        zoom(n, viaKey) {
          if (n === flameFocus && n.parent) flameFocus = n.parent;
          else flameFocus = n;
          this.flameZoomed = flameFocus !== flameRoot;
          refocusFlame = !!viaKey;
          this.keepZoom();
          this.paint();
        },

        resetZoom() {
          flameFocus = flameRoot;
          this.flameZoomed = false;
          this.keepZoom();
          this.paint();
        },

        // Only span-tree frames have a key to put in the hash; a zoom into
        // an Excimer profile frame stays local.
        keepZoom() {
          if (flameRoot !== spanFlame || !spanFlame) return;
          const zoom = flameFocus && flameFocus !== flameRoot && flameFocus.key ? flameFocus.key : '';
          const q = Object.assign({}, store().route.query, { zoom });
          // The hash change must not prepare the flame again: that redraw
          // would drop the frame the keyboard just focused.
          flameSig = this.flameSigFor(this.flameSrc, q);
          setQuery({ zoom }, true);
        },
      };
    });
  }

  return {
    register,
    msText,
    nsText,
    pctText,
    heatOf,
    heatOfRank,
    shortPath,
    costKey,
    flameHeat,
    shortName,
    kindLabel,
    strip,
    stripLabel,
    legend,
    withQuery,
    sortHeaders,
    sortRoutes,
    routeRows,
    operationRows,
    repeatRows,
    requestFilter,
    filterRequests,
    requestRow,
    mergeSummary,
    routeOptions,
    hotRows,
    repeatList,
    slowestGroups,
    tabList,
    showHref,
  };
});
