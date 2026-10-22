(function (root, factory) {
  const isNode = typeof module === 'object' && module.exports;
  const deps = isNode ? { fmt: require('../lib/format.js'), route: require('../lib/route.js'), rail: require('../lib/rail.js') } : root.Inspector;
  const mod = factory(deps);
  if (isNode) {
    module.exports = mod;
  } else {
    root.Inspector.palette = mod;
    document.addEventListener('alpine:init', () => mod.register(window.Alpine, root.Inspector));
  }
})(this, function (deps) {
  const fmt = deps.fmt;
  const route = deps.route;
  const rail = deps.rail;

  // Group order breaks ties between groups that match equally well; with an
  // empty query it is the display order.
  const GROUPS = [
    { id: 'pages', label: 'Pages', icon: '#i-page', limit: 8 },
    { id: 'routes', label: 'Routes', icon: '#i-route', limit: 6 },
    { id: 'traces', label: 'Traces', icon: '#i-traces', limit: 6 },
    { id: 'requests', label: 'Recent requests', icon: '#i-request', limit: 6 },
    { id: 'indices', label: 'OpenSearch indices', icon: '#i-index', limit: 5 },
    { id: 'actions', label: 'Actions', icon: '#i-bolt', limit: 8 },
    { id: 'keys', label: 'Valkey keys', icon: '#i-key', limit: 50 },
  ];
  const EMPTY_GROUPS = ['pages', 'requests', 'actions'];
  const MAX_ROWS = 60;
  const SECTION_LABELS = { health: 'Health', browse: 'Browse', live: 'Live', actions: 'Actions' };
  const PAGE_ICONS = { overview: '#i-overview', traces: '#i-traces', varnish: '#i-varnish', redis: '#i-redis', search: '#i-search', rabbitmq: '#i-rabbitmq', connect: '#i-api', performance: '#i-gauge' };

  function isBoundary(text, i) {
    if (i === 0) return true;
    const prev = text.charAt(i - 1);
    if (/[\s/_.:\-?=&#›·]/.test(prev)) return true;
    const c = text.charAt(i);
    return prev === prev.toLowerCase() && c !== c.toLowerCase();
  }

  // One query token against one text. A substring beats a scattered
  // subsequence; starts of words and the start of the text count extra.
  function matchToken(token, text) {
    const t = text.toLowerCase();
    const at = t.indexOf(token);
    if (at >= 0) {
      let score = 100 + token.length * 4;
      if (at === 0) score += 40;
      else if (isBoundary(text, at)) score += 24;
      score -= Math.min(at, 40) * 0.5;
      const hits = [];
      for (let i = 0; i < token.length; i++) hits.push(at + i);
      return { score, hits };
    }
    const hits = [];
    let score = 0;
    let from = 0;
    let prev = -2;
    for (const ch of token) {
      let i = t.indexOf(ch, from);
      if (i < 0) return null;
      // Prefer the next word start for this character when one exists, so
      // "ccv" lands on catalog/category/view rather than inside a word.
      for (let j = i; j >= 0 && j < t.length; j = t.indexOf(ch, j + 1)) {
        if (isBoundary(text, j)) {
          if (j - i < 24) i = j;
          break;
        }
      }
      const joined = i === prev + 1;
      const atStart = isBoundary(text, i);
      // Short tokens scattered through a word ("cat" in customer/account) are
      // noise; they only count when they follow word starts.
      if (token.length <= 4 && !joined && !atStart) return null;
      score += 3;
      if (joined) score += 6;
      if (atStart) score += 5;
      score -= Math.min(i - from, 20) * 0.2;
      hits.push(i);
      prev = i;
      from = i + 1;
    }
    return { score, hits };
  }

  function tokens(query) {
    return String(query || '')
      .toLowerCase()
      .split(/\s+/)
      .filter((s) => s !== '');
  }

  // Every token has to match the label or the keywords. Highlights come from
  // the label only.
  function match(query, label, keywords) {
    const toks = tokens(query);
    if (!toks.length) return { score: 0, hits: [] };
    const text = String(label || '');
    const extra = String(keywords || '');
    let score = 0;
    const hits = new Set();
    for (const tok of toks) {
      const m = matchToken(tok, text);
      const k = extra ? matchToken(tok, extra) : null;
      if (!m && !k) return null;
      if (m && (!k || m.score >= k.score * 0.8)) {
        score += m.score;
        for (const h of m.hits) hits.add(h);
      } else {
        score += k.score * 0.8;
      }
    }
    // Shorter labels win between equal matches: "Varnish" before "Varnish › Live".
    score -= Math.min(text.length, 120) * 0.05;
    return { score, hits: Array.from(hits).sort((a, b) => a - b) };
  }

  function segments(label, hits) {
    const text = String(label || '');
    if (!hits || !hits.length) return [{ key: 0, text, hit: false, cls: '' }];
    const set = new Set(hits);
    const out = [];
    let cur = null;
    for (let i = 0; i < text.length; i++) {
      const hit = set.has(i);
      if (!cur || cur.hit !== hit) {
        cur = { key: out.length, text: '', hit, cls: hit ? 'palette-hit' : '' };
        out.push(cur);
      }
      cur.text += text.charAt(i);
    }
    return out;
  }

  // Ranks items for a query and returns them grouped, best group first. An
  // item with `always` stays in the list whatever the query (the Valkey key
  // search offer); with an empty query only the starter groups show.
  function rank(items, query, opts) {
    const q = String(query || '').trim();
    const all = !!(opts && opts.all);
    const scored = [];
    items.forEach((item, idx) => {
      if (item.always) {
        if (q) scored.push({ item, score: -1, hits: [], idx });
        return;
      }
      if (!q) {
        if (all || (EMPTY_GROUPS.includes(item.group) && !item.hideEmpty)) scored.push({ item, score: 0, hits: [], idx });
        return;
      }
      const m = match(q, item.label, item.keywords);
      if (m) scored.push({ item, score: m.score + (item.boost || 0), hits: m.hits, idx });
    });
    const byGroup = new Map();
    for (const s of scored) {
      if (!byGroup.has(s.item.group)) byGroup.set(s.item.group, []);
      byGroup.get(s.item.group).push(s);
    }
    const order = GROUPS.map((g) => g.id);
    const groups = [];
    for (const g of GROUPS) {
      const list = byGroup.get(g.id);
      if (!list) continue;
      list.sort((a, b) => b.score - a.score || a.idx - b.idx);
      const top = list.slice(0, q ? g.limit : g.id === 'pages' ? 12 : g.limit);
      groups.push({ id: g.id, label: g.label, best: top[0].score, options: top });
    }
    if (q) groups.sort((a, b) => b.best - a.best || order.indexOf(a.id) - order.indexOf(b.id));
    let n = 0;
    const out = [];
    for (const g of groups) {
      const options = [];
      for (const s of g.options) {
        if (n >= MAX_ROWS) break;
        options.push(Object.assign({}, s.item, { index: n, score: s.score, segments: segments(s.item.label, s.hits) }));
        n++;
      }
      if (options.length) out.push({ id: g.id, label: g.label, labelId: 'pal-group-' + g.id, options });
    }
    return out;
  }

  function pathOf(url) {
    const s = String(url || '');
    const m = s.match(/^[a-z]+:\/\/[^/]+(\/.*)?$/i);
    return m ? m[1] || '/' : s;
  }

  function pageItems(session) {
    const out = [];
    const present = (kind) => rail.isPresent(session, kind);
    const add = (id, label, href, keywords, kind) => out.push({ id: 'page:' + id, group: 'pages', label, href, keywords: keywords || '', icon: PAGE_ICONS[id.split(':')[0]] || '#i-page', kind: kind || '' });
    add('overview', 'Overview', route.format({ tab: 'overview' }), 'home timeline requests health', '');
    add('traces', 'PHP traces › Hotspots', route.format({ tab: 'traces' }), 'apm slow bottleneck n+1', 'php');
    add('traces:requests', 'PHP traces › Requests', route.format({ tab: 'traces', section: 'requests' }), 'apm list', 'php');
    for (const tab of rail.ORDER) {
      if (!route.isService(tab)) continue;
      const kind = route.kindOf(tab);
      if (session && !present(kind)) continue;
      const label = rail.labelOf(session, kind);
      for (const sec of route.SECTIONS) {
        const name = sec === 'health' ? label : label + ' › ' + SECTION_LABELS[sec];
        add(tab + ':' + sec, name, route.format({ tab, section: sec }), tab + ' ' + sec, kind);
      }
    }
    add('performance', 'Performance', route.format({ tab: 'performance' }), 'tracing profiling overhead lean slow cost disable switch off excimer sampling', '');
    add('connect', 'API & extensions', route.format({ tab: 'connect' }), 'api v1 userscript tampermonkey extension devtools cors curl fetch openapi', '');
    return out;
  }

  function routeItems(traces) {
    const counts = new Map();
    for (const t of traces || []) {
      if (!t.route) continue;
      counts.set(t.route, (counts.get(t.route) || 0) + 1);
    }
    return Array.from(counts.entries())
      .sort((a, b) => b[1] - a[1])
      .map(([r, n]) => ({
        id: 'route:' + r,
        group: 'routes',
        label: r,
        mono: true,
        hint: n + (n === 1 ? ' trace' : ' traces'),
        href: route.format({ tab: 'traces', section: 'requests', query: { route: r } }),
        icon: '#i-route',
        kind: 'php',
      }));
  }

  function traceItems(traces) {
    return (traces || []).map((t) => ({
      id: 'trace:' + t.id,
      group: 'traces',
      label: (t.method ? t.method + ' ' : '') + pathOf(t.url),
      mono: true,
      hint: [fmt.ms(t.duration), t.route, t.id.slice(0, 8)].filter(Boolean).join(' · '),
      hintMono: true,
      keywords: t.id + ' ' + (t.route || '') + ' ' + (t.url || ''),
      href: route.format({ tab: 'traces', section: t.id }),
      icon: '#i-traces',
      kind: 'php',
    }));
  }

  function requestItems(timeline) {
    const seen = new Set();
    const out = [];
    for (const r of timeline || []) {
      const url = String(r.url || '');
      if (!url || seen.has(url)) continue;
      seen.add(url);
      const href = r.traceId ? route.format({ tab: 'traces', section: r.traceId }) : route.format({ tab: 'overview', query: { req: r.key } });
      out.push({
        id: 'req:' + r.key,
        group: 'requests',
        label: (r.method ? r.method + ' ' : '') + pathOf(url),
        mono: true,
        hint: [r.handling, r.status || '', r.traceId ? 'traced' : ''].filter(Boolean).join(' · '),
        keywords: url + ' ' + (r.vxid || '') + ' ' + (r.traceId || ''),
        href,
        icon: '#i-request',
        kind: r.traceId ? 'php' : 'varnish',
        hideEmpty: out.length >= 5,
      });
    }
    return out;
  }

  function indexItems(indices) {
    return (indices || [])
      .filter((ix) => !ix.system)
      .map((ix) => ({
        id: 'index:' + ix.index,
        group: 'indices',
        label: ix.index,
        mono: true,
        hint: ix.orphan ? 'orphan' : ix.label ? ix.label.title : (ix.aliases || []).join(', '),
        keywords: (ix.aliases || []).join(' ') + ' ' + (ix.label ? ix.label.title : ''),
        href: route.format({ tab: 'search', section: 'browse', query: { index: ix.index } }),
        icon: '#i-index',
        kind: 'search',
      }));
  }

  // Actions that need a value first ask for it in the palette itself, then
  // open the confirm dialog the service view uses for the same write.
  const ARG_ACTIONS = {
    'purge-tag': { kind: 'varnish', label: 'Purge Varnish pages by tag…', prompt: 'Tag to purge, for example cat_c_41', mode: 'Purge tag' },
    'ban-url': { kind: 'varnish', label: 'Ban Varnish URLs by regex…', prompt: 'URL regex, for example ^/checkout', mode: 'Ban URLs' },
    'clean-tag': { kind: 'redis', label: 'Clean a Valkey cache tag…', prompt: 'Cache tag, for example CAT_P_12', mode: 'Clean tag' },
  };

  function actionItems(input) {
    const session = input.session;
    const out = [];
    const present = (kind) => !!session && rail.isPresent(session, kind);
    const writeHint = session && session.lan ? 'writes refused on the LAN' : '';
    for (const name of Object.keys(ARG_ACTIONS)) {
      const a = ARG_ACTIONS[name];
      if (!present(a.kind)) continue;
      out.push({ id: 'action:' + name, group: 'actions', label: a.label, action: name, arg: true, prompt: a.prompt, mode: a.mode, hint: writeHint, keywords: a.kind + ' write', icon: '#i-bolt', kind: a.kind });
    }
    if (present('varnish')) {
      out.push({ id: 'action:purge-all', group: 'actions', label: 'Purge all Magento pages', action: 'purge-all', hint: writeHint || 'typed confirm', keywords: 'varnish flush', icon: '#i-bolt', kind: 'varnish' });
    }
    for (const st of rail.live({ session, drawer: input.drawer })) {
      const on = st.on === 'true';
      out.push({
        id: 'live:' + st.sub,
        group: 'actions',
        label: (on ? 'Stop ' : 'Start ') + st.label,
        action: 'live',
        sub: st.sub,
        hint: st.optIn ? 'changes the service' : 'live drawer',
        keywords: 'live stream drawer ' + st.sub,
        icon: '#i-live',
        kind: st.kind,
        hideEmpty: true,
      });
    }
    out.push({ id: 'action:drawer', group: 'actions', label: input.drawer && input.drawer.open ? 'Close the live drawer' : 'Open the live drawer', action: 'drawer', hint: 'L', keywords: 'live stream', icon: '#i-live', kind: '' });
    if (input.rawLabel) {
      out.push({ id: 'action:raw', group: 'actions', label: (input.raw ? 'Hide ' : 'Show ') + input.rawLabel.toLowerCase(), action: 'raw', keywords: 'raw json', icon: '#i-bolt', kind: '' });
    }
    return out;
  }

  function buildItems(input) {
    const items = [].concat(
      pageItems(input.session),
      routeItems(input.traces),
      traceItems(input.traces),
      requestItems(input.timeline),
      indexItems(input.indices),
      actionItems(input),
    );
    if (input.session && rail.isPresent(input.session, 'redis')) {
      items.push({ id: 'keys:search', group: 'keys', label: 'Find keys that start with this text', action: 'keys', always: true, hint: 'db' + (input.keyDb || 0) + ' · Enter', icon: '#i-key', kind: 'redis' });
    }
    return items;
  }

  function keyItems(keys, db) {
    return (keys || []).map((k) => ({
      id: 'key:' + k,
      group: 'keys',
      label: k,
      mono: true,
      hint: 'db' + db,
      href: route.format({ tab: 'redis', section: 'browse', query: { db: String(db), key: k } }),
      icon: '#i-key',
      kind: 'redis',
    }));
  }

  function globEscape(s) {
    return String(s).replace(/[*?[\]\\]/g, '\\$&');
  }

  function cacheDb(magento) {
    const c = magento && magento.cache;
    const f = c && (c.default || c.page_cache);
    return f && Number.isInteger(f.database) ? f.database : 0;
  }

  function register(Alpine, I) {
    const store = () => Alpine.store('app');
    const CACHE_MS = 10000;

    Alpine.store('palette', {
      open: false,
      show() {
        this.open = true;
      },
      hide() {
        this.open = false;
      },
      toggle() {
        this.open = !this.open;
      },
    });

    Alpine.data('palette', () => {
      let data = { traces: [], timeline: [], indices: [], at: 0 };
      let memo = { key: '', groups: [] };
      // Option ids change with every rebuilt list: a screen reader announces
      // only a changed aria-activedescendant, and the first result is always
      // index 0.
      let gen = 0;
      let returnFocus = null;
      let fx = null;
      let sayFx = null;
      let sayTimer = null;
      let keySeq = 0;
      let dataVersion = 0;

      return {
        query: '',
        active: 0,
        mode: '',
        argItem: null,
        keys: [],
        keysFor: '',
        note: '',
        loading: false,
        version: 0,
        announce: '',

        init() {
          let wasOpen = false;
          fx = Alpine.effect(() => {
            const open = Alpine.store('palette').open;
            if (open === wasOpen) return;
            wasOpen = open;
            // Run outside the effect: opened() reads the session, and the
            // session poll would otherwise reopen the palette and clear the
            // query mid-typing.
            queueMicrotask(() => (open ? this.opened() : this.closed()));
          });
          sayFx = Alpine.effect(() => {
            if (!Alpine.store('palette').open) return;
            const text = announceText(this.noteText, this.options);
            clearTimeout(sayTimer);
            sayTimer = setTimeout(() => {
              this.announce = text;
            }, ANNOUNCE_MS);
          });
        },

        destroy() {
          if (fx) Alpine.release(fx);
          if (sayFx) Alpine.release(sayFx);
          clearTimeout(sayTimer);
        },

        // A click on a heading or the footer would move focus to <body>,
        // where neither Esc nor the Tab guard on the input can reach it.
        keepFocus(ev) {
          if (ev.target !== this.$refs.input) ev.preventDefault();
        },

        onEscape() {
          if (!this.open) return;
          if (this.mode) this.leaveMode();
          else this.close();
        },

        get open() {
          return Alpine.store('palette').open;
        },

        opened() {
          returnFocus = document.activeElement;
          this.query = '';
          this.active = 0;
          this.mode = '';
          this.argItem = null;
          this.note = '';
          this.keys = [];
          this.warm();
          this.$nextTick(() => {
            if (this.$refs.input) this.$refs.input.focus();
          });
        },

        closed() {
          clearTimeout(sayTimer);
          this.announce = '';
          if (returnFocus && typeof returnFocus.focus === 'function' && document.body.contains(returnFocus)) returnFocus.focus();
          returnFocus = null;
        },

        close() {
          Alpine.store('palette').hide();
        },

        async warm() {
          if (Date.now() - data.at < CACHE_MS) return;
          const s = store().session;
          const get = (path, query) => I.client.get(path, { query, quiet: true }).catch(() => null);
          this.loading = true;
          const [traces, timeline, idx] = await Promise.all([
            get('/api/traces', { limit: 500 }),
            get('/api/timeline', { n: 200 }),
            s && rail.isPresent(s, 'search') ? get('/api/search/indices') : Promise.resolve(null),
          ]);
          data = {
            traces: Array.isArray(traces) ? traces : [],
            timeline: Array.isArray(timeline) ? timeline : [],
            indices: idx && Array.isArray(idx.indices) ? idx.indices : [],
            at: Date.now(),
          };
          dataVersion++;
          this.loading = false;
          this.version++;
        },

        get placeholder() {
          if (this.mode === 'arg' && this.argItem) return this.argItem.prompt;
          if (this.mode === 'keys') return 'Key prefix, Enter to search again';
          return 'Find a page, route, trace id, URL, index or action';
        },

        get modeLabel() {
          if (this.mode === 'arg' && this.argItem) return this.argItem.mode;
          if (this.mode === 'keys') return 'Valkey keys';
          return '';
        },

        get hasMode() {
          return this.mode !== '';
        },

        get modeCls() {
          if (this.mode === 'keys') return 'kind-redis';
          return this.argItem ? fmt.kindClass(this.argItem.kind) : 'kind-none';
        },

        get keyDb() {
          return cacheDb(store().magento);
        },

        get groups() {
          if (this.mode === 'arg') return [];
          const d = Alpine.store('drawer');
          const r = store().route;
          const key = [this.mode, this.query, dataVersion, this.version, d.open, d.selected.join(','), store().raw, r.tab, !!store().session].join('|');
          if (memo.key === key) return memo.groups;
          let groups;
          if (this.mode === 'keys') {
            // The scan already matched the prefix; typing further narrows the
            // result locally until Enter on an empty list scans again.
            groups = rank(keyItems(this.keys, this.keyDb), this.query.trim(), { all: true });
          } else {
            groups = rank(
              buildItems({
                session: store().session,
                traces: data.traces,
                timeline: data.timeline,
                indices: data.indices,
                drawer: { open: d.open, selected: d.selected },
                raw: store().raw,
                rawLabel: rail.rawLabel(r.tab),
                keyDb: this.keyDb,
              }),
              this.query,
            );
          }
          gen++;
          for (const g of groups) {
            for (const o of g.options) {
              o.domId = 'pal-opt-' + gen + '-' + o.index;
              o.cls = fmt.kindClass(o.kind);
              o.isMono = o.mono ? 'true' : 'false';
              o.hintIsMono = o.hintMono ? 'true' : 'false';
              o.hasHint = !!o.hint;
            }
          }
          memo = { key, groups };
          return groups;
        },

        get options() {
          const out = [];
          for (const g of this.groups) for (const o of g.options) out.push(o);
          return out;
        },

        get count() {
          return this.options.length;
        },

        get activeId() {
          const o = this.options[this.active];
          return o ? o.domId : null;
        },

        isActive(o) {
          return o.index === this.active ? 'true' : 'false';
        },

        get showNote() {
          return !!this.noteText;
        },

        get noteText() {
          if (this.note) return this.note;
          if (this.mode === 'arg') return 'Enter opens the confirm dialog. Backspace on an empty field goes back.';
          if (this.loading && !this.count) return 'Reading traces and requests…';
          if (this.mode === 'keys' && !this.count) return 'No scanned key matches. Enter scans db' + this.keyDb + ' for keys that start with "' + this.query.trim() + '".';
          if (this.query.trim() && !this.count) return 'Nothing matches "' + this.query.trim() + '".';
          return '';
        },

        onInput() {
          this.active = 0;
          this.note = '';
        },

        onKey(ev) {
          const n = this.count;
          switch (ev.key) {
            case 'ArrowDown':
              ev.preventDefault();
              if (n) this.setActive((this.active + 1) % n);
              break;
            case 'ArrowUp':
              ev.preventDefault();
              if (n) this.setActive((this.active - 1 + n) % n);
              break;
            case 'Home':
              if (ev.ctrlKey || ev.metaKey) {
                ev.preventDefault();
                this.setActive(0);
              }
              break;
            case 'End':
              if (ev.ctrlKey || ev.metaKey) {
                ev.preventDefault();
                if (n) this.setActive(n - 1);
              }
              break;
            case 'Enter':
              ev.preventDefault();
              this.enter();
              break;
            case 'Escape':
              ev.preventDefault();
              ev.stopPropagation();
              if (this.mode) this.leaveMode();
              else this.close();
              break;
            case 'Backspace':
              if (this.mode && this.query === '') {
                ev.preventDefault();
                this.leaveMode();
              }
              break;
            case 'Tab':
              ev.preventDefault();
              break;
            default:
          }
        },

        setActive(i) {
          this.active = i;
          this.$nextTick(() => {
            const o = this.options[i];
            const el = o && document.getElementById(o.domId);
            if (el && typeof el.scrollIntoView === 'function') el.scrollIntoView({ block: 'nearest' });
          });
        },

        hover(o) {
          if (o.index !== this.active) this.active = o.index;
        },

        leaveMode() {
          this.mode = '';
          this.argItem = null;
          this.query = '';
          this.keys = [];
          this.note = '';
          this.active = 0;
        },

        enter() {
          if (this.mode === 'arg') {
            this.runArg(this.argItem, this.query.trim());
            return;
          }
          if (this.mode === 'keys' && !this.count) {
            this.searchKeys(this.query.trim());
            return;
          }
          const o = this.options[this.active];
          if (o) this.run(o);
        },

        pick(o) {
          this.run(o);
        },

        run(o) {
          if (o.href) {
            this.close();
            window.location.hash = o.href;
            return;
          }
          if (o.arg) {
            this.mode = 'arg';
            this.argItem = o;
            this.query = '';
            this.note = '';
            this.$nextTick(() => this.$refs.input && this.$refs.input.focus());
            return;
          }
          switch (o.action) {
            case 'keys':
              this.searchKeys(this.query.trim());
              return;
            case 'live': {
              const d = Alpine.store('drawer');
              this.close();
              if (d.has(o.sub)) d.hide(o.sub);
              else d.show(o.sub);
              return;
            }
            case 'drawer':
              this.close();
              Alpine.store('drawer').toggleOpen();
              return;
            case 'raw':
              this.close();
              I.toggleRaw();
              return;
            case 'purge-all':
              this.close();
              I.confirm({
                kind: 'varnish',
                action: 'purge-all',
                target: 'all-pages',
                title: 'Purge all Magento pages?',
                effect: 'Every cached Magento page is dropped at once. Expect a burst of full renders while the cache warms up again.',
                call: 'PURGE / X-Magento-Tags-Pattern: .*',
                button: 'Purge all pages',
                typedLabel: 'Type all-pages to confirm',
              });
              return;
            default:
          }
        },

        runArg(item, value) {
          if (!item) return;
          if (!value) {
            this.note = 'Type a value first.';
            return;
          }
          const opts = argConfirm(item.action, value, this.keyDb);
          if (opts.error) {
            this.note = opts.error;
            return;
          }
          this.close();
          I.confirm(opts);
        },

        async searchKeys(q) {
          if (!q) {
            this.note = 'Type the start of a key, then press Enter.';
            return;
          }
          const seq = ++keySeq;
          this.mode = 'keys';
          this.note = 'Scanning db' + this.keyDb + ' for keys that start with ' + q + '…';
          try {
            const r = await I.client.get('/api/redis/keys', { query: { db: this.keyDb, match: globEscape(q) + '*' }, quiet: true });
            if (seq !== keySeq) return;
            this.keys = (r && r.keys) || [];
            this.keysFor = q;
            this.active = 0;
            const more = r && r.cursor && r.cursor !== '0' ? ' The scan stopped early; open Valkey › Browse for the rest.' : '';
            this.note = this.keys.length ? (more ? more.trim() : '') : 'No key in db' + this.keyDb + ' starts with ' + q + '.' + more;
          } catch (e) {
            if (seq === keySeq) this.note = e.message;
          }
        },
      };
    });
  }

  const ANNOUNCE_MS = 300;

  function announceText(note, options) {
    if (note) return note;
    const n = (options || []).length;
    if (!n) return 'No results';
    return n + (n === 1 ? ' result, ' : ' results, ') + options[0].label;
  }

  function argConfirm(action, value, db) {
    switch (action) {
      case 'purge-tag': {
        if (!/^[^\s,]+$/.test(value)) return { error: 'A tag has no spaces or commas.' };
        const pattern = '((^|,)' + value.replace(/[.*+?^${}()|[\]\\]/g, '\\$&') + '(,|$))';
        return {
          kind: 'varnish',
          action: 'purge-tag',
          target: value,
          title: 'Purge pages tagged ' + value + '?',
          effect: 'Every cached page that carries this tag is dropped. The next visitor of each page waits for a full render.',
          call: 'PURGE / X-Magento-Tags-Pattern: ' + pattern,
          button: 'Purge pages',
        };
      }
      case 'ban-url':
        try {
          new RegExp(value);
        } catch (e) {
          return { error: 'Not a valid regex: ' + e.message };
        }
        return {
          kind: 'varnish',
          action: 'ban-url',
          target: value,
          title: 'Ban URLs matching ' + value + '?',
          effect: 'Cached objects whose URL matches are treated as expired. The ban stays in ban.list until every older object has been looked up or expired.',
          call: 'varnishadm ban req.url ~ "' + value + '"',
          button: 'Ban URLs',
        };
      case 'clean-tag':
        if (/\s/.test(value)) return { error: 'A tag has no spaces.' };
        return {
          kind: 'redis',
          action: 'clean-tag',
          target: value,
          params: { db },
          title: 'Clean tag ' + value + '?',
          effect: 'Every entry in db' + db + ' that carries this tag is deleted, the way Magento cleans a tag.',
          call: 'SMEMBERS <tag set>, then UNLINK each entry',
          button: 'Clean tag',
        };
      default:
        return { error: 'Unknown action.' };
    }
  }

  return { GROUPS, match, segments, rank, buildItems, keyItems, argConfirm, announceText, globEscape, cacheDb, pathOf, register };
});
