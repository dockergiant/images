document.addEventListener('alpine:init', () => {
  const Alpine = window.Alpine;
  const I = window.Inspector;
  const fmt = I.fmt;

  const MAX_ROWS = 2000;
  const ROW_H = 24;
  const OVERSCAN = 12;
  // What the server sends when the Performance page stops a source (sse.go).
  const STOPPED = "stopped from Inspector's Performance page";

  // Opt-in streams change the service (slowlog thresholds, vhost tracing), so
  // starting one from anywhere, rail, palette or a view button, goes through
  // the consent dialog first.
  Alpine.store('drawer', {
    open: false,
    selected: [],
    pending: '',

    show(sub) {
      const s = I.api.canonicalSub(sub);
      if (!this.selected.includes(s) && isOptIn(s)) {
        const session = Alpine.store('app').session;
        if (session && session.lan) {
          I.toast('Refused: ' + (session.lanReason || 'Traefik listens beyond loopback, so Inspector changes no service.'), 'warn');
          return;
        }
        this.pending = s;
        return;
      }
      this.add(s);
    },

    add(s) {
      if (!this.selected.includes(s)) this.selected = this.selected.concat([s]);
      this.open = true;
    },

    accept() {
      const s = this.pending;
      this.pending = '';
      if (s) this.add(s);
    },

    decline() {
      this.pending = '';
    },

    hide(sub) {
      const s = I.api.canonicalSub(sub);
      this.selected = this.selected.filter((x) => x !== s);
      if (!this.selected.length) this.open = false;
      refreshCaptures();
    },

    has(sub) {
      return this.open && this.selected.includes(I.api.canonicalSub(sub));
    },

    toggle(sub) {
      if (this.has(sub)) this.hide(sub);
      else this.show(sub);
    },

    toggleOpen() {
      this.open = !this.open;
      if (!this.open) refreshCaptures();
    },
  });

  function isOptIn(sub) {
    const s = Alpine.store('app').session;
    const [kind, name] = sub.split(';')[0].split(':');
    const a = s && (s.adapters || []).find((x) => x.kind === kind);
    const st = a && (a.streams || []).find((x) => x.name === name);
    return !!(st && st.optIn);
  }

  // The header reads the server's capture list, which only the overview poll
  // refreshes; reread it once the restore after a close has had time to run.
  let captureTimer = 0;
  function refreshCaptures() {
    clearTimeout(captureTimer);
    captureTimer = setTimeout(() => I.loadOverview().catch(() => {}), 1500);
  }

  function drawer() {
    return window.Alpine.store('drawer');
  }

  function baseOf(sub) {
    return sub.split(';')[0];
  }

  Alpine.data('drawer', () => {
    let rows = [];
    let seq = 0;
    let off = null;
    let lastKey = '';
    let cache = { v: -1, f: null, list: [] };
    let frame = 0;
    let stopStatus = null;
    let resize = null;
    let effects = [];

    return {
      version: 0,
      filter: '',
      paused: false,
      skipped: 0,
      scrollTop: 0,
      viewportH: 320,
      selectedId: 0,
      notices: {},
      status: { state: 'idle', error: '' },

      init() {
        stopStatus = I.stream.watch((st) => {
          this.status = st;
        });
        effects = [
          Alpine.effect(() => {
            const d = drawer();
            const subs = d.open ? d.selected.slice() : [];
            this.resubscribe(subs);
          }),
          Alpine.effect(() => {
            if (!drawer().open) this.dropOptIn();
            else this.$nextTick(() => this.measure());
          }),
        ];
        this.$nextTick(() => this.observe());
      },

      destroy() {
        for (const fx of effects) Alpine.release(fx);
        effects = [];
        if (off) off();
        if (stopStatus) stopStatus();
        if (resize) resize.disconnect();
      },

      measure() {
        const vp = this.$refs.viewport;
        if (vp && vp.clientHeight) this.viewportH = vp.clientHeight;
      },

      observe() {
        const vp = this.$refs.viewport;
        if (!vp || typeof ResizeObserver !== 'function') return;
        resize = new ResizeObserver(() => {
          if (vp.clientHeight) this.viewportH = vp.clientHeight;
        });
        resize.observe(vp);
      },

      resubscribe(subs) {
        const key = subs.slice().sort().join('\n');
        if (key === lastKey) return;
        lastKey = key;
        if (off) off();
        off = null;
        const keep = {};
        for (const s of subs) if (this.notices[s]) keep[s] = this.notices[s];
        this.notices = keep;
        if (subs.length) off = I.stream.subscribe(subs, (ev) => this.receive(ev));
      },

      dropOptIn() {
        const d = drawer();
        const optIn = new Set(this.streams.filter((s) => s.optIn).map((s) => s.sub));
        const next = d.selected.filter((s) => !optIn.has(baseOf(s)));
        if (next.length !== d.selected.length) d.selected = next;
      },

      receive(ev) {
        if (ev.type === 'redis.monitor') {
          if (ev.data && ev.data.text) this.setNotice(ev.sub, 'warn', ev.data.text);
          return;
        }
        if (ev.type === 'search.capture') {
          const c = ev.data || {};
          if (c.capturing) {
            const on = c.indices && c.indices.length ? c.indices.join(', ') : 'the Magento indices';
            this.setNotice(ev.sub, 'warn', 'Slowlog at 0 ms on ' + on + ' until ' + fmt.clock(c.until).slice(0, 8) + '. The prior thresholds come back when you close the drawer.');
          } else {
            this.setNotice(ev.sub, 'info', 'Capture ended' + (c.reason ? ': ' + c.reason : '') + '. Slowlog thresholds are restored.');
          }
          return;
        }
        if (ev.type === 'stream.error') {
          this.setNotice(ev.sub, 'error', typeof ev.data === 'string' ? ev.data : fmt.pretty(ev.data));
          // Kept selected, the next change to the drawer's subs would open a
          // stream with it again and restart what was just stopped.
          if (ev.data === STOPPED) {
            drawer().hide(ev.sub);
            return;
          }
        }
        if (ev.type === 'gap') {
          ev = Object.assign({}, ev, { type: 'stream.error', data: 'The stream fell behind and dropped events; newer rows continue below.' });
        }
        if (this.paused) {
          this.skipped++;
          return;
        }
        rows.unshift(Object.freeze(fmt.eventRow(ev, ++seq)));
        if (rows.length > MAX_ROWS) rows.length = MAX_ROWS;
        this.bump();
      },

      bump() {
        if (frame) return;
        const run = () => {
          frame = 0;
          this.version++;
        };
        frame = typeof requestAnimationFrame === 'function' ? requestAnimationFrame(run) : setTimeout(run, 16);
      },

      setNotice(sub, tone, text) {
        this.notices = Object.assign({}, this.notices, { [sub]: { tone, text } });
      },

      get open() {
        return drawer().open;
      },

      toggleOpen() {
        drawer().toggleOpen();
      },

      close() {
        drawer().open = false;
        refreshCaptures();
      },

      get session() {
        return Alpine.store('app').session;
      },

      get streams() {
        return I.rail.streamList(this.session);
      },

      // The streams that run, as removable pills in the drawer bar.
      get pills() {
        const d = drawer();
        return I.rail
          .live({ session: this.session, drawer: { open: true, selected: d.selected } })
          .filter((st) => st.on === 'true')
          .map((st) => ({ sub: st.sub, label: st.label, title: st.title, cls: fmt.kindClass(st.kind), removeLabel: 'Stop ' + st.label }));
      },

      get hasPills() {
        return drawer().selected.length > 0;
      },

      // With nothing selected the drawer offers every stream, so it works on a
      // phone where the rail is folded away.
      get offers() {
        return I.rail
          .live({ session: this.session, drawer: { open: true, selected: drawer().selected } })
          .filter((st) => st.on !== 'true' && !st.filtered)
          .map((st) => ({ sub: st.sub, label: st.label, title: st.title, disabled: st.disabled, optIn: st.optIn, cls: fmt.kindClass(st.kind) }));
      },

      start(offer) {
        if (!offer.disabled) drawer().show(offer.sub);
      },

      stop(pill) {
        drawer().hide(pill.sub);
      },

      get capturing() {
        const d = drawer();
        if (!d.open) return '';
        const names = this.streams.filter((s) => s.optIn && d.selected.includes(s.sub)).map((s) => s.label);
        if (!names.length) return '';
        return names.join(', ') + '. This changes the service while it runs and switches back when you close the drawer, after 30 minutes, or when Inspector stops.';
      },

      get noticeList() {
        return Object.keys(this.notices).map((sub) => ({ sub, text: this.notices[sub].text, cls: fmt.toneClass(this.notices[sub].tone) }));
      },

      get statusText() {
        const d = drawer();
        if (!d.open) return 'drawer closed';
        if (!d.selected.length) return '';
        switch (this.status.state) {
          case 'open':
            return 'streaming';
          case 'connecting':
            return 'connecting…';
          case 'reconnecting':
            return 'reconnecting…';
          case 'closed':
            return this.status.error || 'stream closed';
          default:
            return '';
        }
      },

      get statusCls() {
        if (!drawer().open || !drawer().selected.length) return 'text-muted';
        if (this.status.state === 'open') return 'tone-ok';
        if (this.status.state === 'closed') return 'tone-error';
        return 'tone-warn';
      },

      get canRetry() {
        return drawer().open && drawer().selected.length > 0 && this.status.state === 'closed';
      },

      retry() {
        I.stream.retry();
      },

      get filtered() {
        const v = this.version;
        const f = this.filter.trim().toLowerCase();
        if (cache.v === v && cache.f === f) return cache.list;
        const list = f ? rows.filter((r) => r.search.includes(f)) : rows.slice();
        cache = { v, f, list };
        return list;
      },

      get total() {
        return this.filtered.length;
      },

      get countText() {
        const n = this.total;
        let t = fmt.num(n) + (n === 1 ? ' row' : ' rows');
        if (this.filter.trim()) t += ' match';
        if (this.skipped) t += ' · ' + fmt.num(this.skipped) + ' skipped while paused';
        return t;
      },

      get startIndex() {
        return Math.max(0, Math.floor(this.scrollTop / ROW_H) - OVERSCAN);
      },

      get visibleRows() {
        const start = this.startIndex;
        const count = Math.ceil(this.viewportH / ROW_H) + OVERSCAN * 2;
        return this.filtered.slice(start, start + count).map((r) => ({
          id: r.id,
          time: r.time,
          badge: r.badge,
          text: r.text,
          kindCls: fmt.kindClass(r.kind),
          toneCls: fmt.toneClass(r.tone),
          selected: r.id === this.selectedId ? 'true' : 'false',
        }));
      },

      spacerStyle() {
        return { height: this.total * ROW_H + 'px' };
      },

      windowStyle() {
        return { transform: 'translateY(' + this.startIndex * ROW_H + 'px)' };
      },

      onScroll() {
        const vp = this.$refs.viewport;
        if (vp) this.scrollTop = vp.scrollTop;
      },

      get isEmpty() {
        return this.total === 0;
      },

      get emptyText() {
        const d = drawer();
        if (!d.selected.length) return 'Rows appear here as they happen, newest first.';
        if (this.filter.trim()) return 'No rows match the filter.';
        return 'Waiting for events. Load a page in the store to see it here.';
      },

      pick(row) {
        this.selectedId = this.selectedId === row.id ? 0 : row.id;
      },

      get selectedRow() {
        if (!this.selectedId || this.version < 0) return null;
        return rows.find((r) => r.id === this.selectedId) || null;
      },

      get hasDetail() {
        return !!this.selectedRow;
      },

      get detailTitle() {
        const r = this.selectedRow;
        return r ? r.type + ' · ' + r.sub + ' · ' + r.time : '';
      },

      // A trace row is where a slow page shows up first, so its detail
      // links straight to that trace's bottlenecks.
      get traceHref() {
        const r = this.selectedRow;
        const id = r && r.type === 'trace' && r.event && r.event.data ? r.event.data.id : '';
        return id ? '#/traces/' + encodeURIComponent(id) : '';
      },

      get detailText() {
        const r = this.selectedRow;
        return r ? fmt.pretty(r.event.data) : '';
      },

      closeDetail() {
        this.selectedId = 0;
      },

      togglePause() {
        this.paused = !this.paused;
        if (!this.paused) this.skipped = 0;
      },

      get pauseLabel() {
        return this.paused ? 'Resume' : 'Pause';
      },

      clear() {
        rows = [];
        this.skipped = 0;
        this.selectedId = 0;
        this.version++;
      },
    };
  });

  const CONSENT = {
    'search:capture': {
      title: 'Capture every OpenSearch query?',
      effect: 'Inspector sets the search slowlog thresholds to 0 ms on every Magento index, so the engine logs each query. The prior thresholds come back when you close the drawer, after 30 minutes, or when Inspector stops.',
      call: 'PUT /<index>/_settings index.search.slowlog.threshold.query.*=0ms',
      button: 'Start capturing',
    },
    'rabbitmq:firehose': {
      title: 'Turn the RabbitMQ firehose on?',
      effect: 'Inspector switches tracing on for the vhost, so the broker copies every published and delivered message to amq.rabbitmq.trace. Tracing goes off again when you close the drawer, after 30 minutes, or when Inspector stops, unless it was on before.',
      call: 'PUT /api/vhosts/%2F {"tracing":true}',
      button: 'Turn the firehose on',
    },
  };

  Alpine.data('consent', () => {
    let fx = null;
    let returnFocus = null;
    return {
      init() {
        fx = Alpine.effect(() => {
          if (drawer().pending) {
            returnFocus = document.activeElement;
            this.$nextTick(() => this.$refs.cancel && this.$refs.cancel.focus());
          } else if (returnFocus) {
            const el = returnFocus;
            returnFocus = null;
            if (document.body.contains(el) && typeof el.focus === 'function') this.$nextTick(() => el.focus());
          }
        });
      },

      destroy() {
        if (fx) Alpine.release(fx);
      },

      get visible() {
        return !!drawer().pending;
      },

      get spec() {
        const sub = drawer().pending;
        return CONSENT[sub.split(';')[0]] || { title: 'Start ' + sub + '?', effect: 'This stream changes the service while it runs and switches back when you close the drawer.', call: '', button: 'Start' };
      },

      get title() {
        return this.spec.title;
      },

      get kindCls() {
        return fmt.kindClass(drawer().pending.split(':')[0]);
      },

      get effect() {
        return this.spec.effect;
      },

      get call() {
        return this.spec.call;
      },

      get hasCall() {
        return !!this.spec.call;
      },

      get button() {
        return this.spec.button;
      },

      accept() {
        drawer().accept();
      },

      cancel() {
        drawer().decline();
      },

      onEscape() {
        if (this.visible) this.cancel();
      },
    };
  });
});
