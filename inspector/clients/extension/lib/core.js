// The DevTools panel's pure parts: HAR rows, Inspector discovery and the
// shapes the panel draws. node --test loads this file; the panel loads it
// as a classic script.
(function (root, factory) {
  const mod = factory();
  if (typeof module === 'object' && module.exports) {
    module.exports = mod;
  } else {
    root.RollInspector = mod;
  }
})(this, function () {
  const TRACKED = ['document', 'xhr', 'fetch'];
  // The trace lands just after the response, so a row asks again after
  // 250 ms, 500 ms, 1 s and 2 s.
  const BACKOFF = [250, 500, 1000, 2000, 2000];
  const CATEGORIES = [
    { key: 'sql', label: 'SQL' },
    { key: 'cache', label: 'Cache' },
    { key: 'search', label: 'Search' },
    { key: 'http', label: 'HTTP' },
    { key: 'php', label: 'PHP' },
  ];
  const MIN_SEGMENT = 0.6;

  function header(headers, name) {
    const n = String(name).toLowerCase();
    const h = (headers || []).find((x) => String(x.name).toLowerCase() === n);
    return h ? String(h.value) : '';
  }

  // X-Varnish holds the client request's id, and on a HIT also the id of the
  // fetch that stored the object: "426144 1442055".
  function parseXVarnish(value) {
    return String(value || '')
      .split(/[\s,]+/)
      .filter((t) => /^\d+$/.test(t));
  }

  function hostOf(url) {
    try {
      return new URL(url).host.toLowerCase();
    } catch (e) {
      return '';
    }
  }

  function isInspectorHost(host) {
    return /^inspector\./i.test(String(host || ''));
  }

  // Chrome's HAR names the resource type; without it the response type
  // stands in, so pages and JSON calls still show.
  function typeOf(entry) {
    if (entry._resourceType) return entry._resourceType;
    const mime = String((entry.response && entry.response.content && entry.response.content.mimeType) || '').toLowerCase();
    if (mime.startsWith('text/html')) return 'document';
    if (mime.includes('json')) return 'fetch';
    return '';
  }

  function tracked(entry) {
    if (!entry || !entry.request) return false;
    if (!TRACKED.includes(typeOf(entry))) return false;
    const url = String(entry.request.url || '');
    if (!/^https?:/i.test(url)) return false;
    return !isInspectorHost(hostOf(url));
  }

  function pathOf(url) {
    try {
      const u = new URL(url);
      return u.pathname + u.search;
    } catch (e) {
      return String(url || '');
    }
  }

  // Magento serves these from disk: no trace and no timeline row, so asking
  // Inspector only produces 404s.
  function isAsset(path) {
    return /^\/(pub\/)?(static|media)\//.test(String(path || ''));
  }

  function rowFromEntry(entry, seq) {
    const res = entry.response || {};
    const xv = header(res.headers, 'x-varnish');
    const started = Date.parse(entry.startedDateTime);
    return {
      id: 'r' + seq,
      method: entry.request.method,
      url: entry.request.url,
      path: pathOf(entry.request.url),
      type: typeOf(entry),
      status: res.status || 0,
      xVarnish: xv,
      xids: parseXVarnish(xv),
      xCache: header(res.headers, 'x-cache').toUpperCase(),
      ms: entry.time > 0 ? entry.time : -1,
      started: isNaN(started) ? 0 : started,
    };
  }

  // Inspector lives at inspector.<domain>. A shop on app.<domain> drops its
  // first label; a shop on the bare domain keeps it.
  function candidates(host) {
    const h = String(host || '')
      .toLowerCase()
      .replace(/:\d+$/, '');
    if (!h || isInspectorHost(h)) return [];
    const labels = h.split('.');
    const out = [];
    if (labels.length > 2) out.push('https://inspector.' + labels.slice(1).join('.'));
    out.push('https://inspector.' + h);
    return out;
  }

  function normalizeBase(raw) {
    const s = String(raw || '').trim();
    if (!s) return '';
    let u;
    try {
      u = new URL(/^[a-z]+:\/\//i.test(s) ? s : 'https://' + s);
    } catch (e) {
      return '';
    }
    if (u.protocol !== 'https:' && u.protocol !== 'http:') return '';
    return u.protocol + '//' + u.host;
  }

  // The options page's override wins, then what an earlier discovery found
  // for this host, then the candidates in order.
  function plan(opts) {
    const o = opts || {};
    const override = normalizeBase(o.override);
    if (override) return { source: 'override', bases: [override] };
    const cached = normalizeBase(o.cached);
    const found = candidates(o.host);
    if (cached) return { source: 'cache', bases: [cached].concat(found.filter((b) => b !== cached)) };
    return { source: 'discovery', bases: found };
  }

  // GET /api/v1/ answers {api: 1, domain}; the domain must be the one the
  // base names, so a catch-all vhost answering 200 does not count.
  function confirms(base, body) {
    if (!body || body.api !== 1 || !body.domain) return false;
    return hostOf(base) === 'inspector.' + String(body.domain).toLowerCase();
  }

  // Shops call sibling hosts under their own domain (static., media.), so the
  // gate is the domain Inspector confirmed, not the inspected host.
  function onDomain(url, domain) {
    const d = String(domain || '').toLowerCase();
    if (!d) return false;
    const h = hostOf(url);
    return h === d || h.endsWith('.' + d);
  }

  function pageURL(base, row) {
    let u = base + '/api/v1/page?url=' + encodeURIComponent(String(row.url).split('#')[0]);
    if (row.xids && row.xids.length) u += '&xid=' + encodeURIComponent(row.xids.join(' '));
    if (row.started) u += '&since=' + Math.floor(row.started);
    return u;
  }

  function formatMs(ms) {
    if (!(ms >= 0)) return '';
    if (ms >= 1000) return (ms / 1000).toFixed(ms >= 10000 ? 1 : 2) + ' s';
    if (ms >= 10) return Math.round(ms) + ' ms';
    return ms.toFixed(1) + ' ms';
  }

  function handlingOf(row, page) {
    let h = String((page && page.handling) || '').toUpperCase();
    // The response header tells HIT from MISS even when Inspector has no
    // Varnish row (varnishlog only runs while the Overview is open).
    if (!h && row && row.xCache) h = row.xCache.split(/[\s,]/)[0];
    if (!h && row && row.xids && row.xids.length === 2) h = 'HIT';
    if (h === 'HITMISS') return 'MISS';
    if (h === 'HITPASS') return 'PASS';
    return h;
  }

  function toneOf(handling) {
    switch (handling) {
      case 'HIT':
        return 'hit';
      case 'MISS':
        return 'miss';
      case 'PASS':
      case 'PIPE':
      case 'SYNTH':
        return 'pass';
      default:
        return 'none';
    }
  }

  function segments(times) {
    const t = times || {};
    const total = CATEGORIES.reduce((n, c) => n + Math.max(0, Number(t[c.key]) || 0), 0);
    if (!(total > 0)) return [];
    return CATEGORIES.map((c) => {
      const ms = Math.max(0, Number(t[c.key]) || 0);
      return { key: c.key, label: c.label, ms, share: ms / total, width: Math.max(MIN_SEGMENT, (ms / total) * 100) };
    }).filter((s) => s.ms > 0);
  }

  function segmentTitle(s) {
    return s.label + ': ' + formatMs(s.ms) + ' (' + Math.round(s.share * 100) + '%)';
  }

  function heatOf(share) {
    if (share >= 0.25) return 4;
    if (share >= 0.1) return 3;
    if (share >= 0.03) return 2;
    return 1;
  }

  function itemLabel(item) {
    const res = String(item.resource || '');
    if (['sql', 'cache', 'search', 'http'].includes(item.kind)) return res || item.name;
    return res && res !== item.name ? item.name + ' ' + res : item.name;
  }

  function hotRows(page, n) {
    const base = (page && page.links && page.links.bottlenecks) || '';
    return ((page && page.hot) || []).slice(0, n || 5).map((h) => ({
      rank: h.rank,
      kind: h.kind,
      label: itemLabel(h),
      path: (h.path || []).join(' › '),
      self: formatMs(h.selfMs),
      share: Math.round((h.share || 0) * 100) + '%',
      bar: Math.max(MIN_SEGMENT, Math.min(100, (h.share || 0) * 100)),
      heat: heatOf(h.share),
      calls: h.calls > 1 ? '×' + h.calls : '',
      href: base ? base + '?focus=' + encodeURIComponent(h.spanId) : '',
    }));
  }

  function repeatRows(page) {
    return ((page && page.repeats) || []).map((r) => ({
      kind: r.kind,
      count: '×' + r.count,
      text: r.example || r.key,
      total: formatMs(r.totalMs),
      avg: formatMs(r.avgMs),
    }));
  }

  // The cells a list row shows once Inspector answered.
  function summary(row, page) {
    const handling = handlingOf(row, page);
    const t = page && page.trace;
    const counts = (t && t.counts) || {};
    const repeats = (page && page.repeatCount) || ((page && page.repeats) || []).length;
    return {
      handling: handling || '–',
      tone: toneOf(handling),
      php: t ? formatMs(t.duration / 1e6) : '',
      sql: t ? String(counts.sql || 0) : '',
      nplus: repeats ? 'N+1 ×' + repeats : '',
      route: t ? t.route : '',
      segments: segments(t && t.times),
    };
  }

  return {
    BACKOFF,
    header,
    parseXVarnish,
    hostOf,
    isInspectorHost,
    tracked,
    isAsset,
    pathOf,
    rowFromEntry,
    candidates,
    normalizeBase,
    plan,
    confirms,
    onDomain,
    pageURL,
    formatMs,
    handlingOf,
    toneOf,
    segments,
    segmentTitle,
    heatOf,
    hotRows,
    repeatRows,
    summary,
  };
});
