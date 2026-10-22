(function (root, factory) {
  const isNode = typeof module === 'object' && module.exports;
  const deps = isNode ? { fmt: require('../lib/format.js'), route: require('../lib/route.js'), charts: require('../lib/charts.js') } : root.Inspector;
  const mod = factory(deps);
  if (isNode) {
    module.exports = mod;
  } else {
    root.Inspector.performance = mod;
    document.addEventListener('alpine:init', () => mod.register(window.Alpine, root.Inspector));
  }
})(this, function (deps) {
  const fmt = deps.fmt;
  const route = deps.route;
  const C = deps.charts;

  const POLL_MS = 5000;
  const MB = 1 << 20;
  const SERVICE_LABELS = { varnish: 'Varnish', redis: 'Valkey / Redis', search: 'OpenSearch', rabbitmq: 'RabbitMQ' };
  const ARM_LABELS = { off: 'Tracing off', lean: 'Lean', full: 'Full', 'full+profile': 'Full + profiling' };
  const ARM_PHRASE = { lean: 'Lean tracing', full: 'Full tracing', 'full+profile': 'Full tracing with profiling' };
  const BUSY_WHY = 'a benchmark is running; it puts your settings back when it ends';

  function isNum(n) {
    return typeof n === 'number' && Number.isFinite(n);
  }

  function dash(s) {
    return s === '' || s === undefined || s === null ? '–' : s;
  }

  // ---- switches -----------------------------------------------------------

  function envText(env) {
    if (!env || !env.tracing) return '.env.roll: tracing off';
    return '.env.roll: tracing on · ' + (env.detail || 'full') + (env.profiling ? ' · profiling' : '');
  }

  function switchRows(php, state) {
    const busy = !!(state && state.busy);
    const pending = (state && state.pending) || '';
    return (php || []).map((s) => {
      const locked = busy || pending === s.service;
      const why = busy ? BUSY_WHY : s.available ? '' : s.reason || '';
      const off = !s.tracing;
      const noLean = /predates/.test(s.detailReason || '');
      const detailWhy = busy ? BUSY_WHY : !s.available ? why : off ? 'tracing is off' : noLean ? s.detailReason : '';
      return {
        key: s.service,
        service: s.service,
        title: s.service,
        reason: s.available ? '' : s.reason || '',
        tracing: { checked: s.tracing ? 'true' : 'false', disabled: locked || !s.available, why },
        detail: {
          why: detailWhy,
          options: ['full', 'lean'].map((id) => ({
            id,
            label: id === 'full' ? 'Full' : 'Lean',
            pressed: s.tracing && s.detail === id ? 'true' : 'false',
            disabled: locked || !s.available || off || (id === 'lean' && noLean),
          })),
        },
        profiling: { checked: s.profiling ? 'true' : 'false', disabled: locked || !s.available || off, why: busy || !s.available ? why : off ? 'tracing is off' : '' },
        envText: envText(s.env),
        differs: !!s.differs,
        lasts: 'This lasts until ' + s.service + ' restarts. To keep it, put this in .env.roll and run roll env up:',
        permanent: s.permanent || '',
      };
    });
  }

  function sampleLabel(id) {
    return id === 'off' ? 'Paused' : id + ' s';
  }

  function sampleOptions(s) {
    const cur = !s ? '' : s.paused ? 'off' : String(s.interval);
    return ((s && s.options) || ['10', '30', '60', 'off']).map((id) => ({ id, label: sampleLabel(id), pressed: id === cur ? 'true' : 'false' }));
  }

  function sampleNote(s) {
    if (!s) return '';
    const def = s.defaultPaused ? 'paused' : 'every ' + s.default + ' s';
    return '.env.roll: ' + def + '. This lasts until Inspector restarts.';
  }

  function sampleDiffers(s) {
    if (!s) return false;
    if (s.paused || s.defaultPaused) return !!s.paused !== !!s.defaultPaused;
    return s.interval !== s.default;
  }

  // ---- cost ---------------------------------------------------------------

  function mbPerMin(v) {
    if (!isNum(v)) return '–';
    return (v < 10 ? v.toFixed(1) : String(Math.round(v))) + ' MB/min';
  }

  // The benchmark's own requests never reach the store, and a load test next
  // to it loses the traces of its off arm; both say so here.
  function tracedSub(intake, minutes) {
    const bench = (intake.skipped && intake.skipped.benchmark) || 0;
    return 'traced in ' + minutes + ' min' + (bench ? ' · ' + fmt.num(bench) + (bench === 1 ? ' benchmark request' : ' benchmark requests') + ' left out' : '');
  }

  function droppedSub(intake) {
    const n = intake.rejected || 0;
    if (!n) return 'by the store budget';
    return 'by the store budget · ' + fmt.num(n) + (n === 1 ? ' payload refused' : ' payloads refused') + (intake.lastReject ? ', last: ' + intake.lastReject : '');
  }

  function kpis(snap) {
    const self = (snap && snap.inspector) || {};
    const intake = (snap && snap.intake) || {};
    const sampler = (snap && snap.sampler) || {};
    const minutes = isNum(intake.window) ? Math.round(intake.window / 60) : 5;
    const lost = (intake.evicted || 0) + (intake.dropped || 0);
    return [
      { key: 'cpu', label: 'Inspector CPU', value: isNum(self.cpuPct) ? self.cpuPct.toFixed(1) + '%' : '–', sub: isNum(self.cpuPct) ? 'of one core' : self.error ? 'stats unavailable' : 'measuring' },
      { key: 'mem', label: 'Inspector memory', value: dash(fmt.bytes(self.memBytes)), sub: isNum(self.memLimit) && self.memLimit > 0 ? 'of ' + fmt.bytes(self.memLimit) + ' (' + (self.memPct || 0).toFixed(1) + '%)' : '' },
      { key: 'traces', label: 'Traced requests', value: fmt.num(intake.traces || 0), sub: tracedSub(intake, minutes) },
      { key: 'spans', label: 'Spans per request', value: intake.traces ? fmt.num(Math.round(intake.avgSpans)) : '–', sub: 'on average' },
      { key: 'payload', label: 'Payload per request', value: intake.traces ? dash(fmt.bytes(intake.avgBytes)) : '–', sub: intake.profiles ? '+ ' + fmt.bytes(intake.avgProfileBytes) + ' profile' : 'no profiles' },
      { key: 'intake', label: 'Intake', value: mbPerMin(intake.mbPerMin), sub: 'traces and profiles' },
      { key: 'dropped', label: 'Dropped', value: fmt.num(lost), sub: droppedSub(intake) },
      { key: 'execs', label: 'Exec calls', value: isNum(sampler.execsPerMin) ? Math.round(sampler.execsPerMin) + '/min' : '–', sub: 'into service containers' },
    ];
  }

  function tickRows(ticks) {
    return (ticks || [])
      .filter((t) => t.ticks > 0)
      .map((t) => ({
        key: t.kind,
        label: SERVICE_LABELS[t.kind] || t.kind,
        value: t.avgMs,
        full: (SERVICE_LABELS[t.kind] || t.kind) + ' (slowest ' + fmt.millis(t.maxMs) + ' of ' + t.ticks + ' samples)',
        flag: t.errors ? t.errors + ' failed' : '',
      }));
  }

  function intakeSeries(intake) {
    const mb = (intake && intake.series && intake.series.mb) || [];
    return { series: { bytes: mb.map((p) => [p[0], p[1] * MB]) } };
  }

  const HOLDERS = {
    api: 'a page through /api/v1 (any site open in this browser)',
    'api lease': 'api lease: a /api/v1/page or /api/v1/requests call in the last 3 min',
    drawer: 'the live drawer',
    overview: 'the Overview',
    timeline: 'the Overview timeline',
  };

  function liveRows(live) {
    return (live || []).map((l) => ({
      key: l.key,
      label: l.key,
      holders: (l.holders || []).map((h) => HOLDERS[h] || h).join(', '),
      stopLabel: 'Stop ' + l.key,
    }));
  }

  function loadText(load) {
    if (!load || load.error || !isNum(load.one)) return '–';
    return load.one.toFixed(2) + ' · ' + load.five.toFixed(2) + ' · ' + load.fifteen.toFixed(2) + ' on ' + load.cpus + ' CPUs';
  }

  function hasLoad(l) {
    return !!(l && !l.error && isNum(l.one));
  }

  // The live load says nothing about a run that ended minutes ago, so a
  // finished run shows the load it measured under.
  function runLoadText(b) {
    if (!b || b.running || !b.phase || !hasLoad(b.loadBefore)) return '';
    const after = hasLoad(b.loadAfter) ? ' → ' + b.loadAfter.one.toFixed(2) : '';
    return 'Docker VM load ' + b.loadBefore.one.toFixed(2) + after + ' on ' + b.loadBefore.cpus + ' CPUs during the run';
  }

  // A judgement call: above 0.7 runnable tasks per CPU the FPM workers wait
  // for a core, which inflates every setting alike.
  const BUSY_LOAD = 0.7;

  function busyLoad(b) {
    let worst = null;
    for (const l of [b.loadBefore, b.loadAfter]) {
      if (!hasLoad(l) || !(l.cpus > 0)) continue;
      if (l.one / l.cpus > BUSY_LOAD && (!worst || l.one > worst.one)) worst = l;
    }
    return worst;
  }

  // ---- benchmark ----------------------------------------------------------

  function deltaText(d) {
    if (!isNum(d)) return '';
    const r = Math.round(d);
    return (r < 0 ? '−' + Math.abs(r) : '+' + r) + '%';
  }

  function benchRows(b) {
    return ((b && b.arms) || []).map((a) => ({
      key: a.id,
      label: a.label || ARM_LABELS[a.id] || a.id,
      samples: a.samples || [],
      min: a.minMs,
      max: a.maxMs,
      median: a.medianMs,
      note: a.skipped ? 'not measured' : a.id === 'off' ? '' : deltaText(a.deltaPct),
      skipped: a.skipped || '',
    }));
  }

  function benchProgress(b) {
    if (!b || !b.phase) return { pct: 0, text: '' };
    const pct = b.total ? Math.round(((b.phase === 'done' ? b.total : b.done || 0) / b.total) * 100) : 0;
    const where = 'Round ' + b.round + ' of ' + b.rounds + ' · ' + (ARM_LABELS[b.arm] || b.arm);
    let text = '';
    switch (b.phase) {
      case 'probing':
        text = 'Checking that the page answers 200';
        break;
      case 'switching':
        text = where + ' · switching and reloading FPM';
        break;
      case 'warming':
        text = where + ' · warming up (' + (b.warmups || 2) + ' requests)';
        break;
      case 'measuring':
        text = where + ' · measuring';
        break;
      default:
        text = '';
    }
    return { pct, text };
  }

  // Each setting does more work than the one before it, so a cheaper result
  // further down the list, beyond a few points, is noise and not a finding.
  const COST_ORDER = ['off', 'lean', 'full', 'full+profile'];
  const ORDER_SLACK = 5;

  function outOfOrder(arms) {
    const delta = {};
    for (const a of arms) delta[a.id] = a.id === 'off' ? 0 : a.deltaPct;
    let prev = null;
    for (const id of COST_ORDER) {
      const d = delta[id];
      if (!isNum(d)) continue;
      if (prev !== null && d < prev - ORDER_SLACK) return true;
      prev = Math.max(prev === null ? d : prev, d);
    }
    return false;
  }

  function benchVerdict(b) {
    if (!b || !b.phase || b.running) return '';
    const restore = b.restored ? 'Your previous settings are back.' : 'Inspector could not put back your previous settings: ' + (b.restoreError || 'unknown error') + '. Set them on this page or run roll env restart php-fpm.';
    if (b.phase === 'cancelled') return 'Cancelled. ' + restore;
    if (b.phase === 'failed') return 'The benchmark stopped: ' + (b.error || 'unknown error') + '. ' + restore;
    const arms = (b.arms || []).filter((a) => (a.samples || []).length);
    const off = arms.find((a) => a.id === 'off');
    const top = arms.find((a) => a.id === 'full+profile') || arms.find((a) => a.id === 'full');
    const lean = arms.find((a) => a.id === 'lean');
    const skipped = (b.arms || [])
      .filter((a) => a.skipped)
      .map((a) => ' ' + (a.label || ARM_LABELS[a.id] || a.id) + ' was not measured: ' + a.skipped + '.')
      .join('');
    if (!off || !top || !isNum(top.deltaPct)) return (skipped + ' ' + restore).trim();
    const d = Math.round(top.deltaPct);
    let text = ARM_PHRASE[top.id] + ' makes this page ' + Math.abs(d) + '% ' + (d < 0 ? 'faster' : 'slower') + ' (' + fmt.millis(off.medianMs) + ' → ' + fmt.millis(top.medianMs) + ' median)';
    if (lean && isNum(lean.deltaPct)) {
      const l = Math.round(lean.deltaPct);
      text += l < 0 ? '; lean came out ' + -l + '% faster than off' : '; lean costs ' + l + '%';
    }
    text += '.';
    text += skipped;
    if (outOfOrder(arms)) {
      text += ' The settings did not come out in cost order, so differences this size are noise from other load: run it again when the machine is quieter.';
    }
    const busy = busyLoad(b);
    if (busy) {
      text += ' The machine was busy during the run (load ' + busy.one.toFixed(2) + ' on ' + busy.cpus + ' CPUs), which slows every setting alike: run it again when it is quieter.';
    }
    return text + ' ' + restore;
  }

  function benchURL(query, fallback) {
    return (query && query.url) || fallback || '/';
  }

  function pill(summary) {
    if (!summary) return null;
    return { label: summary.busy ? 'Measuring overhead…' : summary.label, on: summary.tracing ? 'true' : 'false', busy: summary.busy ? 'true' : 'false' };
  }

  // ---- Alpine ---------------------------------------------------------------

  function tableList(query) {
    return String((query && query.table) || '')
      .split(',')
      .filter(Boolean);
  }

  function withTable(query, id, on) {
    const list = tableList(query).filter((x) => x !== id);
    if (on) list.push(id);
    return list.join(',');
  }

  function register(Alpine, I) {
    const store = () => Alpine.store('app');

    function setQuery(patch) {
      const r = store().route;
      const query = Object.assign({}, r.query, patch);
      for (const k of Object.keys(query)) if (!query[k]) delete query[k];
      const h = route.format({ tab: r.tab, section: r.section, rest: r.rest, query });
      window.history.replaceState(null, '', h);
      store().route = route.parse(h);
    }

    const CHARTS = [
      { id: 'ticks', ref: 'chartTicks', make: (el, o) => C.bars(el, Object.assign({ title: 'How long does each metrics sample take?', subtitle: 'Average time per sample over 5 min; hover for the slowest', unit: 'ms', valueLabel: 'average', emptyText: 'No samples yet: the sampler is paused or just started' }, o)), data: (s) => tickRows(s.sampler && s.sampler.ticks) },
      { id: 'intake', ref: 'chartIntake', make: (el, o) => C.timeseries(el, Object.assign({ title: 'How much trace data arrives?', subtitle: 'Traces and profiles per minute, last 15 min', unit: 'bytes', window: '15m', series: [{ key: 'bytes', label: 'per minute', slot: 1 }], emptyText: 'No trace data in the last 15 min' }, o)), data: (s) => intakeSeries(s.intake) },
      { id: 'bench', ref: 'chartBench', make: (el, o) => C.ranges(el, Object.assign({ title: 'How much slower does tracing make this page?', subtitle: 'Wall time per request. Small dots are rounds, the large dot is the median; the band runs from the fastest round to the slowest', unit: 'ms', emptyText: 'Run the benchmark to fill this' }, o)), data: (s, b) => benchRows(b) },
    ];

    Alpine.data('performance', () => {
      const made = {};
      let timer = null;
      let fx = null;
      let onVis = null;
      let unsub = null;
      let seq = 0;
      let destroyed = false;

      return {
        snap: null,
        bench: null,
        failed: '',
        pending: '',
        benchUrl: '',
        urlTouched: false,
        starting: false,
        version: 0,

        init() {
          store().item = '';
          fx = Alpine.effect(() => {
            tableList(store().route.query);
            void this.version;
            this.$nextTick(() => this.paint());
          });
          this.load();
          timer = setInterval(() => {
            if (!document.hidden) this.load();
          }, POLL_MS);
          onVis = () => {
            if (!document.hidden) this.load();
          };
          document.addEventListener('visibilitychange', onVis);
        },

        destroy() {
          destroyed = true;
          if (fx) Alpine.release(fx);
          if (timer) clearInterval(timer);
          if (onVis) document.removeEventListener('visibilitychange', onVis);
          this.unfollow();
          for (const k of Object.keys(made)) made[k].destroy();
        },

        async load() {
          if (destroyed) return;
          const s = ++seq;
          try {
            const snap = await I.client.get('/api/performance', { quiet: true });
            if (s !== seq || destroyed) return;
            this.take(snap);
            this.failed = '';
          } catch (e) {
            if (s !== seq || destroyed) return;
            this.failed = e.message;
          }
        },

        take(snap) {
          this.snap = snap;
          this.applyBench(snap.benchmark);
          if (!this.urlTouched) this.benchUrl = benchURL(store().route.query, snap.defaultUrl);
          this.version++;
        },

        applyBench(b) {
          const was = this.bench && this.bench.running;
          this.bench = b || null;
          if (b && b.running) this.follow();
          else this.unfollow();
          if (was && b && !b.running) {
            this.load();
            I.loadSession(false).catch(() => {});
          }
          this.version++;
        },

        // Progress comes over the stream while a run lasts; the subscription
        // ends with it, so an idle page holds no stream.
        follow() {
          if (unsub || destroyed) return;
          unsub = I.stream.subscribe(['perf:benchmark'], (ev) => {
            if (ev.type === 'benchmark' && ev.data && !destroyed) this.applyBench(ev.data);
          });
        },

        unfollow() {
          if (unsub) unsub();
          unsub = null;
        },

        paint() {
          if (destroyed || !this.snap) return;
          const tables = tableList(store().route.query);
          for (const c of CHARTS) {
            const host = this.$refs[c.ref];
            if (!host) continue;
            if (!made[c.id]) {
              made[c.id] = c.make(host, { table: tables.includes(c.id), onTableToggle: (next) => setQuery({ table: withTable(store().route.query, c.id, next) }) });
            }
            made[c.id].update(c.data(this.snap, this.bench), { table: tables.includes(c.id) });
          }
        },

        // ---- switches ----

        get busy() {
          return !!(this.bench && this.bench.running);
        },
        get rows() {
          return switchRows(this.snap ? this.snap.php : [], { busy: this.busy, pending: this.pending });
        },
        get samples() {
          return sampleOptions(this.snap && this.snap.sampler);
        },
        get sampleNote() {
          return sampleNote(this.snap && this.snap.sampler);
        },
        get sampleDiffers() {
          return sampleDiffers(this.snap && this.snap.sampler);
        },
        get samplePermanent() {
          return this.snap ? this.snap.sampler.permanent : '';
        },
        get samplePending() {
          return this.pending === 'sample';
        },

        async change(key, body, done) {
          if (this.pending) return;
          this.pending = key;
          try {
            const snap = await I.client.post('/api/performance/settings', body);
            if (destroyed) return;
            this.take(snap);
            I.toast(done, 'ok');
            I.loadSession(false).catch(() => {});
          } catch (e) {
            this.load();
          } finally {
            this.pending = '';
          }
        },

        toggleTracing(row) {
          if (row.tracing.disabled) return;
          const on = row.tracing.checked !== 'true';
          this.change(row.service, { service: row.service, tracing: on }, 'Tracing ' + (on ? 'on' : 'off') + ' in ' + row.service + '. New requests follow it.');
        },
        setDetail(row, opt) {
          if (opt.disabled || opt.pressed === 'true') return;
          this.change(row.service, { service: row.service, detail: opt.id }, 'Trace detail ' + opt.id + ' in ' + row.service + '.');
        },
        toggleProfiling(row) {
          if (row.profiling.disabled) return;
          const on = row.profiling.checked !== 'true';
          this.change(row.service, { service: row.service, profiling: on }, 'Profiling ' + (on ? 'on' : 'off') + ' in ' + row.service + '.');
        },
        setSample(opt) {
          if (opt.pressed === 'true') return;
          this.change('sample', { sample: opt.id }, opt.id === 'off' ? 'Metrics sampling paused; the history stays.' : 'Metrics sampled every ' + opt.id + ' s.');
        },

        copy(text) {
          if (!text) return;
          if (navigator.clipboard) {
            navigator.clipboard.writeText(text).then(
              () => I.toast('Copied', 'ok'),
              () => I.toast('The browser refused the clipboard', 'warn'),
            );
          }
        },

        // ---- cost ----

        get kpis() {
          return kpis(this.snap);
        },
        get live() {
          return liveRows(this.snap && this.snap.live);
        },
        get hasLive() {
          return this.live.length > 0;
        },
        get stopping() {
          return this.pending === 'live';
        },

        async stopLive(row) {
          if (this.pending) return;
          this.pending = 'live';
          try {
            const snap = await I.client.post('/api/performance/live/stop', { key: row.key });
            if (destroyed) return;
            this.take(snap);
            I.toast(row.key + ' stopped. A page using /api/v1 cannot start it again for 10 minutes.', 'ok');
          } catch (e) {
            this.load();
          } finally {
            this.pending = '';
          }
        },
        get hasFailed() {
          return !!this.failed;
        },
        get ready() {
          return !!this.snap;
        },

        // ---- benchmark ----

        get running() {
          return this.busy;
        },
        get progress() {
          return benchProgress(this.bench);
        },
        progressStyle() {
          return { width: this.progress.pct + '%' };
        },
        get progressText() {
          return this.progress.text;
        },
        get progressLabel() {
          return this.progress.pct + '% done';
        },
        get verdict() {
          return benchVerdict(this.bench);
        },
        get hasVerdict() {
          return !!this.verdict;
        },
        get verdictCls() {
          const b = this.bench;
          if (!b) return '';
          if (!b.restored || b.phase === 'failed') return 'tone-warn';
          return '';
        },
        get loadText() {
          return loadText(this.snap && this.snap.load);
        },
        get runLoadText() {
          return runLoadText(this.bench);
        },
        get showLiveLoad() {
          return !this.runLoadText;
        },
        get benchMeta() {
          const b = this.bench;
          if (!b || !b.url) return '';
          return b.url + ' · ' + b.rounds + ' rounds; per setting ' + b.warmups + ' warm-ups, then the median of ' + (b.perRound || 1) + ' requests';
        },
        get startLabel() {
          return this.starting ? 'Starting…' : this.running ? 'Running…' : 'Measure the overhead';
        },
        get startDisabled() {
          return this.starting || this.running || !this.benchUrl;
        },

        onUrl() {
          this.urlTouched = true;
          setQuery({ url: this.benchUrl === (this.snap && this.snap.defaultUrl) ? '' : this.benchUrl });
        },

        async startBench() {
          if (this.startDisabled) return;
          this.starting = true;
          try {
            const st = await I.client.post('/api/performance/benchmark', { url: this.benchUrl });
            if (!destroyed) this.applyBench(st);
            I.loadSession(false).catch(() => {});
          } catch (e) {
            this.load();
          } finally {
            this.starting = false;
          }
        },

        async cancelBench() {
          try {
            const st = await I.client.post('/api/performance/benchmark/cancel', {});
            if (!destroyed) this.applyBench(st);
          } catch (e) {
            this.load();
          }
        },
      };
    });
  }

  return {
    switchRows,
    sampleOptions,
    sampleNote,
    sampleDiffers,
    kpis,
    tickRows,
    intakeSeries,
    liveRows,
    loadText,
    runLoadText,
    benchRows,
    benchProgress,
    benchVerdict,
    benchURL,
    pill,
    tableList,
    withTable,
    register,
  };
});
