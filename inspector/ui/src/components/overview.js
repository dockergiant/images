(function (root, factory) {
  const isNode = typeof module === 'object' && module.exports;
  const deps = isNode ? { fmt: require('../lib/format.js'), route: require('../lib/route.js'), waterfall: require('../lib/waterfall.js') } : root.Inspector;
  const mod = factory(deps);
  if (isNode) {
    module.exports = mod;
  } else {
    root.Inspector.view = mod.view;
    root.Inspector.overview = mod;
    document.addEventListener('alpine:init', () => mod.register(window.Alpine, root.Inspector));
  }
})(this, function (deps) {
  const fmt = deps.fmt;
  const route = deps.route;
  const wf = deps.waterfall;

  const POLL_MS = 10000;
  const MAX_ROWS = 300;
  const TRACE_LIMIT = 500;
  const NO_STRIP = Object.freeze({ segments: [], label: '' });
  const MIN_BAR = 0.6;
  const LANE_ORDER = ['varnish', 'php', 'redis', 'search', 'rabbitmq'];
  const JOIN_CLS = { exact: 'text-ok', probable: 'text-probable', none: 'text-muted' };
  const JOIN_TEXT = {
    exact: 'Joined by an id that both sides carry, so this is certain.',
    probable: 'Matched by timing and process, so treat it as a likely fit.',
    none: 'Nothing could be tied to this request.',
  };
  const CARD_PICK = {
    varnish: { label: 'Hit rate' },
    redis: { label: 'Hit ratio' },
    search: { label: 'Cluster' },
    rabbitmq: { label: 'Messages ready' },
  };

  function toneText(tone) {
    switch (tone) {
      case 'ok':
        return 'tone-ok';
      case 'warn':
        return 'tone-warn';
      case 'error':
        return 'tone-error';
      default:
        return '';
    }
  }

  function healthModel(h) {
    if (!h) return { loaded: false, status: '', summary: '', cards: [], warnings: [] };
    return {
      loaded: true,
      status: h.status || '',
      summary: h.summary || '',
      cards: (h.cards || []).map((c) => ({ label: c.label, value: c.value, sub: c.sub || '', cls: toneText(c.tone) })),
      warnings: warningRows(h.warnings || []),
    };
  }

  function warningRows(list, source) {
    return list.map((w, i) => ({
      key: (source || 'env') + ':' + w.id + ':' + i,
      id: w.id,
      text: w.text,
      fix: w.fix || '',
      hasFix: !!w.fix,
      source: source || '',
      cls: w.tone === 'error' ? 'tone-error' : w.tone === 'info' ? 'tone-info' : 'tone-warn',
      title: w.tone === 'error' ? 'Needs attention' : w.tone === 'info' ? 'Note' : 'Warning',
    }));
  }

  function compose() {
    const out = {};
    for (const part of arguments) {
      Object.defineProperties(out, Object.getOwnPropertyDescriptors(part));
    }
    return out;
  }

  function cardByLabel(cards, label) {
    return (cards || []).find((c) => c.label.toLowerCase().startsWith(label.toLowerCase())) || null;
  }

  function noLookups(card) {
    return card.value === '–' || card.value === '';
  }

  function idleText(card) {
    return /in the last/.test(card.sub || '') ? 'idle' : 'no requests yet';
  }

  function headline(kind, health) {
    if (!health) return { value: 'unreachable', sub: '', tone: 'error' };
    const cards = health.cards || [];
    const first = cards[0] || { value: health.summary || '', sub: '', tone: health.status };
    const pick = CARD_PICK[kind] && cardByLabel(cards, CARD_PICK[kind].label);
    const c = pick || first;
    let value = c.value;
    let sub = c.sub || '';
    switch (kind) {
      case 'varnish': {
        const backend = cards.find((x) => x.label.toLowerCase().startsWith('backend'));
        const pages = cardByLabel(cards, 'Cached pages');
        value = noLookups(c) ? idleText(c) : c.value + ' hit rate';
        sub = [backend ? 'backend ' + String(backend.value).toLowerCase() : '', pages ? pages.value + ' pages' : ''].filter(Boolean).join(' · ');
        break;
      }
      case 'redis': {
        const mem = cardByLabel(cards, 'Memory');
        const ev = cardByLabel(cards, 'Evictions');
        value = noLookups(c) ? idleText(c) : c.value + ' hits';
        sub = [mem ? mem.value : '', ev ? ev.value + ' evictions' : ''].filter(Boolean).join(' · ');
        if (value === 'idle') sub = [c.sub, mem ? mem.value : ''].filter(Boolean).join(' · ');
        break;
      }
      case 'search':
        value = '● ' + c.value;
        break;
      case 'rabbitmq': {
        const n = Number(c.value);
        value = n > 0 ? fmt.num(n) + (n === 1 ? ' message waiting' : ' messages waiting') : 'queues idle';
        const q = cardByLabel(cards, 'Queues');
        sub = q ? q.sub : c.sub;
        break;
      }
      default:
        break;
    }
    let tone = health.status;
    if (kind === 'search') tone = c.value === 'green' ? 'ok' : c.value === 'red' ? 'error' : 'warn';
    return { value, sub, tone };
  }

  function serviceCards(overview, labelOf) {
    if (!overview || !overview.adapters) return [];
    return overview.adapters.map((a) => {
      const info = a.info || {};
      const present = !!info.present;
      const name = labelOf(a.kind, info);
      const version = fmt.shortVersion(info.version);
      const h = present ? headline(a.kind, a.health) : null;
      return {
        kind: a.kind,
        present,
        label: (name + ' ' + version).trim(),
        value: present ? h.value : 'not in this env',
        sub: present ? h.sub : 'Switch it on in .env.roll to see it here',
        valueCls: present ? toneText(h.tone) : '',
        href: route.format({ tab: a.kind, section: 'health' }),
        cls: present ? fmt.kindClass(a.kind) : fmt.kindClass(a.kind) + ' opacity-60',
      };
    });
  }

  function laneBars(spans) {
    const out = [];
    for (const s of spans || []) {
      const a = Math.max(0, Math.min(1, Number(s[0])));
      const b = Math.max(a, Math.min(1, Number(s[1])));
      const left = a * 100;
      const width = Math.max(MIN_BAR, (b - a) * 100);
      out.push({ left: Math.min(left, 100 - MIN_BAR), width: Math.min(width, 100 - Math.min(left, 100 - MIN_BAR)) });
    }
    return out;
  }

  function barStyle(bar) {
    return { left: bar.left.toFixed(2) + '%', width: bar.width.toFixed(2) + '%' };
  }

  const LANE_SOURCE = { trace: 'from trace spans', monitor: 'from MONITOR' };

  function laneCaption(lane) {
    const parts = [];
    if (lane.summary) parts.push(lane.summary);
    if (lane.join !== 'exact' && lane.reason) parts.push(lane.reason);
    if (LANE_SOURCE[lane.source]) parts.push(LANE_SOURCE[lane.source]);
    return parts.join(' · ');
  }

  function laneRows(lanes, labelOf) {
    const known = (lanes || []).slice().sort((a, b) => {
      const ia = LANE_ORDER.indexOf(a.kind);
      const ib = LANE_ORDER.indexOf(b.kind);
      return (ia < 0 ? 99 : ia) - (ib < 0 ? 99 : ib);
    });
    return known.map((l) => ({
      key: l.kind,
      kind: l.kind,
      label: labelOf(l.kind),
      labelCls: l.kind === 'php' ? 'text-php' : l.kind === 'varnish' ? 'text-varnish' : l.kind === 'redis' ? 'text-redis' : l.kind === 'search' ? 'text-search' : l.kind === 'rabbitmq' ? 'text-rabbitmq' : '',
      trackCls: fmt.kindClass(l.kind),
      join: l.join,
      joinCls: JOIN_CLS[l.join] || 'text-muted',
      joinTitle: l.reason || JOIN_TEXT[l.join] || '',
      caption: laneCaption(l),
      bars: laneBars(l.join === 'none' ? [] : l.spans).map((b, i) => ({ id: i, left: b.left, width: b.width })),
    }));
  }

  function requestBadge(r) {
    const h = String(r.handling || '').toUpperCase();
    if (h) return h;
    return String(r.source || '').toLowerCase();
  }

  // The cost strip of a request comes from its trace summary; a HIT or a
  // request without tracing has none.
  function requestStrip(r, summaries) {
    const s = r.traceId && summaries ? summaries.get(r.traceId) : null;
    if (!s || !s.times) return NO_STRIP;
    return wf.strip(s.times, s.duration / 1e6);
  }

  function requestRow(r, selectedKey, summaries) {
    const h = String(r.handling || '').toUpperCase();
    const st = requestStrip(r, summaries);
    return {
      strip: st.segments,
      stripLabel: st.label,
      hasStrip: st.segments.length > 0,
      key: r.key,
      title: (r.method ? r.method + ' ' : '') + r.url,
      badge: requestBadge(r),
      badgeCls: h === 'MISS' ? 'text-warn' : h === 'HIT' ? 'text-ok' : r.status >= 500 ? 'text-error' : '',
      meta: [r.status || '', r.duration ? fmt.ms(r.duration) : '', fmt.clock(r.start).slice(0, 8)].filter((x) => x !== '').join(' · '),
      selected: r.key === selectedKey ? 'true' : 'false',
      current: r.key === selectedKey ? 'true' : 'false',
    };
  }

  function pickRequest(list, query) {
    const q = query || {};
    if (q.req) {
      const r = list.find((x) => x.key === q.req || (x.replaces || []).includes(q.req));
      if (r) return r;
    }
    if (q.trace) {
      const r = list.find((x) => x.traceId === q.trace);
      if (r) return r;
    }
    return list[0] || null;
  }

  function summaryMap(list) {
    const m = new Map();
    for (const s of list || []) if (s && s.id) m.set(s.id, s);
    return m;
  }

  function mergeRequest(list, req) {
    const gone = new Set(req.replaces || []);
    const next = list.filter((r) => r.key !== req.key && !gone.has(r.key));
    next.push(req);
    next.sort((a, b) => (a.start < b.start ? 1 : a.start > b.start ? -1 : 0));
    if (next.length > MAX_ROWS) next.length = MAX_ROWS;
    return next;
  }

  function assetRequest(r) {
    const path = String(r.url || '').replace(/^\/pub(?=\/)/, '');
    return path.startsWith('/static/') || path.startsWith('/media/');
  }

  function visibleRequests(list, hideHits, hideAssets) {
    return list.filter((r) => !(hideHits && String(r.handling || '').toUpperCase() === 'HIT') && !(hideAssets && assetRequest(r)));
  }

  function magentoFacts(m) {
    if (!m || m.generic) return [];
    const off = Object.keys(m.cacheTypes || {}).filter((k) => !m.cacheTypes[k]).sort();
    const rows = [
      ['Framework', [m.framework, m.frameworkVersion].filter(Boolean).join(' ')],
      ['Mode', m.mode],
      ['Search engine', m.searchEngine],
      ['Page cache', m.fpcApplication],
      ['Sessions', m.sessionSave],
      ['Disabled cache types', off.join(', ')],
      ['Store URL', m.baseUrls && (m.baseUrls.web_secure || m.baseUrls.web_unsecure || Object.values(m.baseUrls)[0])],
    ];
    return rows.filter((r) => r[1]).map((r) => ({ k: r[0], v: String(r[1]) }));
  }

  async function copyText(text) {
    const I = typeof window !== 'undefined' ? window.Inspector : null;
    try {
      await navigator.clipboard.writeText(text);
      if (I) I.toast('Copied: ' + text, 'ok');
      return true;
    } catch (e) {
      if (I) I.toast('Copy failed. Select the command and copy it by hand.', 'warn');
      return false;
    }
  }

  // The dashboard under the cards hands up a card for its picked window; it
  // replaces the server card with the same label prefix, so the card and
  // the chart below it always show the same number.
  function withWindowCard(cards, card) {
    if (!card) return cards;
    return cards.map((c) => (c.label.toLowerCase().startsWith(card.match) ? { label: card.label, value: card.value, sub: card.sub, cls: card.cls } : c));
  }

  function serviceBase(kind, I, Alpine) {
    const store = () => Alpine.store('app');
    let timer = null;
    return {
      health: null,
      healthError: '',
      windowCard: null,

      get section() {
        return store().route.section;
      },
      get isHealth() {
        return this.section === 'health';
      },
      get isBrowse() {
        return this.section === 'browse';
      },
      get isLive() {
        return this.section === 'live';
      },
      get isActions() {
        return this.section === 'actions';
      },
      get raw() {
        return store().raw;
      },
      get lan() {
        const s = store().session;
        return !!(s && s.lan);
      },
      get lanReason() {
        const s = store().session;
        return s && s.lan ? s.lanReason || 'Writes are refused while Traefik listens beyond loopback.' : '';
      },
      get hv() {
        const m = healthModel(this.health);
        return Object.assign({}, m, { cards: withWindowCard(m.cards, this.windowCard) });
      },
      get rawHealth() {
        return this.health ? fmt.pretty(this.health.raw === undefined ? this.health : this.health.raw) : '';
      },
      get hasHealthError() {
        return !!this.healthError;
      },
      get serviceName() {
        return I.label(kind);
      },

      async refreshHealth() {
        try {
          this.health = await I.client.get('/api/' + kind + '/health', { quiet: true });
          this.healthError = '';
        } catch (e) {
          this.healthError = e.message;
        }
      },

      startPolling() {
        this.refreshHealth();
        timer = setInterval(() => {
          if (this.isHealth && !document.hidden) this.refreshHealth();
        }, POLL_MS);
      },

      stopPolling() {
        if (timer) clearInterval(timer);
        timer = null;
      },

      copy(text) {
        return copyText(text);
      },

      openDrawer(sub) {
        Alpine.store('drawer').show(sub);
      },
    };
  }

  const view = { healthModel, warningRows, compose, serviceBase, withWindowCard, toneText, copyText, POLL_MS };

  function register(Alpine, I) {
    const store = () => Alpine.store('app');

    // The selected request (?req= or ?trace=) and the two hide filters
    // (?hits=1, ?static=1 show them) live in the hash like all view state.
    function setQuery(patch) {
      const r = store().route;
      const query = Object.assign({}, r.query, patch);
      for (const k of Object.keys(query)) if (!query[k]) delete query[k];
      const h = route.format({ tab: r.tab, section: r.section, rest: r.rest, query });
      window.history.replaceState(null, '', h);
      store().route = route.parse(h);
    }

    Alpine.data('overview', () => {
      let off = null;
      let offTraces = null;
      let offWatch = null;
      let poll = null;
      let summaries = new Map();
      return {
        requests: [],
        summaryVersion: 0,
        streamState: 'connecting',
        listError: '',
        loaded: false,

        init() {
          I.loadOverview().catch(() => {});
          this.loadRequests();
          this.loadSummaries();
          poll = setInterval(() => {
            if (document.hidden) return;
            I.loadOverview().catch(() => {});
            this.loadRequests();
          }, POLL_MS);
          offWatch = I.stream.watch((st) => {
            this.streamState = st.state;
          });
          off = I.stream.subscribe(['timeline'], (ev) => this.receive(ev));
          offTraces = I.stream.subscribe(['php:traces'], (ev) => this.receiveTrace(ev));
        },

        destroy() {
          if (off) off();
          if (offTraces) offTraces();
          if (offWatch) offWatch();
          if (poll) clearInterval(poll);
        },

        async loadSummaries() {
          try {
            const list = await I.client.get('/api/traces', { query: { limit: TRACE_LIMIT, sort: 'recent' }, quiet: true });
            summaries = summaryMap(list);
            this.summaryVersion++;
          } catch (e) {
            // The list still works without strips; the next trace event fills them in.
          }
        },

        receiveTrace(ev) {
          if (ev.type === 'gap') {
            this.loadSummaries();
            return;
          }
          if (ev.type !== 'trace' || !ev.data || !ev.data.id) return;
          summaries.set(ev.data.id, ev.data);
          if (summaries.size > TRACE_LIMIT * 2) summaries.delete(summaries.keys().next().value);
          this.summaryVersion++;
        },

        get hideHits() {
          return store().route.query.hits !== '1';
        },
        get hideAssets() {
          return store().route.query.static !== '1';
        },

        async loadRequests() {
          try {
            const list = await I.client.get('/api/timeline', { query: { n: 100 }, quiet: true });
            this.requests = list.slice(0, MAX_ROWS);
            this.listError = '';
          } catch (e) {
            this.listError = e.message;
          }
          this.loaded = true;
        },

        receive(ev) {
          if (ev.type === 'gap') {
            this.loadRequests();
            return;
          }
          if (ev.type !== 'timeline' || !ev.data || !ev.data.key) return;
          this.requests = mergeRequest(this.requests, ev.data);
          if ((ev.data.replaces || []).includes(store().route.query.req)) setQuery({ req: ev.data.key });
        },

        get overview() {
          return store().overview;
        },
        get warnings() {
          const o = this.overview;
          if (!o) return [];
          let rows = warningRows(o.warnings || [], 'env');
          for (const a of o.adapters || []) {
            if (a.health && a.health.warnings && a.health.warnings.length) {
              rows = rows.concat(warningRows(a.health.warnings, fmt.serviceLabel(a.kind, a.info)));
            }
          }
          return rows;
        },
        get hasWarnings() {
          return this.warnings.length > 0;
        },
        get facts() {
          return magentoFacts(this.overview && this.overview.magento);
        },
        get hasFacts() {
          return this.facts.length > 0;
        },
        get magentoError() {
          return (this.overview && this.overview.magentoError) || '';
        },
        get hasMagentoError() {
          return !!this.magentoError;
        },

        get visible() {
          return visibleRequests(this.requests, this.hideHits, this.hideAssets);
        },
        get rows() {
          void this.summaryVersion;
          const cur = this.current ? this.current.key : '';
          return this.visible.map((r) => requestRow(r, cur, summaries));
        },
        get hasRows() {
          return this.rows.length > 0;
        },
        get hiddenText() {
          const hits = this.hideHits ? this.requests.filter((r) => String(r.handling || '').toUpperCase() === 'HIT').length : 0;
          const assets = this.hideAssets ? this.requests.filter((r) => assetRequest(r) && String(r.handling || '').toUpperCase() !== 'HIT').length : 0;
          const parts = [];
          if (hits) parts.push(fmt.num(hits) + (hits === 1 ? ' HIT' : ' HITs'));
          if (assets) parts.push(fmt.num(assets) + (assets === 1 ? ' static file' : ' static files'));
          return parts.length ? parts.join(' and ') + ' hidden' : '';
        },
        get emptyText() {
          if (this.listError) return this.listError;
          if (!this.loaded) return 'Loading requests…';
          if (this.requests.length) return 'Every recent request is hidden by the filters above. Untick them to see the HITs and static files.';
          return 'No requests yet. Load a page in the store and it shows up here.';
        },
        get live() {
          return this.streamState === 'open';
        },
        get liveText() {
          return this.live ? '● live' : this.streamState === 'closed' ? 'stream closed' : 'connecting…';
        },
        get liveCls() {
          return this.live ? 'text-ok' : 'text-warn';
        },
        toggleHits() {
          setQuery({ hits: this.hideHits ? '1' : '' });
        },
        toggleAssets() {
          setQuery({ static: this.hideAssets ? '1' : '' });
        },

        get current() {
          return pickRequest(this.visible, store().route.query);
        },
        get hasCurrent() {
          return !!this.current;
        },
        select(row) {
          setQuery({ req: row.key, trace: '' });
        },
        get currentStrip() {
          void this.summaryVersion;
          return this.current ? requestStrip(this.current, summaries) : NO_STRIP;
        },
        get hasCurrentStrip() {
          return this.currentStrip.segments.length > 0;
        },
        get title() {
          const r = this.current;
          return r ? (r.method ? r.method + ' ' : '') + r.url : '';
        },
        get subtitle() {
          const r = this.current;
          if (!r) return '';
          return [r.vxid || (r.key.startsWith('v:') ? 'xid ' + r.key.slice(2) : ''), r.status, r.duration ? fmt.ms(r.duration) : '', r.host].filter((x) => x !== '' && x !== undefined).join(' · ');
        },
        get traceHref() {
          const r = this.current;
          return r && r.traceId ? route.format({ tab: 'traces', section: r.traceId }) : '';
        },
        get hasTrace() {
          return !!this.traceHref;
        },
        get axisEnd() {
          const r = this.current;
          return r && r.duration ? fmt.ms(r.duration) : '';
        },
        get lanes() {
          const r = this.current;
          return r ? laneRows(r.lanes, (k) => (k === 'php' ? 'PHP' : I.label(k))) : [];
        },
        get answers() {
          const r = this.current;
          if (!r) return [];
          return (r.answers || []).map((c, i) => ({ key: i, label: c.label, value: c.value, sub: c.sub || '', cls: toneText(c.tone) }));
        },
        get hasAnswers() {
          return this.answers.length > 0;
        },
        barStyle(bar) {
          return barStyle(bar);
        },
        copy(text) {
          return copyText(text);
        },
      };
    });
  }

  return {
    register,
    view,
    toneText,
    healthModel,
    warningRows,
    compose,
    headline,
    serviceCards,
    laneBars,
    barStyle,
    laneCaption,
    laneRows,
    requestRow,
    requestStrip,
    pickRequest,
    summaryMap,
    mergeRequest,
    visibleRequests,
    assetRequest,
    magentoFacts,
  };
});
