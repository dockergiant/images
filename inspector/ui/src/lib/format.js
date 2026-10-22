(function (root, factory) {
  const mod = factory();
  if (typeof module === 'object' && module.exports) {
    module.exports = mod;
  } else {
    root.Inspector = root.Inspector || {};
    root.Inspector.fmt = mod;
  }
})(this, function () {
  const NS_PER_MS = 1e6;

  const ENGINES = {
    varnish: 'Varnish',
    redis: 'Redis',
    valkey: 'Valkey',
    dragonfly: 'Dragonfly',
    opensearch: 'OpenSearch',
    elasticsearch: 'Elasticsearch',
    rabbitmq: 'RabbitMQ',
    php: 'PHP',
  };

  const KIND_FALLBACK = {
    varnish: 'Varnish',
    redis: 'Redis',
    search: 'Search',
    rabbitmq: 'RabbitMQ',
    php: 'PHP',
  };

  const KINDS = ['varnish', 'redis', 'search', 'rabbitmq', 'php'];
  const TONES = ['ok', 'warn', 'error', 'info'];

  function isNum(n) {
    return typeof n === 'number' && Number.isFinite(n);
  }

  function millis(m) {
    if (!isNum(m)) return '';
    if (m === 0) return '0 ms';
    if (m < 1) return '<1 ms';
    if (m < 1000) return Math.round(m) + ' ms';
    const s = m / 1000;
    if (s < 60) return (Math.floor(s * 10) / 10).toFixed(1) + ' s';
    const whole = Math.round(s);
    return Math.floor(whole / 60) + ' min ' + (whole % 60) + ' s';
  }

  function ms(ns) {
    if (!isNum(ns)) return '';
    return millis(ns / NS_PER_MS);
  }

  function bytes(n) {
    if (!isNum(n) || n < 0) return '';
    if (n < 1024) return Math.round(n) + ' B';
    const units = ['KB', 'MB', 'GB', 'TB'];
    let v = n / 1024;
    let i = 0;
    while (v >= 1024 && i < units.length - 1) {
      v /= 1024;
      i++;
    }
    return (v >= 10 ? String(Math.round(v)) : v.toFixed(1)) + ' ' + units[i];
  }

  function num(n) {
    if (!isNum(n)) return '';
    const neg = n < 0;
    const [int, frac] = String(Math.abs(n)).split('.');
    const grouped = int.replace(/\B(?=(\d{3})+(?!\d))/g, ',');
    return (neg ? '-' : '') + grouped + (frac ? '.' + frac : '');
  }

  function pct(ratio, digits) {
    if (!isNum(ratio)) return '';
    return (ratio * 100).toFixed(digits || 0) + '%';
  }

  function toDate(t) {
    if (t instanceof Date) return Number.isNaN(t.getTime()) ? null : t;
    if (isNum(t)) return new Date(t < 1e12 ? t * 1000 : t);
    if (typeof t !== 'string' || t === '' || t.startsWith('0001-01-01')) return null;
    const d = new Date(t);
    return Number.isNaN(d.getTime()) ? null : d;
  }

  function pad(n, w) {
    return String(n).padStart(w || 2, '0');
  }

  function clock(t) {
    const d = toDate(t);
    if (!d) return '';
    return pad(d.getHours()) + ':' + pad(d.getMinutes()) + ':' + pad(d.getSeconds()) + '.' + pad(d.getMilliseconds(), 3);
  }

  function ago(t, now) {
    const d = toDate(t);
    if (!d) return '';
    const s = Math.floor(((now === undefined ? Date.now() : now) - d.getTime()) / 1000);
    if (s < 5) return 'just now';
    if (s < 60) return s + ' s ago';
    if (s < 3600) return Math.floor(s / 60) + ' min ago';
    if (s < 86400) return Math.floor(s / 3600) + ' h ago';
    return Math.floor(s / 86400) + ' d ago';
  }

  function span(seconds) {
    if (!isNum(seconds) || seconds < 0) return '';
    const s = Math.floor(seconds);
    if (s < 60) return s + ' s';
    if (s < 3600) return Math.floor(s / 60) + ' min ' + (s % 60) + ' s';
    if (s < 86400) return Math.floor(s / 3600) + ' h ' + Math.floor((s % 3600) / 60) + ' min';
    return Math.floor(s / 86400) + ' d ' + Math.floor((s % 86400) / 3600) + ' h';
  }

  function truncate(s, n) {
    if (s === null || s === undefined) return '';
    const str = String(s);
    return str.length > n ? str.slice(0, n - 1) + '…' : str;
  }

  function engineName(engine) {
    if (!engine) return '';
    return ENGINES[String(engine).toLowerCase()] || engine;
  }

  function shortVersion(v) {
    if (!v) return '';
    return String(v).split('.').slice(0, 2).join('.');
  }

  function serviceLabel(kind, info) {
    const engine = info && info.engine;
    return engineName(engine) || KIND_FALLBACK[kind] || kind;
  }

  function streamLabel(service, title) {
    title = title.replace(/\s*\([^)]*\)\s*$/, '');
    if (title.toLowerCase().startsWith(service.toLowerCase())) return title;
    return service + ' ' + title.charAt(0).toLowerCase() + title.slice(1);
  }

  function toneClass(tone) {
    return 'tone-' + (TONES.includes(tone) ? tone : 'info');
  }

  function kindClass(kind) {
    return 'kind-' + (KINDS.includes(kind) ? kind : 'none');
  }

  function handlingTone(h) {
    switch (String(h || '').toLowerCase()) {
      case 'hit':
        return 'ok';
      case 'miss':
        return 'warn';
      default:
        return 'info';
    }
  }

  function pretty(v) {
    if (v === undefined || v === null) return '';
    if (typeof v === 'string') return v;
    try {
      return JSON.stringify(v, null, 2);
    } catch (e) {
      return String(v);
    }
  }

  function join(parts) {
    return parts.filter((p) => p !== '' && p !== null && p !== undefined).join(' ');
  }

  function rowParts(ev) {
    const d = ev.data || {};
    switch (ev.type) {
      case 'varnish.req': {
        let text = join([d.method, d.url, d.status || '', ms(d.duration)]);
        if (d.tags && d.tags.length) text += ' · tags ' + d.tags.length;
        if (d.reason) text += ' · ' + d.reason;
        return { badge: String(d.handling || '').toUpperCase(), tone: handlingTone(d.handling), text: text };
      }
      case 'redis.cmd': {
        let text = truncate(join([d.cmd].concat((d.args || []).map((a) => truncate(a, 120)))), 240);
        if (d.folded > 1) text += ' ×' + d.folded;
        if (d.container) text += ' · ' + d.container;
        return { badge: 'db' + (d.db || 0), tone: 'info', text: text };
      }
      case 'search.query':
        return { badge: d.phase || 'query', tone: 'info', text: join([d.label || d.index, ms(d.took), d.summary]) };
      case 'mq.pub': {
        let text = (d.exchange || '(default)') + ' → ' + (d.routingKey || '');
        if (d.count > 1) text += ' ×' + d.count;
        if (d.size) text += ' · ' + bytes(d.size);
        return { badge: 'publish', tone: 'info', text: text };
      }
      case 'trace': {
        let text = join([d.method, d.url, d.status || '', ms(d.duration)]);
        if (d.nPlusOne) text += ' · N+1';
        return { badge: 'trace', tone: d.status >= 500 ? 'error' : 'info', text: text };
      }
      case 'nginx.access':
        return { badge: 'nginx', tone: d.status >= 500 ? 'error' : 'info', text: join([d.method, d.url, d.status || '']) };
      case 'stream.error':
        return { badge: 'error', tone: 'error', text: typeof ev.data === 'string' ? ev.data : pretty(ev.data) };
      default: {
        let text;
        try {
          text = typeof ev.data === 'string' ? ev.data : JSON.stringify(ev.data);
        } catch (e) {
          text = String(ev.data);
        }
        return { badge: ev.type || '', tone: 'info', text: truncate(text, 240) };
      }
    }
  }

  function eventRow(ev, id) {
    const p = rowParts(ev);
    return {
      id: id,
      sub: ev.sub || '',
      type: ev.type || '',
      kind: ev.kind || '',
      time: clock(ev.time),
      badge: p.badge,
      tone: p.tone,
      text: p.text,
      search: (p.badge + ' ' + p.text + ' ' + (ev.kind || '')).toLowerCase(),
      event: ev,
    };
  }

  return {
    ms,
    millis,
    bytes,
    num,
    pct,
    clock,
    ago,
    span,
    truncate,
    engineName,
    shortVersion,
    serviceLabel,
    streamLabel,
    toneClass,
    kindClass,
    handlingTone,
    pretty,
    eventRow,
  };
});
