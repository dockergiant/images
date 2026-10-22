(function (root, factory) {
  const isNode = typeof module === 'object' && module.exports;
  const deps = isNode ? { fmt: require('../lib/format.js'), view: require('./overview.js').view } : root.Inspector;
  const mod = factory(deps);
  if (isNode) {
    module.exports = mod;
  } else {
    document.addEventListener('alpine:init', () => mod.register(window.Alpine, root.Inspector));
  }
})(this, function (deps) {
  const fmt = deps.fmt;
  const view = deps.view;

  const TABS = [
    { id: 'search', label: 'Find requests' },
    { id: 'bans', label: 'Bans' },
    { id: 'vcl', label: 'VCL' },
    { id: 'backends', label: 'Backends' },
    { id: 'params', label: 'Parameters' },
  ];

  function missRows(raw) {
    return ((raw && raw.topMisses) || []).map((m, i) => ({
      key: i,
      url: m.url,
      misses: fmt.num(m.misses),
      reason: m.reason,
      reasonCls: /cookie|no-cache|private|no-store/i.test(m.reason || '') ? 'text-warn' : 'text-soft',
    }));
  }

  function banRows(bans, now) {
    return (bans || []).map((b, i) => ({
      key: i,
      age: fmt.ago(b.time, now),
      spec: b.spec || (b.completed ? 'completed' : 'pending'),
      flush: !!b.fullFlush,
      refs: b.refs,
      done: b.completed ? 'completed' : 'pending',
    }));
  }

  function requestRows(reqs) {
    return (reqs || []).map((r, i) => ({
      key: r.vxid + ':' + i,
      time: fmt.clock(r.start),
      badge: String(r.handling || '').toUpperCase(),
      badgeCls: r.handling === 'HIT' ? 'text-ok' : r.handling === 'MISS' ? 'text-warn' : '',
      text: [r.method, r.url, r.status || '', fmt.ms(r.duration)].filter((x) => x !== '').join(' '),
      tags: r.tags && r.tags.length ? r.tags.length + ' tags' : '',
      reason: r.reason || '',
      raw: (r.raw || []).join('\n'),
    }));
  }

  function backendRows(list) {
    return (list || []).map((b) => ({
      key: b.name,
      name: b.name,
      health: b.probe && b.probe !== 'healthy' ? b.probe : b.adminHealth,
      healthCls: b.adminHealth === 'healthy' && (!b.probe || b.probe === 'healthy' || b.probe === 'unknown') ? 'text-ok' : 'text-warn',
      probe: b.window ? b.good + ' of ' + b.window + ' probes good' : 'no probe',
      changed: fmt.ago(b.lastChange),
    }));
  }

  function filterSub(url, q) {
    let sub = 'varnish:log';
    const pairs = [];
    if (q && q.trim()) pairs.push('q=' + q.trim());
    if (url && url.trim()) pairs.push('url=' + url.trim());
    return pairs.length ? sub + ';' + pairs.join(';') : sub;
  }

  function purgeCall(pattern) {
    return 'PURGE / X-Magento-Tags-Pattern: ' + pattern;
  }

  function tagPattern(tag) {
    return '((^|,)' + tag.replace(/[.*+?^${}()|[\]\\]/g, '\\$&') + '(,|$))';
  }

  function tagPreview(res) {
    if (!res) return '';
    const reqs = (res.requests || []).filter((r) => r.method !== 'PURGE' && r.method !== 'BAN');
    if (!reqs.length) return 'The Varnish log holds no request with this tag right now, so the purge may drop nothing.';
    const pages = [];
    for (const r of reqs) {
      const path = String(r.url || '').split('?')[0];
      if (!pages.includes(path)) pages.push(path);
    }
    return 'The Varnish log holds ' + fmt.num(reqs.length) + (reqs.length === 1 ? ' request' : ' requests') + ' for ' + fmt.num(pages.length) + (pages.length === 1 ? ' page' : ' pages') + ' with this tag, for example ' + pages[0] + '.';
  }

  function register(Alpine, I) {
    Alpine.data('varnish', () => {
      let stopBrowse = null;
      // Any hash change replaces the route object, so the effect re-runs on
      // a Raw toggle too; only entering the section should fetch.
      let browseKey = '';
      let vclSeq = 0;
      let searchSeq = 0;
      return view.compose(view.serviceBase('varnish', I, Alpine), {
        tabs: TABS,
        tab: 'search',
        loading: false,
        error: '',
        bans: null,
        vcls: null,
        backends: null,
        params: null,
        panic: null,
        vclName: '',
        vclFiles: null,
        searchUrl: '',
        searchTag: '',
        searchQ: '',
        result: null,
        picked: '',
        liveUrl: '',
        liveQ: '',
        purgeTag: '',
        previewing: false,
        banRegex: '',

        init() {
          this.startPolling();
          stopBrowse = Alpine.effect(() => {
            const r = Alpine.store('app').route;
            const key = r.tab === 'varnish' ? r.section : '';
            if (key === browseKey) return;
            browseKey = key;
            if (key === 'browse') setTimeout(() => this.loadTab(), 0);
          });
        },

        destroy() {
          this.stopPolling();
          if (stopBrowse) Alpine.release(stopBrowse);
          stopBrowse = null;
        },

        get misses() {
          return missRows(this.health && this.health.raw);
        },
        get hasMisses() {
          return this.misses.length > 0;
        },
        get missNote() {
          const raw = this.health && this.health.raw;
          if (!raw) return '';
          if (!raw.seen) return 'Nothing counted yet. Misses are tallied from the Varnish log while this tab or the Overview is open; load a few pages.';
          return 'Counted from the last ' + fmt.num(raw.seen) + ' requests seen while Inspector was watching.';
        },
        get recentBans() {
          return banRows(this.health && this.health.raw && this.health.raw.bans);
        },
        get hasBans() {
          return this.recentBans.length > 0;
        },
        get rawCounters() {
          const raw = this.health && this.health.raw;
          return raw ? fmt.pretty(raw.counters) : '';
        },

        selectTab(t) {
          this.tab = t.id;
          this.loadTab();
        },
        get tabList() {
          return TABS.map((t) => ({ id: t.id, label: t.label, pressed: t.id === this.tab ? 'true' : 'false' }));
        },
        get isTabSearch() {
          return this.tab === 'search';
        },
        get isTabBans() {
          return this.tab === 'bans';
        },
        get isTabVcl() {
          return this.tab === 'vcl';
        },
        get isTabBackends() {
          return this.tab === 'backends';
        },
        get isTabParams() {
          return this.tab === 'params';
        },

        async fetchInto(field, path) {
          this.loading = true;
          try {
            this[field] = await I.client.get(path, { quiet: true });
            this.error = '';
          } catch (e) {
            this.error = e.message;
          } finally {
            this.loading = false;
          }
        },

        async loadTab() {
          switch (this.tab) {
            case 'bans':
              return this.fetchInto('bans', '/api/varnish/bans');
            case 'vcl':
              await this.fetchInto('vcls', '/api/varnish/vcl');
              if (!this.vclName && this.vcls && this.vcls.length) {
                const active = this.vcls.find((v) => v.status === 'active') || this.vcls[0];
                return this.showVcl(active.name);
              }
              return null;
            case 'backends':
              return this.fetchInto('backends', '/api/varnish/backends');
            case 'params':
              await this.fetchInto('params', '/api/varnish/params');
              return this.fetchInto('panic', '/api/varnish/panic');
            default:
              return null;
          }
        },

        get hasError() {
          return !!this.error;
        },
        get banList() {
          return banRows(this.bans, Date.now());
        },
        get vclList() {
          return (this.vcls || []).map((v) => ({
            name: v.name,
            meta: [v.status, v.temperature, v.busy ? v.busy + ' busy' : ''].filter(Boolean).join(' · '),
            current: v.name === this.vclName ? 'true' : 'false',
          }));
        },
        async showVcl(name) {
          const seq = ++vclSeq;
          this.vclName = name;
          this.vclFiles = null;
          try {
            const r = await I.client.get('/api/varnish/vcl/' + encodeURIComponent(name), { quiet: true });
            if (seq !== vclSeq) return;
            this.vclFiles = r.files || [];
            this.error = '';
          } catch (e) {
            if (seq === vclSeq) this.error = e.message;
          }
        },
        get vclFileList() {
          return (this.vclFiles || []).map((f, i) => ({ key: i, path: f.path || this.vclName, source: f.source }));
        },
        get backendList() {
          return backendRows(this.backends);
        },
        get paramList() {
          return (this.params || []).map((p) => ({
            key: p.name,
            name: p.name,
            value: p.value + (p.units ? ' ' + p.units : ''),
            def: p.default ? 'default ' + p.default : '',
            description: p.description || '',
          }));
        },
        get panicText() {
          if (!this.panic) return '';
          return this.panic.panic ? 'The child panicked: ' + this.panic.text : this.panic.text;
        },
        get panicCls() {
          return this.panic && this.panic.panic ? 'tone-error' : 'tone-ok';
        },

        get canSearch() {
          return !!(this.searchUrl.trim() || this.searchTag.trim() || this.searchQ.trim());
        },
        async runSearch() {
          if (!this.canSearch) return;
          this.picked = '';
          const seq = ++searchSeq;
          this.loading = true;
          try {
            const r = await I.client.get('/api/varnish/search', { query: { url: this.searchUrl.trim(), tag: this.searchTag.trim(), q: this.searchQ.trim() }, quiet: true });
            if (seq !== searchSeq) return;
            this.result = r;
            this.error = '';
          } catch (e) {
            if (seq !== searchSeq) return;
            this.error = e.message;
            this.result = null;
          } finally {
            if (seq === searchSeq) this.loading = false;
          }
        },
        get hasResult() {
          return !!this.result;
        },
        get resultRows() {
          return requestRows(this.result && this.result.requests).map((r) => Object.assign({ open: r.key === this.picked ? 'true' : 'false' }, r));
        },
        get resultTitle() {
          if (!this.result) return '';
          const n = this.result.requests.length;
          return fmt.num(n) + (n === 1 ? ' request' : ' requests') + (this.result.truncated ? ' (the log search stopped at its limit)' : '');
        },
        get resultQuery() {
          return this.result ? this.result.query : '';
        },
        pick(row) {
          this.picked = this.picked === row.key ? '' : row.key;
        },
        get pickedRaw() {
          const r = this.resultRows.find((x) => x.key === this.picked);
          return r ? r.raw : '';
        },
        get hasPicked() {
          return !!this.picked;
        },

        get liveSub() {
          return filterSub(this.liveUrl, this.liveQ);
        },
        get liveOpen() {
          return Alpine.store('drawer').has(this.liveSub);
        },
        get liveButton() {
          return this.liveOpen ? 'Showing in the drawer' : 'Open the Varnish log';
        },
        get liveInvalid() {
          return this.liveUrl.includes(';') || this.liveQ.includes(';');
        },
        openLive() {
          if (this.liveInvalid) return;
          this.openDrawer(this.liveSub);
        },

        get canPurgeTag() {
          return !this.lan && !this.previewing && /^[^\s,]+$/.test(this.purgeTag.trim());
        },
        get canBan() {
          return !this.lan && this.banRegex.trim() !== '';
        },
        async doPurgeTag() {
          const tag = this.purgeTag.trim();
          if (!this.canPurgeTag) return;
          let preview = '';
          this.previewing = true;
          try {
            preview = tagPreview(await I.client.get('/api/varnish/search', { query: { tag }, quiet: true }));
          } catch (e) {
            preview = '';
          } finally {
            this.previewing = false;
          }
          const res = await I.confirm({
            kind: 'varnish',
            action: 'purge-tag',
            target: tag,
            title: 'Purge pages tagged ' + tag + '?',
            effect: 'Every cached page that carries this tag is dropped. The next visitor of each page waits for a full render.' + (preview ? ' ' + preview : ''),
            call: purgeCall(tagPattern(tag)),
            button: 'Purge pages',
          });
          if (res) this.purgeTag = '';
        },
        async doBanUrl() {
          const rx = this.banRegex.trim();
          if (!this.canBan) return;
          const res = await I.confirm({
            kind: 'varnish',
            action: 'ban-url',
            target: rx,
            title: 'Ban URLs matching ' + rx + '?',
            effect: 'Cached objects whose URL matches are treated as expired. The ban stays in ban.list until every older object has been looked up or expired.',
            call: 'varnishadm ban req.url ~ "' + rx + '"',
            button: 'Ban URLs',
          });
          if (res) this.banRegex = '';
        },
        doPurgeAll() {
          return I.confirm({
            kind: 'varnish',
            action: 'purge-all',
            target: 'all-pages',
            title: 'Purge all Magento pages?',
            effect: 'Every cached Magento page is dropped at once. Expect a burst of full renders while the cache warms up again.',
            call: purgeCall('.*'),
            button: 'Purge all pages',
            typedLabel: 'Type all-pages to confirm',
          });
        },
        doBanAll() {
          return I.confirm({
            kind: 'varnish',
            action: 'ban-all',
            target: 'everything',
            title: 'Ban everything?',
            effect: 'Every object in the cache is banned, including non-Magento content such as static files and API responses.',
            call: 'varnishadm ban req.url ~ .',
            button: 'Ban everything',
            typedLabel: 'Type everything to confirm',
          });
        },
      });
    });
  }

  return { register, missRows, banRows, requestRows, backendRows, filterSub, purgeCall, tagPattern, tagPreview };
});
