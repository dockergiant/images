(function (root, factory) {
  const mod = factory();
  if (typeof module === 'object' && module.exports) {
    module.exports = mod;
  } else {
    root.Inspector = root.Inspector || {};
    root.Inspector.route = mod;
  }
})(this, function () {
  const TABS = [
    { id: 'overview', kind: '' },
    { id: 'varnish', kind: 'varnish' },
    { id: 'redis', kind: 'redis' },
    { id: 'search', kind: 'search' },
    { id: 'rabbitmq', kind: 'rabbitmq' },
    { id: 'traces', kind: 'php' },
    { id: 'connect', kind: '' },
    { id: 'performance', kind: '' },
  ];
  const SECTIONS = ['health', 'browse', 'live', 'actions'];
  const SERVICE_TABS = ['varnish', 'redis', 'search', 'rabbitmq'];
  const TRACE_PAGES = ['requests'];
  const TRACE_MODES = ['bottlenecks', 'waterfall', 'flame', 'types', 'spans'];

  function decode(s) {
    try {
      return decodeURIComponent(s);
    } catch (e) {
      return s;
    }
  }

  function isService(tab) {
    return SERVICE_TABS.includes(tab);
  }

  function kindOf(tab) {
    const t = TABS.find((x) => x.id === tab);
    return t ? t.kind : '';
  }

  function parseQuery(s) {
    const out = {};
    for (const pair of String(s || '').split('&')) {
      if (!pair) continue;
      const i = pair.indexOf('=');
      const k = decode((i < 0 ? pair : pair.slice(0, i)).replace(/\+/g, ' '));
      if (!k) continue;
      out[k] = i < 0 ? '' : decode(pair.slice(i + 1).replace(/\+/g, ' '));
    }
    return out;
  }

  function formatQuery(q) {
    const keys = Object.keys(q || {}).filter((k) => q[k] !== undefined && q[k] !== null && q[k] !== '').sort();
    if (!keys.length) return '';
    return '?' + keys.map((k) => encodeURIComponent(k) + '=' + encodeURIComponent(String(q[k]))).join('&');
  }

  function parse(hash) {
    const raw = String(hash || '').replace(/^#\/?/, '');
    const qi = raw.indexOf('?');
    const path = qi < 0 ? raw : raw.slice(0, qi);
    const query = qi < 0 ? {} : parseQuery(raw.slice(qi + 1));
    const parts = path.split('/').filter((p) => p !== '');
    const tab = TABS.some((t) => t.id === parts[0]) ? parts[0] : 'overview';
    if (tab !== parts[0]) return { tab: 'overview', section: '', rest: '', query: {} };
    let section = parts[1] ? decode(parts[1]) : '';
    if (isService(tab) && !SECTIONS.includes(section)) {
      section = 'health';
      return { tab, section, rest: '', query };
    }
    return { tab, section, rest: parts.slice(2).map(decode).join('/'), query };
  }

  function format(r) {
    let h = '#/' + r.tab;
    if (r.section) h += '/' + encodeURIComponent(r.section);
    if (r.rest) h += '/' + String(r.rest).split('/').map(encodeURIComponent).join('/');
    return h + formatQuery(r.query);
  }

  // Which traces screen a route names: Hotspots at #/traces, the request list
  // at #/traces/requests, and a trace (with its tab) at #/traces/<id>[/<mode>].
  function traceView(r) {
    if (!r || r.tab !== 'traces' || !r.section) return { page: 'hotspots', id: '', mode: '' };
    if (TRACE_PAGES.includes(r.section)) return { page: r.section, id: '', mode: '' };
    const mode = TRACE_MODES.includes(r.rest) ? r.rest : 'bottlenecks';
    return { page: 'trace', id: r.section, mode };
  }

  return { TABS, SECTIONS, TRACE_MODES, parse, format, parseQuery, formatQuery, isService, kindOf, traceView };
});
