(function () {
  const I = window.Inspector;
  const fmt = I.fmt;
  const route = I.route;

  const TOAST_MS = { ok: 5000, info: 5000, warn: 8000, error: 10000 };

  let readyResolve;
  I.ready = new Promise((r) => {
    readyResolve = r;
  });

  function store() {
    return window.Alpine.store('app');
  }

  I.toast = function (text, tone) {
    if (!text) return;
    window.dispatchEvent(new CustomEvent('inspector:toast', { detail: { text: String(text), tone: tone || 'info' } }));
  };

  I.client = I.api.createClient({
    fetch: (url, init) => window.fetch(url, init),
    csrf: () => {
      const s = store().session;
      return s ? s.csrf : '';
    },
    refresh: () => I.loadSession(false),
    onError: (e) => I.toast(e.message, 'error'),
  });

  I.stream = I.api.createStream({
    EventSource: window.EventSource,
    fetch: (url, init) => window.fetch(url, init),
  });

  I.go = function (tab, section, rest) {
    window.location.hash = route.format({ tab, section: section || '', rest: rest || '' });
  };

  I.adapter = function (kind) {
    const s = store().session;
    if (!s) return null;
    return s.adapters.find((a) => a.kind === kind) || null;
  };

  I.present = function (kind) {
    if (kind === 'php' || kind === '') return true;
    const a = I.adapter(kind);
    return !!(a && a.info && a.info.present);
  };

  I.label = function (kind) {
    const a = I.adapter(kind);
    return fmt.serviceLabel(kind, a && a.info);
  };

  I.actionSpec = function (kind, name) {
    const a = I.adapter(kind);
    if (!a) return null;
    return a.actions.find((x) => x.name === name) || null;
  };

  // The first session names the build this page loaded with; a later one with
  // another version or UI hash means the image was updated under it.
  let loaded = null;
  I.loadSession = async function (refresh) {
    const s = await I.client.get('/api/session', { query: { refresh: refresh ? 1 : '' }, quiet: true });
    store().session = s;
    if (s && (s.uiHash || s.version)) {
      if (!loaded) loaded = { uiHash: s.uiHash || '', version: s.version || '' };
      else if ((s.uiHash || '') !== loaded.uiHash || (s.version || '') !== loaded.version) store().updated = true;
    }
    return s;
  };

  I.loadOverview = async function () {
    const o = await I.client.get('/api/overview', { quiet: true });
    store().overview = o;
    store().magento = o.magento || null;
    store().captures = o.captures || [];
    return o;
  };

  // Raw lives in the hash (?raw=1) like the rest of the view state; the store
  // field mirrors it for the views that read store().raw.
  I.toggleRaw = function () {
    const r = store().route;
    const query = Object.assign({}, r.query);
    if (query.raw === '1') delete query.raw;
    else query.raw = '1';
    window.location.hash = route.format(Object.assign({}, r, { query }));
  };

  function typing(target) {
    if (!target || target === document.body) return false;
    const tag = target.tagName;
    return tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT' || target.isContentEditable;
  }

  function pathKey(r) {
    return r.tab + '/' + r.section + '/' + r.rest;
  }

  document.addEventListener('alpine:init', () => {
    const Alpine = window.Alpine;
    const first = route.parse(window.location.hash);

    Alpine.store('app', {
      session: null,
      magento: null,
      overview: null,
      captures: [],
      fatal: '',
      updated: false,
      loading: true,
      route: first,
      raw: first.query.raw === '1',
      // What the current screen shows, for the last breadcrumb (a trace's
      // route, a key). Views set it; a route change clears it.
      item: '',
    });

    Alpine.data('shell', () => {
      let onHash = null;
      let poll = null;
      let sheetFx = null;
      let wide = null;
      let onWide = null;
      let titleFx = null;

      return {
        sheetOpen: false,

        init() {
          onHash = () => this.syncRoute();
          window.addEventListener('hashchange', onHash);
          if (!window.location.hash) window.history.replaceState(null, '', route.format(store().route));
          this.boot();
          poll = setInterval(() => {
            if (document.hidden || !store().session) return;
            I.loadSession(false).catch(() => {});
            // The overview view polls it itself; elsewhere the rail dots need it.
            if (store().route.tab !== 'overview') I.loadOverview().catch(() => {});
          }, I.view.POLL_MS);
          sheetFx = Alpine.effect(() => {
            if (this.sheetOpen) this.$nextTick(() => this.focusRail());
          });
          // Views set store().item once they load, so the title follows it
          // as well as the route.
          titleFx = Alpine.effect(() => {
            const s = store().session;
            document.title = I.rail.pageTitle(this.crumbs, s ? s.env : '');
          });
          // The workspace is inert while the sheet is open; widening past the
          // sheet breakpoint must not leave it that way.
          if (typeof window.matchMedia === 'function') {
            wide = window.matchMedia('(min-width: 720px)');
            onWide = (e) => {
              if (e.matches) this.sheetOpen = false;
            };
            wide.addEventListener('change', onWide);
          }
        },

        destroy() {
          if (onHash) window.removeEventListener('hashchange', onHash);
          if (poll) clearInterval(poll);
          if (sheetFx) Alpine.release(sheetFx);
          if (titleFx) Alpine.release(titleFx);
          if (wide && onWide) wide.removeEventListener('change', onWide);
        },

        syncRoute() {
          const next = route.parse(window.location.hash);
          const cur = store().route;
          const moved = pathKey(next) !== pathKey(cur);
          if (moved) {
            store().item = '';
            // The workspace is one scroller for every screen, so a new screen
            // would otherwise open at the previous one's scroll position.
            const main = document.querySelector('main');
            if (main) main.scrollTop = 0;
          }
          store().raw = next.query.raw === '1';
          store().route = next;
          if (this.sheetOpen && !moved) {
            this.closeSheet();
            return;
          }
          this.sheetOpen = false;
          // A new screen takes focus, so a screen reader announces it and the
          // next Tab starts inside it; a link within the screen keeps focus
          // where it is, and so does a filter writing the query.
          if (moved) this.$nextTick(() => this.focusContent());
        },

        focusContent() {
          const main = document.getElementById('content');
          if (!main || main.contains(document.activeElement) || this.modalOpen) return;
          main.focus({ preventScroll: true });
        },

        // ---- modals ----

        get modalOpen() {
          return !!(Alpine.store('palette').open || Alpine.store('confirm').request || Alpine.store('drawer').pending);
        },

        get workspaceInert() {
          return this.sheetOpen || this.modalOpen;
        },

        async boot() {
          store().loading = true;
          store().fatal = '';
          try {
            await I.loadSession(false);
            readyResolve(store().session);
            I.loadOverview().catch(() => {});
          } catch (e) {
            store().fatal = e.message;
          } finally {
            store().loading = false;
          }
        },

        retry() {
          this.boot();
        },

        get loading() {
          return store().loading;
        },

        get fatal() {
          return store().fatal;
        },

        get updated() {
          return store().updated;
        },

        reload() {
          window.location.reload();
        },

        get ready() {
          return !!store().session;
        },

        // ---- keyboard ----

        onKey(ev) {
          if (ev.defaultPrevented) return;
          const k = ev.key;
          if ((k === 'k' || k === 'K') && (ev.metaKey || ev.ctrlKey) && !ev.altKey) {
            ev.preventDefault();
            Alpine.store('palette').toggle();
            return;
          }
          if (ev.metaKey || ev.ctrlKey || ev.altKey) return;
          if (Alpine.store('palette').open || Alpine.store('confirm').request || Alpine.store('drawer').pending) return;
          if (k === 'Escape' && this.sheetOpen) {
            this.closeSheet();
            return;
          }
          if (typing(ev.target)) return;
          if (k === '/') {
            ev.preventDefault();
            Alpine.store('palette').show();
          } else if (k === 'l' || k === 'L') {
            ev.preventDefault();
            Alpine.store('drawer').toggleOpen();
          }
        },

        openPalette() {
          Alpine.store('palette').show();
        },

        get findKey() {
          const p = (navigator.userAgentData && navigator.userAgentData.platform) || navigator.platform || '';
          return /mac|iphone|ipad/i.test(p) ? '⌘K' : 'Ctrl K';
        },

        // ---- rail ----

        toggleSheet() {
          this.sheetOpen = !this.sheetOpen;
        },

        closeSheet() {
          if (!this.sheetOpen) return;
          this.sheetOpen = false;
          this.$nextTick(() => this.$refs.menu && this.$refs.menu.focus());
        },

        focusRail() {
          const el = this.$refs.rail && this.$refs.rail.querySelector('a[href]');
          if (el) el.focus();
        },

        get sheetState() {
          return this.sheetOpen ? 'true' : 'false';
        },

        get services() {
          return I.rail.services({ route: store().route, session: store().session, overview: store().overview }).map((s) => Object.assign(s, { cls: fmt.kindClass(s.kind) }));
        },

        // Below 1100 px the rail shows icons only, so the current service's
        // sections move into the top bar.
        get currentSections() {
          const cur = this.services.find((s) => s.active);
          return cur ? cur.sections.filter((s) => s.id !== 'trace') : [];
        },

        get hasCurrentSections() {
          return this.currentSections.length > 0;
        },

        get liveRows() {
          const d = Alpine.store('drawer');
          return I.rail.live({ session: store().session, drawer: { open: d.open, selected: d.selected } }).map((s) => Object.assign(s, { cls: fmt.kindClass(s.kind), stateLabel: s.on === 'true' ? 'on' : 'off' }));
        },

        get liveCount() {
          return this.liveRows.filter((s) => s.on === 'true').length;
        },

        get liveCountText() {
          const n = this.liveCount;
          return n ? n + ' on' : 'off';
        },

        get liveOn() {
          return this.liveCount > 0 ? 'true' : 'false';
        },

        toggleLive(row) {
          if (row.disabled) return;
          Alpine.store('drawer').toggle(row.sub);
        },

        get envName() {
          const s = store().session;
          return s ? s.env : '';
        },

        get envMeta() {
          const m = store().magento;
          if (!m) return '';
          if (m.generic) return 'generic mode';
          const parts = [];
          if (m.framework) parts.push(m.framework + (m.frameworkVersion ? ' ' + m.frameworkVersion : ''));
          if (m.mode) parts.push(m.mode);
          return parts.join(' · ');
        },

        get versionLine() {
          const s = store().session;
          if (!s) return '';
          return 'Inspector ' + (s.version || 'dev') + ' · LAN ' + (s.lan ? 'on' : 'off');
        },

        get perfHref() {
          return route.format({ tab: 'performance' });
        },

        get perfCurrent() {
          return store().route.tab === 'performance' ? 'page' : null;
        },

        // The pill reads the summary /api/session carries, so it costs no
        // poll of its own.
        get perfPill() {
          const s = store().session;
          return s && I.performance ? I.performance.pill(s.performance) : null;
        },

        get hasPerfPill() {
          return !!this.perfPill;
        },

        get perfPillLabel() {
          return this.perfPill ? this.perfPill.label : '';
        },

        get perfPillOn() {
          return this.perfPill ? this.perfPill.on : 'false';
        },

        get perfPillBusy() {
          return this.perfPill ? this.perfPill.busy : 'false';
        },

        get connectHref() {
          return route.format({ tab: 'connect' });
        },

        get connectCurrent() {
          return store().route.tab === 'connect' ? 'page' : null;
        },

        get lan() {
          const s = store().session;
          return !!(s && s.lan);
        },

        get lanReason() {
          const s = store().session;
          return s ? s.lanReason || '' : '';
        },

        get hasAlert() {
          return this.lan || !!this.capturing;
        },

        get capturing() {
          const s = store().session;
          if (!s) return '';
          const d = Alpine.store('drawer');
          const live = [];
          if (d.open) {
            for (const st of I.rail.streamList(s)) {
              if (st.optIn && d.selected.includes(st.sub)) live.push(st.label);
            }
          }
          if (live.length) return 'Capturing: ' + live.join(', ');
          const keys = store().captures.length ? store().captures : s.captures || [];
          return keys.length ? 'Capturing ' + keys.join(', ') : '';
        },

        // ---- top bar ----

        get crumbs() {
          return I.rail.crumbs({ route: store().route, session: store().session, item: store().item });
        },

        get rawLabel() {
          return this.hasRaw ? I.rail.rawLabel(store().route.tab) : '';
        },

        get hasRaw() {
          const tab = store().route.tab;
          return this.ready && !!I.rail.rawLabel(tab) && I.present(route.kindOf(tab));
        },

        get raw() {
          return store().raw ? 'true' : 'false';
        },

        toggleRaw() {
          I.toggleRaw();
        },

        get drawerOpen() {
          return Alpine.store('drawer').open ? 'true' : 'false';
        },

        get liveBtnLabel() {
          return Alpine.store('drawer').open ? 'Close the live drawer (L)' : 'Open the live drawer (L)';
        },

        toggleDrawer() {
          Alpine.store('drawer').toggleOpen();
        },

        // ---- views ----

        get kindCls() {
          return fmt.kindClass(route.kindOf(store().route.tab));
        },

        showView(tab) {
          if (!this.ready || store().route.tab !== tab) return false;
          return I.present(route.kindOf(tab));
        },

        isAbsent(tab) {
          if (!this.ready || store().route.tab !== tab) return false;
          return !I.present(route.kindOf(tab));
        },

        absentTitle(tab) {
          return I.label(route.kindOf(tab)) + ' is not part of this env';
        },

        absentText(tab) {
          const a = I.adapter(route.kindOf(tab));
          if (a && a.info && a.info.error) return a.info.error;
          return 'Inspector found no ' + I.label(route.kindOf(tab)) + ' container in ' + store().session.env + '. Switch the service on in .env.roll and run roll env up; this page fills in by itself.';
        },
      };
    });

    Alpine.data('toasts', () => ({
      items: [],
      seq: 0,

      get lifted() {
        return Alpine.store('drawer').open ? 'true' : 'false';
      },

      add(ev) {
        const d = ev.detail || {};
        const id = ++this.seq;
        const tone = d.tone || 'info';
        this.items.push({ id, text: d.text, tone, cls: fmt.toneClass(tone) });
        if (this.items.length > 5) this.items.shift();
        setTimeout(() => this.dismissId(id), TOAST_MS[tone] || 6000);
      },

      dismiss(item) {
        this.dismissId(item.id);
      },

      dismissId(id) {
        this.items = this.items.filter((t) => t.id !== id);
      },
    }));
  });
})();
