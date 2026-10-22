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
    { id: 'search', label: 'Search' },
    { id: 'analyzer', label: 'Analyzer' },
    { id: 'mapping', label: 'Mapping' },
  ];

  function healthDot(h) {
    switch (h) {
      case 'green':
        return { cls: 'text-ok', text: 'green' };
      case 'yellow':
        return { cls: 'text-probable', text: 'yellow' };
      case 'red':
        return { cls: 'text-error', text: 'red' };
      default:
        return { cls: 'text-muted', text: h || '' };
    }
  }

  function indexRows(list, showSystem) {
    const out = [];
    for (const ix of list || []) {
      if (ix.system && !showSystem) continue;
      const l = ix.label;
      const h = healthDot(ix.health);
      let name = ix.index;
      if (ix.aliases && ix.aliases.length) name += ' ← ' + ix.aliases.join(', ');
      else if (ix.orphan) name += ' (no alias, left after a reindex)';
      out.push({
        key: ix.index,
        title: ix.orphan ? 'Old ' + (l ? l.entity : '') + ' index' : l ? l.title.split(' · ')[0] : ix.index,
        store: l ? (l.storeCode ? l.storeCode + (l.storeId ? ' (' + l.storeId + ')' : '') : 'store ' + l.storeId) : '',
        docs: fmt.num(ix.docs),
        size: fmt.bytes(ix.bytes),
        health: '● ' + h.text,
        healthCls: h.cls,
        name,
        orphan: !!ix.orphan,
        titleCls: ix.orphan ? 'text-warn' : '',
        rowCls: ix.orphan ? 'text-warn' : '',
        index: ix.index,
      });
    }
    return out;
  }

  function searchTargets(list) {
    const out = [];
    for (const ix of list || []) {
      if (ix.system) continue;
      if (ix.aliases && ix.aliases.length) for (const a of ix.aliases) out.push({ value: a, label: (ix.label ? ix.label.title : ix.index) + ' · ' + a });
      else if (!ix.orphan) out.push({ value: ix.index, label: ix.index });
    }
    return out;
  }

  function defaultTarget(list) {
    const t = searchTargets(list);
    const product = (list || []).find((ix) => ix.label && ix.label.entity === 'product' && ix.aliases && ix.aliases.length);
    if (product) return product.aliases[0];
    return t.length ? t[0].value : '';
  }

  function walkWeights(node, out) {
    const m = /^weight\((?:Synonym\()?([^:()\s]+):/.exec(node.description || '');
    if (m) {
      let boost = 1;
      const find = (n) => {
        if (n.description === 'boost' && typeof n.value === 'number') boost = n.value;
        for (const d of n.details || []) find(d);
      };
      find(node);
      out.push({ field: m[1], value: node.value, boost });
      return;
    }
    for (const d of node.details || []) walkWeights(d, out);
  }

  function whyOf(explanation) {
    if (!explanation) return { text: '', boost: 1 };
    const found = [];
    walkWeights(explanation, found);
    if (!found.length) return { text: '', boost: 1 };
    const byField = {};
    for (const f of found) {
      const cur = byField[f.field] || { value: 0, boost: 1 };
      cur.value += f.value;
      cur.boost = Math.max(cur.boost, f.boost);
      byField[f.field] = cur;
    }
    const ranked = Object.keys(byField)
      .filter((k) => k !== '_search')
      .sort((a, b) => byField[b].value - byField[a].value);
    const top = ranked[0] || '_search';
    const t = byField[top];
    const label = top === '_search' ? 'any searchable field' : top;
    return { text: t.boost > 1 ? label + ' ×' + fmt.num(t.boost) + ' boost' : label + ' match', boost: t.boost };
  }

  function firstLine(s) {
    return String(s === undefined || s === null ? '' : s).split('\n')[0];
  }

  function hitRows(resp) {
    const hits = (resp && resp.hits && resp.hits.hits) || [];
    return hits.map((h, i) => {
      const src = h._source || {};
      const why = whyOf(h._explanation);
      return {
        key: h._id + ':' + i,
        name: firstLine(src.name) || '(no name in the index)',
        sku: Array.isArray(src.sku) ? src.sku[0] : src.sku || '',
        score: typeof h._score === 'number' ? h._score.toFixed(h._score >= 1 ? 1 : 4) : '',
        why: why.text,
        explanation: h._explanation ? JSON.stringify(h._explanation, null, 2) : '',
      };
    });
  }

  function analyzeTokens(resp) {
    if (!resp) return [];
    if (Array.isArray(resp.tokens)) return resp.tokens.map((t) => t.token);
    const d = resp.detail;
    if (!d) return [];
    if (d.tokenfilters && d.tokenfilters.length) return d.tokenfilters[d.tokenfilters.length - 1].tokens.map((t) => t.token);
    if (d.tokenizer) return d.tokenizer.tokens.map((t) => t.token);
    return [];
  }

  function analyzerNames(resp) {
    const out = new Set();
    for (const idx of Object.keys((resp && resp.analysis) || {})) {
      for (const k of Object.keys(resp.analysis[idx].analyzer || {})) out.add(k);
    }
    return Array.from(out).sort();
  }

  function mappingRows(fields) {
    return (fields || []).map((f, i) => ({
      key: f.index + ':' + f.field + ':' + i,
      field: f.field,
      type: f.type,
      analyzer: [f.analyzer, f.searchAnalyzer && f.searchAnalyzer !== f.analyzer ? 'search: ' + f.searchAnalyzer : ''].filter(Boolean).join(' · '),
      copyTo: (f.copyTo || []).join(', '),
    }));
  }

  function register(Alpine, I) {
    Alpine.data('search', () => {
      let poll = null;
      let stopBrowse = null;
      // Any hash change replaces the route object, so the effect re-runs on
      // a Raw toggle too; only entering the section should fetch.
      let browseKey = '';
      return view.compose(view.serviceBase('search', I, Alpine), {
        tabs: TABS,
        tab: 'search',
        indices: null,
        storesError: '',
        showSystem: false,
        error: '',
        target: '',
        term: '',
        profile: false,
        result: null,
        searching: false,
        openHit: '',
        analyzeText: '',
        analyzeAnalyzer: '',
        analyzed: null,
        analyzerList: [],
        mappingFilter: '',
        mapping: null,

        init() {
          this.startPolling();
          stopBrowse = Alpine.effect(() => {
            const r = Alpine.store('app').route;
            const key = r.tab === 'search' ? r.section : '';
            if (key === browseKey) return;
            browseKey = key;
            if (key === 'browse') setTimeout(() => this.loadIndices(), 0);
          });
          poll = setInterval(() => {
            if (!document.hidden && (this.isBrowse || this.isActions)) this.loadIndices();
          }, view.POLL_MS);
        },

        destroy() {
          this.stopPolling();
          if (poll) clearInterval(poll);
          if (stopBrowse) Alpine.release(stopBrowse);
          stopBrowse = null;
        },

        async loadIndices() {
          try {
            const r = await I.client.get('/api/search/indices', { quiet: true });
            this.indices = r.indices;
            this.storesError = r.storesError || '';
            if (!this.target) this.target = defaultTarget(r.indices);
            this.error = '';
          } catch (e) {
            this.error = e.message;
          }
        },

        get hasError() {
          return !!this.error;
        },
        get rows() {
          return indexRows(this.indices, this.showSystem);
        },
        get hasStoresError() {
          return !!this.storesError;
        },
        get targets() {
          return searchTargets(this.indices);
        },
        get orphans() {
          return indexRows(this.indices, false).filter((r) => r.orphan);
        },
        get hasOrphans() {
          return this.orphans.length > 0;
        },
        toggleSystem() {
          this.showSystem = !this.showSystem;
        },

        get tabList() {
          return TABS.map((t) => ({ id: t.id, label: t.label, pressed: t.id === this.tab ? 'true' : 'false' }));
        },
        get isTabSearch() {
          return this.tab === 'search';
        },
        get isTabAnalyzer() {
          return this.tab === 'analyzer';
        },
        get isTabMapping() {
          return this.tab === 'mapping';
        },
        selectTab(t) {
          this.tab = t.id;
          if (t.id === 'mapping' && !this.mapping) this.loadMapping();
          if (t.id === 'analyzer') this.loadAnalyzers();
        },

        get canSearch() {
          return !!this.target && this.term.trim() !== '' && !this.searching;
        },
        async runSearch() {
          if (!this.canSearch) return;
          this.searching = true;
          this.openHit = '';
          try {
            this.result = await I.client.post('/api/search/search', { index: this.target, term: this.term.trim(), explain: true, size: 20 }, { quiet: true });
            this.error = '';
          } catch (e) {
            this.error = e.message;
            this.result = null;
          } finally {
            this.searching = false;
          }
        },
        get hasResult() {
          return !!this.result;
        },
        get hits() {
          return hitRows(this.result && this.result.response).map((h) => Object.assign({ open: h.key === this.openHit }, h));
        },
        get resultTitle() {
          const r = this.result && this.result.response;
          if (!r) return '';
          const n = r.hits.total && typeof r.hits.total === 'object' ? r.hits.total.value : r.hits.total;
          return fmt.num(n) + (n === 1 ? ' product' : ' products');
        },
        get resultTook() {
          const r = this.result && this.result.response;
          return r ? fmt.millis(r.took) : '';
        },
        get shownNote() {
          const r = this.result && this.result.response;
          if (!r) return '';
          const n = r.hits.total && typeof r.hits.total === 'object' ? r.hits.total.value : r.hits.total;
          return n > this.hits.length ? 'Showing the best ' + this.hits.length + '. The storefront pages through the rest.' : '';
        },
        toggleHit(h) {
          this.openHit = this.openHit === h.key ? '' : h.key;
        },
        get openExplanation() {
          const h = this.hits.find((x) => x.key === this.openHit);
          return h ? h.explanation : '';
        },
        get hasOpenHit() {
          return !!this.openHit;
        },

        async loadAnalyzers() {
          if (!this.target) return;
          try {
            this.analyzerList = analyzerNames(await I.client.get('/api/search/index/' + encodeURIComponent(this.target) + '/analyzers', { quiet: true }));
          } catch (e) {
            this.analyzerList = [];
          }
        },
        get canAnalyze() {
          return !!this.target && this.analyzeText.trim() !== '';
        },
        async runAnalyze() {
          if (!this.canAnalyze) return;
          try {
            const body = { index: this.target, text: this.analyzeText };
            if (this.analyzeAnalyzer) body.analyzer = this.analyzeAnalyzer;
            const r = await I.client.post('/api/search/analyze', body, { quiet: true });
            this.analyzed = { tokens: analyzeTokens(r.response), raw: fmt.pretty(r.response) };
            this.error = '';
          } catch (e) {
            this.error = e.message;
            this.analyzed = null;
          }
        },
        get hasAnalyzed() {
          return !!this.analyzed;
        },
        get tokenText() {
          return this.analyzed ? this.analyzeText.trim() + ' → [' + this.analyzed.tokens.join(', ') + ']' : '';
        },
        get analyzedRaw() {
          return this.analyzed ? this.analyzed.raw : '';
        },
        get analyzerOptions() {
          return [{ value: '', label: "the index's default analyzer" }].concat(this.analyzerList.map((a) => ({ value: a, label: a })));
        },

        async loadMapping() {
          if (!this.target) return;
          try {
            const r = await I.client.get('/api/search/index/' + encodeURIComponent(this.target) + '/mapping', { query: { field: this.mappingFilter.trim() }, quiet: true });
            this.mapping = r.fields;
            this.error = '';
          } catch (e) {
            this.error = e.message;
          }
        },
        get mappingList() {
          return mappingRows(this.mapping);
        },

        get slowlogOn() {
          return Alpine.store('drawer').has('search:slowlog');
        },
        get captureOn() {
          return Alpine.store('drawer').has('search:capture');
        },
        get insightsOn() {
          return Alpine.store('drawer').has('search:insights');
        },
        get captureDisabled() {
          return this.lan;
        },
        get captureLabel() {
          return this.captureOn ? 'Stop capturing' : 'Capture all queries';
        },
        get slowlogLabel() {
          return this.slowlogOn ? 'Showing the slowlog' : 'Tail the slowlog';
        },
        get insightsLabel() {
          return this.insightsOn ? 'Showing top queries' : 'Show top queries';
        },
        openInsights() {
          this.openDrawer('search:insights');
        },
        toggleSlowlog() {
          const d = Alpine.store('drawer');
          if (this.slowlogOn) {
            d.hide('search:slowlog');
            return;
          }
          d.hide('search:capture');
          d.show('search:slowlog');
        },
        toggleCapture() {
          const d = Alpine.store('drawer');
          if (this.captureOn) {
            d.hide('search:capture');
            return;
          }
          if (this.lan) return;
          d.hide('search:slowlog');
          d.show('search:capture');
        },

        async deleteOrphan(row) {
          const res = await I.confirm({
            kind: 'search',
            action: 'delete-orphan',
            target: row.index,
            title: 'Delete ' + row.index + '?',
            effect: 'The index has no alias and its store no longer exists, so no storefront reads it. Its ' + row.docs + ' documents (' + row.size + ') are removed from the engine.',
            call: 'DELETE /' + row.index,
            button: 'Delete index',
            typedLabel: 'Type the index name to confirm',
          });
          if (res) this.loadIndices();
        },
      });
    });
  }

  return { register, indexRows, searchTargets, defaultTarget, whyOf, hitRows, analyzeTokens, analyzerNames, mappingRows, healthDot };
});
