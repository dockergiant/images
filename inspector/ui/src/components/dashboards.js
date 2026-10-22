(function (root, factory) {
  const isNode = typeof module === 'object' && module.exports;
  const deps = isNode ? { fmt: require('../lib/format.js'), route: require('../lib/route.js'), charts: require('../lib/charts.js'), overview: require('./overview.js') } : root.Inspector;
  const mod = factory(deps);
  if (isNode) {
    module.exports = mod;
  } else {
    root.Inspector.dashboards = mod;
    document.addEventListener('alpine:init', () => mod.register(window.Alpine, root.Inspector));
  }
})(this, function (deps) {
  const fmt = deps.fmt;
  const route = deps.route;
  const C = deps.charts;

  const POLL_MS = 10000;
  const WINDOWS = ['5m', '15m', '60m'];
  const DEFAULT_WINDOW = '15m';
  const WINDOW_LABEL = { '5m': '5 min', '15m': '15 min', '60m': '60 min' };
  const TILE_WINDOW = '15m';
  const START_SPAN = 60;
  const WARN_REASON = /cookie|no-cache|private|no-store/i;
  const RANKED = 8;

  function isNum(n) {
    return typeof n === 'number' && Number.isFinite(n);
  }

  // ---- hash state ---------------------------------------------------------

  function windowOf(query) {
    const w = query && query.win;
    return WINDOWS.includes(w) ? w : DEFAULT_WINDOW;
  }

  function tableList(query) {
    return String((query && query.table) || '')
      .split(',')
      .filter(Boolean);
  }

  // The value of ?table= after one chart switches its table view on or off.
  function withTable(query, id, on) {
    const list = tableList(query).filter((x) => x !== id);
    if (on) list.push(id);
    return list.join(',');
  }

  function windowButtons(query) {
    const cur = windowOf(query);
    return WINDOWS.map((w) => ({ id: w, label: WINDOW_LABEL[w], pressed: w === cur ? 'true' : 'false' }));
  }

  // ---- reading a metrics payload -----------------------------------------

  function seriesMap(m) {
    return (m && m.series) || {};
  }

  function endOf(m) {
    let end = -Infinity;
    const map = seriesMap(m);
    for (const k in map) for (const p of map[k] || []) if (isNum(p[0]) && p[0] > end) end = p[0];
    return isNum(end) ? end : undefined;
  }

  function lastValue(points) {
    for (let i = (points || []).length - 1; i >= 0; i--) if (isNum(points[i][1])) return points[i][1];
    return null;
  }

  function firstValue(points) {
    for (const p of points || []) if (isNum(p[1])) return p[1];
    return null;
  }

  // A ratio over the first minute the window holds, from the per-second rate
  // series, so a donut can say how the window moved since it began.
  function startRatio(m, num, den) {
    const map = seriesMap(m);
    const ref = map[den[0]] || [];
    const first = ref.length ? ref[0][0] : null;
    if (!isNum(first)) return null;
    let n = 0;
    let d = 0;
    const sum = (keys) => {
      let s = 0;
      for (const k of keys) for (const p of map[k] || []) if (isNum(p[0]) && p[0] < first + START_SPAN && isNum(p[1])) s += p[1];
      return s;
    };
    n = sum(num);
    d = sum(den);
    return d > 0 ? n / d : null;
  }

  function ratioDonut(m, key, num, den) {
    const totals = (m && m.totals) || {};
    const latest = (m && m.latest) || {};
    const life = latest[key];
    return {
      value: isNum(totals[key]) ? totals[key] : null,
      start: startRatio(m, num, den),
      startLabel: 'at the window start',
      sub: fraction(life) ? 'Lifetime ' + fmt.pct(life, 1) : '',
    };
  }

  // One Varnish hit ratio everywhere: hits / (hits + misses + passes). The
  // server's hit_ratio series divides the same way; synth and pipe stay out.
  const VARNISH_HIT = { num: ['hit'], den: ['hit', 'miss', 'pass'] };

  function countText(n, one, many) {
    return fmt.num(n) + ' ' + (n === 1 ? one : many);
  }

  // A lifetime hit ratio outside 0..1 comes from a server restart while
  // Inspector's own lookups are subtracted; it says nothing, so it is left out.
  function fraction(v) {
    return isNum(v) && v >= 0 && v <= 1;
  }

  function lifetimeText(m, key) {
    const life = m.latest && m.latest[key];
    return fraction(life) ? 'lifetime ' + fmt.pct(life, 1) : '';
  }

  // The Health cards for the picked window, from the same payload as the
  // charts. Each names the server card it stands in for by label prefix.
  function varnishHitCard(m, win) {
    if (collecting(m)) return null;
    const t = m.totals || {};
    const lifetime = lifetimeText(m, 'hit_ratio');
    const label = 'Hit rate, last ' + WINDOW_LABEL[win];
    if (!isNum(t.hit_ratio)) {
      return { label, match: 'hit rate', value: '–', sub: ['no requests in the last ' + WINDOW_LABEL[win], lifetime].filter(Boolean).join(' · '), cls: '' };
    }
    const counts = [countText(t.hit || 0, 'hit', 'hits'), countText(t.miss || 0, 'miss', 'misses'), countText(t.pass || 0, 'pass', 'passes') + ' (not cacheable)'];
    return { label, match: 'hit rate', value: fmt.pct(t.hit_ratio, 1), sub: counts.concat(lifetime ? [lifetime] : []).join(' · '), cls: '' };
  }

  // Keyspace hits / (hits + misses) with Inspector's own lookups left out,
  // the same division as the hit_ratio series and the Go card.
  function redisHitCard(m, win) {
    if (collecting(m)) return null;
    const t = m.totals || {};
    const lifetime = lifetimeText(m, 'hit_ratio');
    const label = 'Hit ratio, last ' + WINDOW_LABEL[win];
    if (!isNum(t.hit_ratio)) {
      return { label, match: 'hit ratio', value: '–', sub: ['no key lookups in the last ' + WINDOW_LABEL[win], lifetime].filter(Boolean).join(' · '), cls: '' };
    }
    const counts = [countText(t.hits || 0, 'hit', 'hits'), countText(t.misses || 0, 'miss', 'misses')];
    return { label, match: 'hit ratio', value: fmt.pct(t.hit_ratio, 1), sub: counts.concat(lifetime ? [lifetime] : []).join(' · '), cls: t.hit_ratio < 0.5 ? 'tone-warn' : '' };
  }

  // The seconds a counter's rate points cover. The first point already spans
  // one step back to the sample before it, so the step is added once.
  function coveredSeconds(m, key) {
    const p = seriesMap(m)[key] || [];
    if (!p.length) return 0;
    return p[p.length - 1][0] - p[0][0] + (m.step || 10);
  }

  function perSecond(total, secs) {
    return (secs > 0 ? total / secs : 0).toFixed(1) + '/s';
  }

  // The flow chart's own average, not the broker's last two samples.
  function rabbitPublishCard(m, win) {
    if (collecting(m)) return null;
    const t = m.totals || {};
    const label = 'Publish rate, last ' + WINDOW_LABEL[win];
    if (!isNum(t.publish)) return { label, match: 'publish rate', value: '–', sub: 'the broker reports no message stats yet', cls: '' };
    if (t.publish === 0) return { label, match: 'publish rate', value: '0.0/s', sub: 'nothing published in the last ' + WINDOW_LABEL[win], cls: '' };
    const sub = [fmt.num(t.publish) + ' published', isNum(t.deliver) ? fmt.num(t.deliver) + ' delivered' : '', isNum(t.ack) ? fmt.num(t.ack) + ' acknowledged' : ''].filter(Boolean).join(' · ');
    return { label, match: 'publish rate', value: perSecond(t.publish, coveredSeconds(m, 'publish')), sub, cls: '' };
  }

  // Queries per second and the average query time, as the two query charts.
  function searchQueriesCard(m, win) {
    if (collecting(m)) return null;
    const t = m.totals || {};
    const label = 'Searches, last ' + WINDOW_LABEL[win];
    if (!isNum(t.queries) || t.queries === 0) return { label, match: 'searches', value: '0.0/s', sub: 'no queries in the last ' + WINDOW_LABEL[win], cls: '' };
    const sub = countText(t.queries, 'query', 'queries') + (isNum(t.query_ms) ? ' · avg ' + C.duration(t.query_ms) + ' per query' : '');
    return { label, match: 'searches', value: perSecond(t.queries, coveredSeconds(m, 'queries')), sub, cls: '' };
  }

  function collecting(m) {
    return !m || !(m.samples >= 2);
  }

  // Empty text for a ratio donut: collecting first, then "nothing counted".
  function ratioEmpty(m, key, what) {
    if (collecting(m)) return C.EMPTY_TEXT;
    const life = m.latest && m.latest[key];
    return 'No ' + what + ' in this window.' + (fraction(life) ? ' Lifetime: ' + fmt.pct(life, 1) + '.' : '');
  }

  function fillDonut(used, total, fmtValue, what) {
    if (!isNum(used) || !isNum(total) || total <= 0) return { value: null };
    return { value: Math.min(1, used / total), sub: fmtValue(used) + ' ' + what + ' of ' + fmtValue(total) };
  }

  // One row per member of a "<prefix><name>" group, from latest or totals.
  function groupRows(values, prefix, labelOf) {
    const out = [];
    for (const k of Object.keys(values || {})) {
      if (!k.startsWith(prefix) || !isNum(values[k])) continue;
      const name = k.slice(prefix.length);
      const other = name === 'other';
      out.push({ key: name, label: labelOf ? labelOf(name, other) : name, value: values[k], other });
    }
    return out;
  }

  // The server already folds its tail into "<prefix>other"; ranking that row
  // with the rest would show two Other rows once the kit folds its own tail.
  // So the top RANKED - 1 members stay and everything else, the server's
  // other included, becomes one grey row at the end.
  function rankRows(rows, otherLabel) {
    const named = rows.filter((r) => !r.other).sort((a, b) => b.value - a.value);
    const tail = named.slice(RANKED - 1).concat(rows.filter((r) => r.other));
    const head = named.slice(0, RANKED - 1);
    const rest = tail.reduce((sum, r) => sum + r.value, 0);
    if (rest > 0) head.push({ key: '__other', label: otherLabel, value: rest, other: true });
    return head;
  }

  function commandLabel(name, other) {
    if (other) return 'Other commands';
    return String(name).toUpperCase().replace(/\|/g, ' ');
  }

  // Calls per command over the window. The ranking comes from the server.
  function commandRows(m) {
    return rankRows(groupRows(m && m.totals, 'calls.', commandLabel).filter((r) => r.value > 0), 'Other commands');
  }

  function dbNames(types) {
    const names = {};
    for (const t of (types && types.types) || []) {
      const db = String(t.db);
      (names[db] = names[db] || []).push(t.label || t.name);
    }
    return names;
  }

  function dbRows(m, types) {
    const names = dbNames(types);
    return groupRows(m && m.latest, 'keys.db', (n) => 'db' + n + (names[n] ? ' · ' + names[n].join(', ') : ''))
      .filter((r) => !r.other)
      .map((r) => Object.assign(r, { key: 'db' + r.key }));
  }

  function queueRows(m) {
    const latest = (m && m.latest) || {};
    const names = [];
    for (const k of Object.keys(latest)) {
      const hit = /^(ready|unacked)\.(.+)$/.exec(k);
      if (hit && !names.includes(hit[2])) names.push(hit[2]);
    }
    const out = [];
    for (const q of names) {
      const ready = isNum(latest['ready.' + q]) ? latest['ready.' + q] : 0;
      const unacked = isNum(latest['unacked.' + q]) ? latest['unacked.' + q] : 0;
      if (ready + unacked <= 0) continue;
      const other = q === 'other';
      const consumers = latest['consumers.' + q];
      out.push({
        key: q,
        label: other ? 'Other queues' : q,
        values: { ready, unacked },
        flag: !other && ready > 0 && consumers === 0 ? 'no consumer' : '',
      });
    }
    return out;
  }

  function consumerRows(m) {
    return rankRows(groupRows(m && m.latest, 'consumers.', (n, other) => (other ? 'Other queues' : n)).filter((r) => r.value > 0), 'Other queues');
  }

  function seconds(ms) {
    return ms / 1000;
  }

  function bucketLabel(b) {
    const edge = (v) => (v < 1000 ? String(v) : seconds(v) + ' s');
    if (b.to === null || b.to === undefined) return '>' + edge(b.from);
    if (!b.from) return '≤' + edge(b.to);
    return edge(b.to);
  }

  function bucketFull(b) {
    if (b.to === null || b.to === undefined) return 'over ' + (b.from < 1000 ? b.from + ' ms' : seconds(b.from) + ' s');
    if (!b.from) return (b.to < 1000 ? b.to + ' ms' : seconds(b.to) + ' s') + ' or less';
    if (b.to < 1000) return b.from + ' to ' + b.to + ' ms';
    if (b.from >= 1000) return seconds(b.from) + ' to ' + seconds(b.to) + ' s';
    return b.from + ' ms to ' + seconds(b.to) + ' s';
  }

  function histogramRows(m) {
    return ((m && m.histogram) || []).map((b) => ({ label: bucketLabel(b), full: bucketFull(b), value: isNum(b.count) ? b.count : 0 }));
  }

  // Varnish counts misses from its log while Inspector watches, not per window.
  function missRows(health) {
    const raw = health && health.raw;
    return ((raw && raw.topMisses) || []).map((x, i) => ({
      key: 'm' + i,
      label: x.url,
      value: x.misses,
      full: x.reason ? x.url + ' · ' + x.reason : x.url,
      flag: WARN_REASON.test(x.reason || '') ? x.reason : '',
    }));
  }

  function missEmpty(health) {
    const raw = health && health.raw;
    if (!raw) return 'Reading the Varnish log…';
    if (!raw.seen) return 'Nothing counted yet. Misses are tallied from the Varnish log while this tab or the Overview is open; load a few pages.';
    return 'No misses among the last ' + fmt.num(raw.seen) + ' requests Inspector saw.';
  }

  function missSubtitle(health) {
    const raw = health && health.raw;
    return raw && raw.seen ? 'Misses among the last ' + fmt.num(raw.seen) + ' requests Inspector saw' : 'Counted from the Varnish log while Inspector watches';
  }

  function sumSeries(m, prefix) {
    const map = seriesMap(m);
    const acc = new Map();
    for (const k of Object.keys(map)) {
      if (!k.startsWith(prefix)) continue;
      for (const p of map[k] || []) if (isNum(p[0]) && isNum(p[1])) acc.set(p[0], (acc.get(p[0]) || 0) + p[1]);
    }
    return Array.from(acc.entries()).sort((a, b) => a[0] - b[0]);
  }

  // The newest PHP point is the minute still running; drawn as it is, every
  // chart would end in a drop. Minutes that began before the oldest trace in
  // the store are unknown or only partly known, so they go too. The totals
  // keep both.
  function completeMinutes(m) {
    if (!m || !m.series) return m;
    const since = isNum(m.since) ? m.since : -Infinity;
    const series = {};
    for (const k of Object.keys(m.series)) series[k] = (m.series[k] || []).slice(0, -1).filter((p) => p[0] >= since);
    return Object.assign({}, m, { series });
  }

  function msText(v) {
    return isNum(v) ? C.duration(v) : '–';
  }

  // ---- what each dashboard draws -----------------------------------------
  //
  // Every chart names its x-ref host, the kit form (or a function of the data
  // when the form depends on it), the options it is created with and how it
  // reads the payload. Timeseries read the payload as it is.

  const varnishCharts = [
    {
      id: 'hit',
      ref: 'chartHit',
      form: 'donut',
      opts: { title: 'How much is served from cache?', subtitle: 'Hit ratio over the window, lifetime underneath', label: 'from cache', goodWhen: 'up' },
      data: (m) => ratioDonut(m, 'hit_ratio', VARNISH_HIT.num, VARNISH_HIT.den),
      empty: (m) => ratioEmpty(m, 'hit_ratio', 'requests'),
    },
    {
      id: 'outcomes',
      ref: 'chartOutcomes',
      form: 'timeseries',
      opts: {
        title: 'What does Varnish do with each request?',
        subtitle: 'Requests per second by outcome',
        mode: 'stack',
        unit: 'rate',
        series: [
          { key: 'hit', label: 'hit', slot: 1 },
          { key: 'miss', label: 'miss', slot: 2 },
          { key: 'pass', label: 'pass', slot: 3 },
        ],
      },
    },
    {
      id: 'storage',
      ref: 'chartStorage',
      form: (m) => (m && m.latest && isNum(m.latest.g_space) ? 'donut' : 'timeseries'),
      opts: (m, form) =>
        form === 'donut'
          ? { title: 'How full is the cache storage?', subtitle: 'Memory storage (SMA s0) used against its size', label: 'used', goodWhen: 'down' }
          : { title: 'How much cache storage is used?', subtitle: 'This Varnish does not report free space, so no limit is drawn', unit: 'bytes', series: [{ key: 'g_bytes', label: 'used', slot: 1 }] },
      data: (m, ctx, form) => {
        if (form !== 'donut') return m;
        const l = m.latest || {};
        const used = l.g_bytes;
        const total = isNum(used) && isNum(l.g_space) ? used + l.g_space : null;
        const series = seriesMap(m);
        const u0 = firstValue(series.g_bytes);
        const f0 = firstValue(series.g_space);
        const d = fillDonut(used, total, fmt.bytes, 'used');
        // A storage delta under a tenth of a point is noise, not news.
        if (isNum(u0) && isNum(f0) && u0 + f0 > 0 && isNum(d.value) && Math.abs(u0 / (u0 + f0) - d.value) >= 0.001) {
          d.start = u0 / (u0 + f0);
          d.startLabel = 'at the window start';
        }
        return d;
      },
    },
    {
      id: 'backend',
      ref: 'chartBackend',
      form: 'timeseries',
      opts: {
        title: 'How often does Varnish go to the backend?',
        subtitle: 'Client requests against backend fetches, per second',
        unit: 'rate',
        series: [
          { key: 'client_req', label: 'client requests', slot: 1 },
          { key: 'backend_req', label: 'backend fetches', slot: 2 },
        ],
      },
    },
    {
      id: 'objects',
      ref: 'chartObjects',
      form: 'timeseries',
      opts: { title: 'How many objects are cached?', subtitle: 'Objects in cache', unit: 'count', series: [{ key: 'n_object', label: 'objects', slot: 1 }] },
    },
    {
      id: 'misses',
      ref: 'chartMisses',
      form: 'bars',
      opts: () => ({ title: 'Which pages miss most?', subtitle: 'Counted from the Varnish log, not limited to the window', unit: 'count', mono: true, nameLabel: 'URL', valueLabel: 'misses' }),
      data: (m, ctx) => missRows(ctx.health),
      empty: (m, ctx) => missEmpty(ctx.health),
    },
    {
      id: 'churn',
      ref: 'chartChurn',
      form: 'timeseries',
      opts: {
        title: 'Why do objects leave the cache?',
        subtitle: 'Expired and pushed out (LRU) per second',
        unit: 'rate',
        height: 120,
        series: [
          { key: 'n_expired', label: 'expired', slot: 1 },
          { key: 'n_lru_nuked', label: 'pushed out', slot: 2 },
        ],
      },
    },
    {
      id: 'bans',
      ref: 'chartBans',
      form: 'timeseries',
      opts: { title: 'How long is the ban list?', subtitle: 'Bans Varnish still tests cached objects against', unit: 'count', height: 120, series: [{ key: 'bans', label: 'bans', slot: 1 }] },
    },
  ];

  const redisCharts = [
    {
      id: 'hit',
      ref: 'chartHit',
      form: 'donut',
      opts: { title: 'How often does a lookup find its key?', subtitle: 'Keyspace hit ratio over the window, lifetime underneath', label: 'hits', goodWhen: 'up' },
      data: (m) => ratioDonut(m, 'hit_ratio', ['hits'], ['hits', 'misses']),
      empty: (m) => ratioEmpty(m, 'hit_ratio', 'key lookups'),
    },
    {
      id: 'ops',
      ref: 'chartOps',
      form: 'timeseries',
      opts: { title: 'How busy is the server?', subtitle: "Commands per second, Inspector's own left out", unit: 'rate', series: [{ key: 'ops', label: 'commands', slot: 1 }] },
    },
    {
      id: 'memory',
      ref: 'chartMemory',
      form: (m) => (m && m.latest && m.latest.maxmemory > 0 ? 'donut' : 'timeseries'),
      opts: (m, form) =>
        form === 'donut'
          ? { title: 'How close is memory to the limit?', subtitle: 'Used memory against maxmemory', label: 'used', goodWhen: 'down' }
          : { title: 'How much memory is used?', subtitle: 'No maxmemory limit is set, so nothing is evicted for space', unit: 'bytes', series: [{ key: 'used_memory', label: 'used', slot: 1 }] },
      data: (m, ctx, form) => {
        if (form !== 'donut') return m;
        const l = m.latest || {};
        return fillDonut(l.used_memory, l.maxmemory, fmt.bytes, 'used');
      },
    },
    {
      id: 'churn',
      ref: 'chartChurn',
      form: 'timeseries',
      opts: {
        title: 'Are keys being evicted?',
        subtitle: 'Evicted and expired keys per second',
        unit: 'rate',
        series: [
          { key: 'evicted', label: 'evicted', slot: 1 },
          { key: 'expired', label: 'expired', slot: 2 },
        ],
      },
    },
    {
      id: 'dbs',
      ref: 'chartDbs',
      form: 'bars',
      opts: { title: 'Which cache holds the keys?', subtitle: 'Keys per database, named by the Magento cache that uses it', unit: 'count', nameLabel: 'Database', valueLabel: 'keys' },
      data: (m, ctx) => dbRows(m, ctx.extra),
      empty: (m) => (collecting(m) ? C.EMPTY_TEXT : 'Every database is empty.'),
    },
    {
      id: 'commands',
      ref: 'chartCommands',
      form: 'bars',
      opts: { title: 'Which commands run most?', subtitle: "Calls in the window, Inspector's own included", unit: 'count', mono: true, nameLabel: 'Command', valueLabel: 'calls', sort: false },
      data: (m) => commandRows(m),
      empty: (m) => (collecting(m) ? C.EMPTY_TEXT : 'No commands ran in this window.'),
    },
  ];

  const searchCharts = [
    {
      id: 'heap',
      ref: 'chartHeap',
      form: 'donut',
      opts: { title: 'How full is the JVM heap?', subtitle: 'Heap used on the fullest node', label: 'heap used', goodWhen: 'down' },
      data: (m) => {
        const series = seriesMap(m);
        const now = lastValue(series.heap_used_pct);
        const first = firstValue(series.heap_used_pct);
        return { value: isNum(now) ? now / 100 : null, start: isNum(first) ? first / 100 : null, startLabel: 'at the window start' };
      },
    },
    {
      id: 'qcache',
      ref: 'chartQueryCache',
      form: 'donut',
      opts: { title: 'Does the query cache help?', subtitle: 'Query cache hit ratio over the window', label: 'hits', goodWhen: 'up' },
      data: (m) => ratioDonut(m, 'query_cache_hit_ratio', [], []),
      empty: (m) => ratioEmpty(m, 'query_cache_hit_ratio', 'query cache lookups'),
    },
    {
      id: 'rcache',
      ref: 'chartRequestCache',
      form: 'donut',
      opts: { title: 'Does the request cache help?', subtitle: 'Shard request cache hit ratio over the window', label: 'hits', goodWhen: 'up' },
      data: (m) => ratioDonut(m, 'request_cache_hit_ratio', [], []),
      empty: (m) => ratioEmpty(m, 'request_cache_hit_ratio', 'request cache lookups'),
    },
    {
      id: 'queries',
      ref: 'chartQueries',
      form: 'timeseries',
      opts: { title: 'How many searches run?', subtitle: 'Queries per second', unit: 'rate', series: [{ key: 'queries', label: 'queries', slot: 1 }] },
    },
    {
      id: 'latency',
      ref: 'chartLatency',
      form: 'timeseries',
      opts: { title: 'How long does a search take?', subtitle: 'Average query time per sample', unit: 'ms', series: [{ key: 'query_ms', label: 'average', slot: 1 }] },
    },
    {
      id: 'indexing',
      ref: 'chartIndexing',
      form: 'timeseries',
      opts: { title: 'Is anything being indexed?', subtitle: 'Documents indexed per second', unit: 'rate', series: [{ key: 'indexing', label: 'indexed', slot: 1 }] },
    },
    {
      id: 'gc',
      ref: 'chartGc',
      form: 'timeseries',
      opts: {
        title: 'How much time goes to garbage collection?',
        subtitle: 'JVM GC milliseconds per second',
        unit: 'ms',
        format: (v) => C.duration(v) + '/s',
        series: [{ key: 'gc_ms', label: 'GC', slot: 1 }],
      },
    },
    {
      id: 'docs',
      ref: 'chartDocs',
      form: 'bars',
      opts: { title: 'Which indices hold the most documents?', subtitle: 'Documents per index, system indices left out', unit: 'count', mono: true, nameLabel: 'Index', valueLabel: 'documents', sort: false },
      data: (m) => rankRows(groupRows(m && m.latest, 'docs.'), 'Other indices'),
      empty: (m) => (collecting(m) ? C.EMPTY_TEXT : 'No index holds documents yet. Run roll magento indexer:reindex.'),
    },
    {
      id: 'size',
      ref: 'chartSize',
      form: 'bars',
      opts: { title: 'Which indices take the most disk?', subtitle: 'Store size per index', unit: 'bytes', mono: true, nameLabel: 'Index', valueLabel: 'size', sort: false },
      data: (m) => rankRows(groupRows(m && m.latest, 'bytes.'), 'Other indices'),
      empty: (m) => (collecting(m) ? C.EMPTY_TEXT : 'No index stores anything yet.'),
    },
  ];

  const rabbitCharts = [
    {
      id: 'flow',
      ref: 'chartFlow',
      form: 'timeseries',
      opts: {
        title: 'Are messages flowing?',
        subtitle: 'Published, delivered and acknowledged per second',
        unit: 'rate',
        series: [
          { key: 'publish', label: 'published', slot: 1 },
          { key: 'deliver', label: 'delivered', slot: 2 },
          { key: 'ack', label: 'acknowledged', slot: 3 },
        ],
      },
    },
    {
      id: 'connections',
      ref: 'chartConnections',
      form: 'timeseries',
      opts: { title: 'Is anything connected?', subtitle: 'Open connections to the broker', unit: 'count', series: [{ key: 'connections', label: 'connections', slot: 1 }] },
    },
    {
      id: 'queues',
      ref: 'chartQueues',
      form: 'stackedBars',
      opts: {
        title: 'Which queues are backing up?',
        subtitle: 'Messages ready and unacknowledged right now',
        unit: 'count',
        mono: true,
        nameLabel: 'Queue',
        series: [
          { key: 'ready', label: 'ready', slot: 1 },
          { key: 'unacked', label: 'unacked', slot: 2 },
        ],
      },
      data: (m) => queueRows(m),
      empty: (m) => (collecting(m) ? C.EMPTY_TEXT : 'Every queue is empty.'),
    },
    {
      id: 'consumers',
      ref: 'chartConsumers',
      form: 'bars',
      opts: { title: 'Which queues have a consumer?', subtitle: 'Consumers attached per queue', unit: 'count', mono: true, nameLabel: 'Queue', valueLabel: 'consumers', sort: false },
      data: (m) => consumerRows(m),
      empty: (m) => (collecting(m) ? C.EMPTY_TEXT : 'No queue has a consumer attached. Magento starts them from cron (roll magento cron:install) or with roll magento queue:consumers:start <name>.'),
    },
  ];

  // The PHP strip sits above the bottleneck tables, so its plots are short.
  const STRIP_H = 96;

  // PHP points are whole minutes, so traffic in the current minute has no
  // point yet; the trace store is read directly, so there is no collecting.
  function phpEmpty(m) {
    if (!m) return C.EMPTY_TEXT;
    const t = m.totals || {};
    return t.requests > 0 ? 'The first point lands when this minute closes.' : 'No traced request in this window.';
  }

  const phpCharts = [
    {
      id: 'requests',
      ref: 'chartRequests',
      form: 'timeseries',
      opts: { title: 'How many requests reach PHP?', subtitle: 'Traced web requests per minute', unit: 'count', height: STRIP_H, format: (v) => C.compact(v) + '/min', series: [{ key: 'requests', label: 'requests', slot: 1 }] },
      empty: phpEmpty,
    },
    {
      id: 'latency',
      ref: 'chartLatency',
      form: 'timeseries',
      opts: {
        title: 'How slow are requests?',
        subtitle: 'p50 and p95 per minute',
        unit: 'ms',
        height: STRIP_H,
        series: [
          { key: 'p50', label: 'p50', slot: 1 },
          { key: 'p95', label: 'p95', slot: 2 },
        ],
      },
      empty: phpEmpty,
    },
    {
      id: 'histogram',
      ref: 'chartHistogram',
      form: 'histogram',
      opts: { title: 'How are response times spread?', subtitle: 'Requests per latency bucket in the window', unit: 'count', height: STRIP_H, valueLabel: 'requests', bucketLabel: 'Response time' },
      data: (m) => histogramRows(m),
      empty: (m) => (m ? 'No traced request in this window. Load a shop page Varnish does not cache.' : C.EMPTY_TEXT),
    },
    {
      id: 'sql',
      ref: 'chartSql',
      form: 'timeseries',
      opts: {
        title: 'How many queries does a request run?',
        subtitle: 'SQL queries per request, p50 and p95 per minute',
        unit: 'count',
        height: STRIP_H,
        series: [
          { key: 'sql_p50', label: 'p50', slot: 1 },
          { key: 'sql_p95', label: 'p95', slot: 2 },
        ],
      },
      empty: phpEmpty,
    },
  ];

  const SPECS = {
    varnish: { url: '/api/varnish/metrics', charts: varnishCharts, windowCard: varnishHitCard },
    redis: { url: '/api/redis/metrics', charts: redisCharts, windowCard: redisHitCard, extra: (I) => I.client.get('/api/redis/types', { quiet: true }) },
    search: { url: '/api/search/metrics', charts: searchCharts, windowCard: searchQueriesCard },
    rabbitmq: { url: '/api/rabbitmq/metrics', charts: rabbitCharts, windowCard: rabbitPublishCard },
    php: { url: '/api/traces/metrics', charts: phpCharts, prep: completeMinutes },
  };

  function formOf(spec, m, ctx) {
    return typeof spec.form === 'function' ? spec.form(m, ctx) : spec.form;
  }

  function optsOf(spec, m, form) {
    return typeof spec.opts === 'function' ? spec.opts(m, form) : spec.opts;
  }

  // The headline numbers on top of the PHP strip, for the whole window.
  function phpKpis(m, win) {
    const t = (m && m.totals) || {};
    const n = isNum(t.requests) ? t.requests : 0;
    return [
      { key: 'requests', label: 'Requests', value: m ? fmt.num(n) : '–', sub: 'traced in the last ' + WINDOW_LABEL[win] },
      { key: 'p50', label: 'p50', value: msText(t.p50), sub: 'half of them are faster' },
      { key: 'p95', label: 'p95', value: msText(t.p95), sub: '1 in 20 is slower' },
      { key: 'max', label: 'Slowest', value: msText(t.max), sub: 'in the window' },
      { key: 'sql', label: 'SQL per request', value: isNum(t.sql_p95) ? fmt.num(Math.round(t.sql_p95)) : '–', sub: isNum(t.sql_p50) ? 'p95 · p50 ' + fmt.num(Math.round(t.sql_p50)) : 'p95' },
    ].map((k, i, all) => Object.assign(k, { cls: all.length % 2 && i === all.length - 1 ? 'dash-kpi dash-kpi-tail' : 'dash-kpi' }));
  }

  function sampleNote(kind, m, err) {
    if (err && !m) return 'Metrics did not load: ' + err;
    if (!m) return 'Reading the history…';
    if (kind === 'php') {
      const n = m.samples || 0;
      return n ? fmt.num(n) + (n === 1 ? ' traced request' : ' traced requests') + ' in the window, one point per minute' : 'No traced web request in this window yet';
    }
    if (collecting(m)) return 'Collecting: Inspector samples every ' + (m.step || 10) + ' s';
    return 'One sample every ' + m.step + ' s · ' + fmt.num(m.samples) + ' in the window';
  }

  // ---- overview tiles ------------------------------------------------------

  const TILE_ORDER = ['varnish', 'php', 'redis', 'search', 'rabbitmq'];

  const TILE_CHART = {
    varnish: { form: 'donut', caption: 'served from cache, last 15 min' },
    redis: { form: 'sparkline', key: 'ops', unit: 'rate', caption: 'commands per second, last 15 min' },
    search: { form: 'sparkline', key: 'query_ms', unit: 'ms', caption: 'average query time, last 15 min' },
    rabbitmq: { form: 'sparkline', key: 'ready', unit: 'count', caption: 'messages waiting, last 15 min' },
    php: { form: 'sparkline', key: 'p95', unit: 'ms', caption: 'p95 per minute, last 15 min' },
  };

  function tileHref(kind) {
    return kind === 'php' ? route.format({ tab: 'traces' }) : route.format({ tab: kind, section: 'health' });
  }

  function phpHeadline(m) {
    if (!m) return { value: '…', sub: 'Reading the trace store', tone: '' };
    const t = m.totals || {};
    if (!t.requests) return { value: 'no requests yet', sub: 'Load a shop page that Varnish does not serve from cache', tone: '' };
    return { value: 'p95 ' + msText(t.p95), sub: fmt.num(t.requests) + (t.requests === 1 ? ' request' : ' requests') + ' in 15 min · p50 ' + msText(t.p50), tone: '' };
  }

  // The tile's hit ratio covers the same 15 minutes as its sparkline; until
  // the history holds two samples the Health card's own window stands in.
  function redisTileText(m, health, c) {
    if (collecting(m)) return { value: c.value, sub: c.sub };
    const cards = (health && health.cards) || [];
    const mem = cards.find((x) => x.label.toLowerCase().startsWith('memory'));
    const ev = cards.find((x) => x.label.toLowerCase().startsWith('evictions'));
    const r = m.totals && m.totals.hit_ratio;
    if (!isNum(r)) return { value: 'idle', sub: ['no key lookups in 15 min', mem ? mem.value : ''].filter(Boolean).join(' · ') };
    return { value: fmt.pct(r, 1) + ' hits', sub: ['last 15 min', mem ? mem.value : '', ev ? ev.value + ' evictions' : ''].filter(Boolean).join(' · ') };
  }

  function varnishTileText(health) {
    const cards = (health && health.cards) || [];
    const pages = cards.find((c) => c.label.toLowerCase().startsWith('cached pages'));
    const backend = cards.find((c) => c.label.toLowerCase().startsWith('backend'));
    return {
      value: pages ? pages.value + ' pages cached' : 'cache',
      sub: [backend ? 'backend ' + String(backend.value).toLowerCase() : '', pages ? pages.sub : ''].filter(Boolean).join(' · '),
    };
  }

  // One tile per service: the existing health headline, the service's chart
  // and a link to its dashboard. A service that is not in the env says so.
  function tiles(overview, metrics, labelOf) {
    const cards = deps.overview.serviceCards(overview, labelOf);
    const byKind = {};
    for (const c of cards) byKind[c.kind] = c;
    return TILE_ORDER.map((kind) => tileRow(kind, byKind[kind], overview, metrics, labelOf)).map((t) =>
      Object.assign(t, {
        linkText: t.present ? 'View dashboard' : 'How to add it',
        linkLabel: t.present ? 'View the ' + t.label + ' dashboard' : 'How to add ' + t.label,
      }),
    );
  }

  function tileRow(kind, c, overview, metrics, labelOf) {
    const spec = TILE_CHART[kind];
    if (kind === 'php') {
      const h = phpHeadline(metrics.php);
      return { kind, label: 'PHP traces', present: true, value: h.value, sub: h.sub, valueCls: '', href: tileHref(kind), cls: fmt.kindClass(kind), caption: spec.caption, hasChart: 'true', form: spec.form };
    }
    if (!c) return { kind, label: labelOf(kind), present: false, value: overview ? 'not in this env' : '…', sub: overview ? 'Switch it on in .env.roll to see it here' : 'Reading the services', valueCls: '', href: tileHref(kind), cls: fmt.kindClass(kind) + ' is-absent', caption: '', hasChart: 'false', form: spec.form };
    const adapter = ((overview && overview.adapters) || []).find((a) => a.kind === kind);
    let value = c.value;
    let sub = c.sub;
    if (kind === 'varnish' && c.present) {
      const t = varnishTileText(adapter && adapter.health);
      value = t.value;
      sub = t.sub;
    }
    if (kind === 'redis' && c.present) {
      const t = redisTileText(metrics.redis, adapter && adapter.health, c);
      value = t.value;
      sub = t.sub;
    }
    return {
      kind,
      label: c.label,
      present: c.present,
      value,
      sub,
      valueCls: c.valueCls,
      href: tileHref(kind),
      cls: c.cls + (c.present ? '' : ' is-absent'),
      caption: c.present ? spec.caption : '',
      hasChart: c.present ? 'true' : 'false',
      form: spec.form,
    };
  }

  function tileData(kind, m) {
    if (!m) return undefined;
    if (kind === 'varnish') {
      const d = ratioDonut(m, 'hit_ratio', VARNISH_HIT.num, VARNISH_HIT.den);
      return { value: d.value };
    }
    if (kind === 'rabbitmq') return sumSeries(m, 'ready.');
    if (kind === 'php') return completeMinutes(m);
    return m;
  }

  function tileEmpty(kind, m) {
    if (kind === 'varnish') return collecting(m) ? 'Collecting' : 'No requests in 15 min';
    if (kind === 'php') return m && m.totals && m.totals.requests > 0 ? 'First point when this minute closes' : 'No requests in 15 min';
    return 'Collecting';
  }

  // ---- Alpine --------------------------------------------------------------

  function register(Alpine, I) {
    const store = () => Alpine.store('app');

    // Window and table views live in the hash like every other view state;
    // replaceState keeps the picker out of the back button's history.
    function setQuery(patch) {
      const r = store().route;
      const query = Object.assign({}, r.query, patch);
      for (const k of Object.keys(query)) if (!query[k]) delete query[k];
      const h = route.format({ tab: r.tab, section: r.section, rest: r.rest, query });
      window.history.replaceState(null, '', h);
      store().route = route.parse(h);
    }

    // One factory per kind; each mounted instance gets its own charts, timer
    // and history, so leaving and re-entering Health starts clean.
    function dashboard(kind) {
      const spec = SPECS[kind];
      return () => {
        const made = {};
        let metrics = null;
        let extra = null;
        let timer = null;
        let fx = null;
        let onVis = null;
        let seq = 0;
        let win = '';
        let destroyed = false;
        let extraPending = false;

        return {
          version: 0,
          failed: '',

          init() {
            fx = Alpine.effect(() => {
              const q = store().route.query;
              const w = windowOf(q);
              tableList(q);
              void this.parentHealth();
              if (w !== win) {
                const first = !win;
                win = w;
                setTimeout(() => this.load(!first), 0);
              } else {
                this.$nextTick(() => this.paint());
              }
            });
            this.loadExtra();
            timer = setInterval(() => {
              if (!document.hidden) this.load(false);
            }, POLL_MS);
            onVis = () => {
              if (!document.hidden) this.load(false);
            };
            document.addEventListener('visibilitychange', onVis);
          },

          destroy() {
            destroyed = true;
            if (spec.windowCard) this.shareCard(true);
            if (fx) Alpine.release(fx);
            if (timer) clearInterval(timer);
            if (onVis) document.removeEventListener('visibilitychange', onVis);
            for (const id of Object.keys(made)) made[id].chart.destroy();
          },

          // Extra lookups (the Valkey cache names) are fetched once; a failed
          // one is tried again on the next poll.
          loadExtra() {
            if (!spec.extra || extra || extraPending || destroyed) return;
            extraPending = true;
            spec
              .extra(I)
              .then((x) => {
                extra = x;
                this.paint();
              })
              .catch(() => {})
              .finally(() => {
                extraPending = false;
              });
          },

          async load(dim) {
            if (destroyed) return;
            this.loadExtra();
            const s = ++seq;
            if (dim) for (const id of Object.keys(made)) made[id].chart.stale(true);
            try {
              const m = await I.client.get(spec.url, { query: { window: win }, quiet: true });
              if (s !== seq || destroyed) return;
              metrics = m;
              this.failed = '';
            } catch (e) {
              if (s !== seq || destroyed) return;
              this.failed = e.message;
            }
            this.version++;
            this.paint();
            if (spec.windowCard) this.shareCard();
          },

          // The service view draws the key numbers above this component, so
          // the card for the picked window is handed up to it.
          shareCard(clear) {
            const scope = this.$el && this.$el.parentElement ? Alpine.$data(this.$el.parentElement) : null;
            if (scope && 'windowCard' in scope) scope.windowCard = clear ? null : spec.windowCard(metrics, win);
          },

          paint() {
            if (destroyed) return;
            const q = store().route.query;
            const tables = tableList(q);
            const ctx = { health: this.parentHealth(), extra, win };
            const shown = metrics && spec.prep ? spec.prep(metrics) : metrics;
            for (const c of spec.charts) {
              const host = this.$refs[c.ref];
              if (!host) continue;
              const form = formOf(c, shown, ctx);
              let entry = made[c.id];
              if (entry && entry.form !== form) {
                entry.chart.destroy();
                entry = null;
              }
              if (!entry) {
                const opts = Object.assign({ group: kind, window: win, table: tables.includes(c.id), onTableToggle: (next) => setQuery({ table: withTable(store().route.query, c.id, next) }) }, optsOf(c, shown, form));
                entry = made[c.id] = { form, chart: C[form](host, opts) };
              }
              const more = { table: tables.includes(c.id), window: win, end: endOf(shown), emptyText: c.empty ? c.empty(shown, ctx) : undefined };
              entry.chart.update(shown ? (c.data ? c.data(shown, ctx, form) : shown) : undefined, more);
              entry.chart.stale(false);
            }
          },

          // The service view around the dashboard owns the health poll; a
          // method's this is only this component, so read it through the
          // element's data stack.
          parentHealth() {
            const scope = this.$el && this.$el.parentElement ? Alpine.$data(this.$el.parentElement) : null;
            return scope ? scope.health : null;
          },

          get windows() {
            return windowButtons(store().route.query);
          },
          setWindow(id) {
            setQuery({ win: id === DEFAULT_WINDOW ? '' : id });
          },
          get note() {
            void this.version;
            return sampleNote(kind, metrics, this.failed);
          },
          get sampleError() {
            void this.version;
            return metrics && metrics.error ? 'The last sample failed: ' + metrics.error : '';
          },
          get hasFailed() {
            return !!this.failed && !metrics;
          },
          get kpis() {
            void this.version;
            return kind === 'php' ? phpKpis(metrics, windowOf(store().route.query)) : [];
          },
          get rawMetrics() {
            void this.version;
            return store().raw && metrics ? fmt.pretty(metrics) : '';
          },
          retry() {
            this.load(false);
          },
        };
      };
    }

    Alpine.data('varnishDashboard', dashboard('varnish'));
    Alpine.data('redisDashboard', dashboard('redis'));
    Alpine.data('searchDashboard', dashboard('search'));
    Alpine.data('rabbitmqDashboard', dashboard('rabbitmq'));
    Alpine.data('phpDashboard', dashboard('php'));

    Alpine.data('overviewTiles', () => {
      const made = {};
      const metrics = {};
      let timer = null;
      let onVis = null;
      let fx = null;
      let destroyed = false;
      return {
        version: 0,

        init() {
          // A tile only gets its chart once the overview says the service is
          // there, which can land after the metrics.
          fx = Alpine.effect(() => {
            void store().overview;
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
          for (const k of Object.keys(made)) made[k].destroy();
        },

        async load() {
          const kinds = TILE_ORDER.filter((k) => k === 'php' || I.present(k));
          await Promise.all(
            kinds.map(async (k) => {
              try {
                metrics[k] = await I.client.get(SPECS[k].url, { query: { window: TILE_WINDOW }, quiet: true });
              } catch (e) {
                // A tile keeps its last chart; the health headline still shows.
              }
            }),
          );
          if (destroyed) return;
          this.version++;
          this.$nextTick(() => this.paint());
        },

        paint() {
          if (destroyed) return;
          for (const t of this.tiles) {
            if (t.hasChart !== 'true') continue;
            const host = this.$el.querySelector('[data-tile="' + t.kind + '"]');
            if (!host) continue;
            const spec = TILE_CHART[t.kind];
            if (!made[t.kind]) {
              made[t.kind] =
                spec.form === 'donut'
                  ? C.donut(host, { size: 96, thickness: 10, tableToggle: false })
                  : C.sparkline(host, { unit: spec.unit, key: spec.key, label: spec.caption, window: TILE_WINDOW, height: 36 });
            }
            const m = metrics[t.kind];
            made[t.kind].update(tileData(t.kind, m), { emptyText: tileEmpty(t.kind, m) });
          }
        },

        get tiles() {
          void this.version;
          return tiles(store().overview, metrics, (k, info) => (info ? fmt.serviceLabel(k, info) : I.label(k)));
        },
      };
    });
  }

  return {
    register,
    POLL_MS,
    WINDOWS,
    DEFAULT_WINDOW,
    SPECS,
    windowOf,
    tableList,
    withTable,
    windowButtons,
    endOf,
    lastValue,
    firstValue,
    startRatio,
    ratioDonut,
    ratioEmpty,
    VARNISH_HIT,
    varnishHitCard,
    redisHitCard,
    rabbitPublishCard,
    searchQueriesCard,
    fillDonut,
    groupRows,
    commandLabel,
    commandRows,
    dbRows,
    queueRows,
    consumerRows,
    bucketLabel,
    bucketFull,
    histogramRows,
    missRows,
    missEmpty,
    sumSeries,
    completeMinutes,
    rankRows,
    phpKpis,
    sampleNote,
    tiles,
    tileData,
    formOf,
    optsOf,
  };
});
