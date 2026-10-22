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
    { id: 'entries', label: 'Entries' },
    { id: 'tags', label: 'Tags' },
    { id: 'clients', label: 'Clients' },
    { id: 'slowlog', label: 'Slowlog' },
  ];
  const TAG_CHIPS = 12;

  function looksLikeUrl(s) {
    const t = String(s || '').trim();
    return /^https?:\/\/\S+$/i.test(t) || /^\/\S*$/.test(t);
  }

  function globEscape(s) {
    return String(s).replace(/[*?[\]\\]/g, '\\$&');
  }

  function typeRows(res) {
    return ((res && res.types) || []).map((t) => ({
      key: t.name,
      db: t.db,
      name: t.label,
      meta: ['db' + t.db, fmt.num(t.keys) + (t.keys === 1 ? ' entry' : ' entries'), t.name === 'session' ? '' : t.compression ? t.compression : ''].filter(Boolean).join(' · '),
      note: t.serializerNote || '',
      layout: t.layout,
      cacheType: t.name,
      raw: t,
    }));
  }

  const TAG_INDEX = { symfony: 'cache:tags:', cm: 'zc:ti:' };

  function magentoTag(tag) {
    return String(tag).toUpperCase().replace(/\./g, '__').replace(/[^A-Za-z0-9_]/g, '_');
  }

  // Follows tagKey in the redis adapter. Without one known layout and id
  // prefix for the db the server picks the key itself, so none is named.
  function tagCall(types, db, tag) {
    const rows = typeRows(types).filter((t) => String(t.db) === String(db));
    const one = rows.length && rows.every((t) => t.layout === rows[0].layout && t.raw.prefix === rows[0].raw.prefix) ? rows[0] : null;
    const base = one && TAG_INDEX[one.layout] && one.raw.prefix ? TAG_INDEX[one.layout] + one.raw.prefix : '';
    if (!base) return 'SMEMBERS the tag index for ' + tag + ' (cache:tags: or zc:ti:, by cache layout), then UNLINK each entry';
    const norm = magentoTag(tag);
    return 'SMEMBERS ' + base + tag + (norm !== tag ? ' (or ' + base + norm + ')' : '') + ', then UNLINK each entry';
  }

  function expiryText(expire, infinite, ttl, now) {
    if (infinite) return 'never expires';
    if (expire) {
      const s = expire - Math.floor((now === undefined ? Date.now() : now) / 1000);
      return s > 0 ? 'expires in ' + fmt.span(s) : 'expired';
    }
    if (ttl > 0) return 'expires in ' + fmt.span(ttl);
    if (ttl === -1) return 'never expires';
    return '';
  }

  const INDEX_FAMILIES = ['tag', 'index', 'reverse-index'];

  function keyRows(keys, selectedKey, raw) {
    return (keys || []).filter((k) => raw || !INDEX_FAMILIES.includes(k.family)).map((k) => ({
      key: k.key,
      title: raw ? k.key : k.id || k.key,
      meta: [k.type, k.family, k.memory ? fmt.bytes(k.memory) : '', k.ttl > 0 ? 'ttl ' + fmt.span(k.ttl) : k.ttl === -1 ? 'no ttl' : ''].filter(Boolean).join(' · '),
      selected: k.key === selectedKey ? 'true' : 'false',
    }));
  }

  function contextText(ctx) {
    if (!ctx) return '';
    return Object.keys(ctx)
      .map((k) => k + ' ' + (typeof ctx[k] === 'object' ? JSON.stringify(ctx[k]) : ctx[k]))
      .join(' · ');
  }

  function previewText(e) {
    const text = e.preview || '';
    if (e.dataForm !== 'json' || e.truncated) return text;
    try {
      return JSON.stringify(JSON.parse(text), null, 2);
    } catch (err) {
      return text;
    }
  }

  function detailOf(v, now) {
    if (!v) return null;
    const e = v.entry;
    const rows = [];
    const add = (k, val) => {
      if (val !== '' && val !== undefined && val !== null) rows.push({ k, v: String(val) });
    };
    if (e) {
      add('URL', e.url);
      add('Context', contextText(e.context));
      add('Stored', [e.dataForm, e.compression, e.dataSize ? fmt.bytes(e.dataSize) + ' decoded' : ''].filter(Boolean).join(' · '));
      add('Cached', e.mtime ? fmt.ago(e.mtime * 1000, now) : '');
    } else {
      add('Type', [v.type, v.encoding].filter(Boolean).join(' · '));
      add('Size', v.size ? fmt.bytes(v.size) : '');
    }
    let body = '';
    if (e) body = previewText(e);
    else if (v.value !== undefined) body = fmt.pretty(v.value);
    const tags = e ? e.tags || [] : [];
    return {
      title: e && (v.family === 'entry' || e.layout) ? 'Cache entry' : 'Key',
      expiry: expiryText(e ? e.expire : 0, e ? e.infinite : false, v.ttl, now),
      rows,
      tags,
      body,
      tooLarge: !!v.tooLarge,
      truncated: !!(v.truncated || (e && e.truncated)),
      foot: [v.key ? 'Raw key ' + v.key : '', e && e.layout ? 'layout ' + e.layout : '', v.memory ? fmt.bytes(v.memory) + ' in memory' : ''].filter(Boolean).join(' · '),
    };
  }

  function tagRows(res) {
    return ((res && res.tags) || []).map((t) => ({ key: t.tag, tag: t.tag, count: fmt.num(t.count) }));
  }

  function memberRows(res) {
    return ((res && res.members) || []).map((m) => ({ key: m.key, id: m.id, rawKey: m.key, exists: m.exists, existsCls: m.exists ? '' : 'text-warn', meta: m.exists ? '' : 'stale: the entry is gone' }));
  }

  function clientRows(res) {
    return ((res && res.clients) || []).map((c, i) => ({
      key: c.id || i,
      who: c.self ? 'Inspector' : [c.container, c.role, c.process].filter(Boolean).join(' · ') || c.name || c.addr,
      addr: c.addr,
      db: 'db' + c.db,
      cmd: c.cmd,
      age: fmt.span(Number(c.age)),
      idle: fmt.span(Number(c.idle)),
    }));
  }

  function slowRows(list) {
    return (list || []).map((s) => ({
      key: s.id,
      time: fmt.clock(s.time).slice(0, 8),
      took: fmt.millis((s.micros || 0) / 1000),
      cmd: fmt.truncate((s.args || []).join(' '), 200),
      from: s.clientName || s.clientAddr || '',
    }));
  }

  function latencyRows(list) {
    return (list || []).slice(0, 12).map((l) => ({ key: l.cmd, cmd: l.cmd, p50: fmt.millis(l.p50 / 1000), p99: fmt.millis(l.p99 / 1000) }));
  }

  function commandRows(list) {
    return (list || [])
      .slice()
      .sort((a, b) => b.usec - a.usec)
      .slice(0, 8)
      .map((c) => ({ key: c.cmd, cmd: c.cmd, calls: fmt.num(c.calls), total: fmt.millis(c.usec / 1000), per: c.usecPerCall < 1000 ? c.usecPerCall.toFixed(1) + ' µs' : fmt.millis(c.usecPerCall / 1000), failed: c.failed ? fmt.num(c.failed) : '' }));
  }

  function monitorSub(db, match) {
    const pairs = [];
    if (String(db).trim() !== '') pairs.push('db=' + String(db).trim());
    if (match && match.trim()) pairs.push('match=' + match.trim());
    return pairs.length ? 'redis:monitor;' + pairs.join(';') : 'redis:monitor';
  }

  function register(Alpine, I) {
    Alpine.data('redis', () => {
      let stopBrowse = null;
      // Any hash change replaces the route object, so the effect re-runs on
      // a Raw toggle too; only entering the section should fetch.
      let browseKey = '';
      let detailSeq = 0;
      let tagSeq = 0;
      return view.compose(view.serviceBase('redis', I, Alpine), {
        tab: 'entries',
        types: null,
        typesError: '',
        selType: '',
        keys: [],
        cursor: '0',
        keysLoaded: false,
        query: '',
        note: '',
        detail: null,
        detailKey: '',
        detailDb: 0,
        detailError: '',
        tagList: null,
        tagsHidden: 0,
        tagsPartial: false,
        showViewTags: false,
        tagMembers: null,
        tagSel: '',
        showAllTags: false,
        clients: null,
        slow: null,
        error: '',
        liveDb: '',
        liveMatch: '',
        actKey: '',
        actTag: '',
        actDb: '0',
        actType: '',
        actFlush: '0',

        init() {
          this.startPolling();
          stopBrowse = Alpine.effect(() => {
            const r = Alpine.store('app').route;
            const key = r.tab === 'redis' ? r.section : '';
            if (key === browseKey) return;
            browseKey = key;
            if (r.tab !== 'redis') return;
            if (r.section === 'browse') setTimeout(() => this.loadTypes(), 0);
            else if (r.section === 'actions' && !this.types) setTimeout(() => this.fetchInto('types', '/api/redis/types'), 0);
          });
        },
        destroy() {
          this.stopPolling();
          if (stopBrowse) Alpine.release(stopBrowse);
          stopBrowse = null;
        },

        get typeList() {
          return typeRows(this.types).map((t) => Object.assign({ current: t.key === this.selType ? 'true' : 'false' }, t));
        },
        get hasTypes() {
          return this.typeList.length > 0;
        },
        get generic() {
          return !!(this.types && this.types.generic);
        },
        get currentType() {
          return typeRows(this.types).find((t) => t.key === this.selType) || null;
        },
        get currentDb() {
          return this.currentType ? this.currentType.db : 0;
        },
        get typeNote() {
          const t = this.currentType;
          return t ? t.note : '';
        },
        get hasError() {
          return !!this.error;
        },

        get tabList() {
          return TABS.map((t) => ({ id: t.id, label: t.label, pressed: t.id === this.tab ? 'true' : 'false' }));
        },
        get isTabEntries() {
          return this.tab === 'entries';
        },
        get isTabTags() {
          return this.tab === 'tags';
        },
        get isTabClients() {
          return this.tab === 'clients';
        },
        get isTabSlowlog() {
          return this.tab === 'slowlog';
        },
        selectTab(t) {
          this.tab = t.id;
          this.loadTab();
        },
        loadTab() {
          if (this.tab === 'tags') return this.loadTags();
          if (this.tab === 'clients') return this.fetchInto('clients', '/api/redis/clients');
          if (this.tab === 'slowlog') return this.fetchInto('slow', '/api/redis/slowlog');
          return null;
        },
        async fetchInto(field, path, query) {
          try {
            this[field] = await I.client.get(path, { query, quiet: true });
            this.error = '';
          } catch (e) {
            this.error = e.message;
          }
        },

        async loadTypes() {
          await this.fetchInto('types', '/api/redis/types');
          if (!this.types) return;
          const first = typeRows(this.types)[0];
          if (!this.selType && first) await this.pickType(first);
          else if (!this.keysLoaded && this.selType) await this.loadKeys(true);
        },
        async pickType(t) {
          detailSeq++;
          tagSeq++;
          this.selType = t.key;
          this.tagList = null;
          this.tagMembers = null;
          this.detail = null;
          this.detailKey = '';
          await this.loadKeys(true);
          if (this.tab === 'tags') this.loadTags();
        },

        async loadKeys(reset) {
          const q = this.query.trim();
          if (reset) {
            this.keys = [];
            this.cursor = '0';
            this.note = '';
          }
          if (q && looksLikeUrl(q)) return this.findUrl(q);
          const match = q ? '*' + globEscape(q) + '*' : '';
          try {
            const r = await I.client.get('/api/redis/keys', { query: { db: this.currentDb, match, cursor: this.cursor === '0' ? '' : this.cursor }, quiet: true });
            this.keys = this.keys.concat(r.keys);
            this.cursor = r.cursor;
            this.keysLoaded = true;
            this.error = '';
            if (!this.keys.length && r.cursor === '0') this.note = q ? 'No key in db' + this.currentDb + ' contains "' + q + '".' : 'db' + this.currentDb + ' is empty.';
          } catch (e) {
            this.error = e.message;
          }
          return null;
        },
        async findUrl(url) {
          const seq = ++detailSeq;
          try {
            const r = await I.client.get('/api/redis/find', { query: { url }, quiet: true });
            if (seq !== detailSeq) return;
            this.error = '';
            if (r.found) {
              this.detail = detailOf(r.entry);
              this.detailKey = r.entry.key;
              this.detailDb = r.db;
              this.keys = [];
              this.note = 'Found the page cache entry for ' + url + '.';
            } else {
              this.note = r.reason === 'unknown' ? 'Inspector could not work out the cache id for that URL. Search by cache id or tag instead.' : r.reason || 'Not found.';
            }
          } catch (e) {
            if (seq === detailSeq) this.error = e.message;
          }
        },
        search() {
          return this.loadKeys(true);
        },
        get keyList() {
          return keyRows(this.keys, this.detailKey, this.raw);
        },
        get hasKeys() {
          return this.keyList.length > 0;
        },
        get hasMore() {
          return this.cursor !== '0' && !looksLikeUrl(this.query.trim());
        },
        loadMore() {
          return this.loadKeys(false);
        },
        get hasNote() {
          return !!this.note;
        },

        async openKey(row, force) {
          const seq = ++detailSeq;
          this.detailError = '';
          this.detailKey = row.key;
          this.detailDb = this.currentDb;
          try {
            const v = await I.client.get('/api/redis/key', { query: { db: this.currentDb, key: row.key, force: force ? 1 : '' }, quiet: true });
            if (seq !== detailSeq) return;
            this.detail = detailOf(v);
          } catch (e) {
            if (seq !== detailSeq) return;
            this.detail = null;
            this.detailError = e.message;
          }
        },
        loadAnyway() {
          return this.openKey({ key: this.detailKey }, true);
        },
        get hasDetail() {
          return !!this.detail;
        },
        get hasDetailError() {
          return !!this.detailError;
        },
        get dTitle() {
          return this.detail ? this.detail.title : '';
        },
        get dExpiry() {
          return this.detail ? this.detail.expiry : '';
        },
        get dRows() {
          return this.detail ? this.detail.rows : [];
        },
        get dTags() {
          if (!this.detail) return [];
          const all = this.detail.tags;
          const list = this.showAllTags ? all : all.slice(0, TAG_CHIPS);
          return list.map((t) => ({ key: t, tag: t }));
        },
        get hasDTags() {
          return !!this.detail && this.detail.tags.length > 0;
        },
        get moreTags() {
          return this.detail && !this.showAllTags && this.detail.tags.length > TAG_CHIPS ? '+' + (this.detail.tags.length - TAG_CHIPS) : '';
        },
        get hasMoreTags() {
          return !!this.moreTags;
        },
        expandTags() {
          this.showAllTags = true;
        },
        get dBody() {
          return this.detail ? this.detail.body : '';
        },
        get hasBody() {
          return !!this.dBody;
        },
        get dFoot() {
          return this.detail ? this.detail.foot : '';
        },
        get dTooLarge() {
          return !!this.detail && this.detail.tooLarge;
        },
        get dTruncated() {
          return !!this.detail && this.detail.truncated;
        },
        async deleteEntry() {
          const key = this.detailKey;
          const db = this.detailDb;
          const res = await I.confirm({
            kind: 'redis',
            action: 'delete-key',
            target: key,
            params: { db },
            title: 'Delete this cache entry?',
            effect: 'The entry and its tag index records are removed from db' + db + '. Magento rebuilds it on the next request that needs it.',
            call: 'UNLINK ' + key,
            button: 'Delete entry',
          });
          if (res) {
            detailSeq++;
            this.detail = null;
            this.detailKey = '';
            this.loadKeys(true);
          }
        },

        async loadTags() {
          try {
            const r = await I.client.get('/api/redis/tags', { query: { db: this.currentDb, all: this.showViewTags ? 1 : '' }, quiet: true });
            this.tagList = r.tags;
            this.tagsHidden = r.hidden;
            this.tagsPartial = r.partial;
            this.error = '';
          } catch (e) {
            this.error = e.message;
          }
        },
        get tagRowsList() {
          return tagRows({ tags: this.tagList }).map((t) => Object.assign({ current: t.tag === this.tagSel ? 'true' : 'false' }, t));
        },
        get hasTagRows() {
          return this.tagRowsList.length > 0;
        },
        get tagNote() {
          const parts = [];
          if (this.tagsHidden && !this.showViewTags) parts.push(this.tagsHidden + ' per-view tags hidden');
          if (this.tagsPartial) parts.push('list cut short, narrow it down');
          return parts.join(' · ');
        },
        toggleViewTags() {
          this.showViewTags = !this.showViewTags;
          return this.loadTags();
        },
        async pickTag(tag) {
          this.tagSel = tag;
          this.tab = 'tags';
          const seq = ++tagSeq;
          try {
            const m = await I.client.get('/api/redis/tag', { query: { db: this.currentDb, tag }, quiet: true });
            if (seq !== tagSeq) return;
            this.tagMembers = m;
            this.error = '';
          } catch (e) {
            if (seq === tagSeq) this.error = e.message;
          }
        },
        pickTagChip(t) {
          this.tab = 'tags';
          this.loadTags();
          return this.pickTag(t.tag);
        },
        get memberList() {
          return memberRows(this.tagMembers);
        },
        get hasMembers() {
          return !!this.tagMembers;
        },
        get memberTitle() {
          const m = this.tagMembers;
          return m ? m.tag + ': ' + fmt.num(m.total) + (m.total === 1 ? ' entry' : ' entries') + (m.stale ? ', ' + m.stale + ' stale' : '') : '';
        },
        get memberTruncated() {
          return !!(this.tagMembers && this.tagMembers.truncated);
        },
        openMember(m) {
          this.tab = 'entries';
          return this.openKey({ key: m.rawKey }, false);
        },
        async cleanTag() {
          const m = this.tagMembers;
          if (!m) return;
          const res = await I.confirm({
            kind: 'redis',
            action: 'clean-tag',
            target: m.tag,
            params: { db: m.db },
            title: 'Clean tag ' + m.tag + '?',
            effect: fmt.num(m.total) + (m.total === 1 ? ' entry that carries this tag is' : ' entries that carry this tag are') + ' deleted from db' + m.db + ', the way Magento cleans a tag.',
            call: 'SMEMBERS ' + m.key + ', then UNLINK each entry',
            button: 'Clean tag',
          });
          if (res) {
            this.tagMembers = null;
            this.loadTags();
          }
        },

        get clientList() {
          return clientRows(this.clients);
        },
        get slowList() {
          return slowRows(this.slow && this.slow.slowlog);
        },
        get latencyList() {
          return latencyRows(this.slow && this.slow.latency);
        },
        get commandList() {
          return commandRows(this.slow && this.slow.commandstats);
        },
        get topCommands() {
          return commandRows(this.health && this.health.raw && this.health.raw.commandstats);
        },
        get hasTopCommands() {
          return this.topCommands.length > 0;
        },

        get monitorSub() {
          return monitorSub(this.liveDb, this.liveMatch);
        },
        get monitorOpen() {
          return Alpine.store('drawer').has(this.monitorSub);
        },
        get monitorLabel() {
          return this.monitorOpen ? 'Monitor is open in the drawer' : 'Open the monitor';
        },
        get monitorInvalid() {
          return this.liveMatch.includes(';') || String(this.liveDb).includes(';');
        },
        openMonitor() {
          if (this.monitorInvalid) return;
          this.openDrawer(this.monitorSub);
        },

        get actionTypes() {
          const m = Alpine.store('app').magento;
          const names = m && m.cacheTypes ? Object.keys(m.cacheTypes).sort() : [];
          return names.map((n) => ({ value: n, label: n + (m.cacheTypes[n] ? '' : ' (disabled)') }));
        },
        get dbOptions() {
          const seen = {};
          const out = [];
          for (const d of (this.types && this.types.databases) || []) {
            seen[d.db] = true;
            out.push({ value: String(d.db), label: 'db' + d.db + ' · ' + fmt.num(d.keys) + ' keys' });
          }
          for (const t of typeRows(this.types)) {
            if (!seen[t.db]) {
              seen[t.db] = true;
              out.push({ value: String(t.db), label: 'db' + t.db + ' · empty' });
            }
          }
          return out.sort((a, b) => Number(a.value) - Number(b.value));
        },
        get canDeleteKey() {
          return !this.lan && this.actKey.trim() !== '';
        },
        get canCleanTag() {
          return !this.lan && this.actTag.trim() !== '';
        },
        get canCleanType() {
          return !this.lan && this.actType !== '';
        },
        async actDeleteKey() {
          const key = this.actKey.trim();
          const res = await I.confirm({
            kind: 'redis',
            action: 'delete-key',
            target: key,
            params: { db: this.actDb },
            title: 'Delete ' + key + '?',
            effect: 'The key is removed from db' + this.actDb + ' together with its tag index records when it is a Magento cache entry.',
            call: 'UNLINK ' + key,
            button: 'Delete entry',
          });
          if (res) this.actKey = '';
        },
        async actCleanTag() {
          const tag = this.actTag.trim();
          const res = await I.confirm({
            kind: 'redis',
            action: 'clean-tag',
            target: tag,
            params: { db: this.actDb },
            title: 'Clean tag ' + tag + '?',
            effect: 'Every entry that carries this tag is deleted from db' + this.actDb + '.',
            call: tagCall(this.types, this.actDb, tag),
            button: 'Clean tag',
          });
          if (res) this.actTag = '';
        },
        actCleanType() {
          return I.confirm({
            kind: 'redis',
            action: 'clean-type',
            target: this.actType,
            title: 'Clean cache type ' + this.actType + '?',
            effect: 'Runs bin/magento cache:clean for this type inside the php-fpm container.',
            call: 'bin/magento cache:clean ' + this.actType,
            button: 'Clean cache type',
          });
        },
        actFlushDb() {
          const db = String(this.actFlush);
          return I.confirm({
            kind: 'redis',
            action: 'flushdb',
            target: db,
            title: 'Flush database ' + db + '?',
            effect: 'Every key in db' + db + ' is deleted, including anything that is not a Magento cache entry.',
            call: 'FLUSHDB (db' + db + ')',
            button: 'Flush db' + db,
            typedLabel: 'Type ' + db + ' to confirm',
          });
        },
      });
    });
  }

  return { register, looksLikeUrl, globEscape, typeRows, tagCall, expiryText, keyRows, detailOf, tagRows, memberRows, clientRows, slowRows, latencyRows, commandRows, monitorSub, contextText };
});
