(function (root, factory) {
  const isNode = typeof module === 'object' && module.exports;
  const deps = isNode ? { route: require('../lib/route.js') } : root.Inspector;
  const mod = factory(deps);
  if (isNode) {
    module.exports = mod;
  } else {
    root.Inspector.connect = mod;
    document.addEventListener('alpine:init', () => mod.register(window.Alpine, root.Inspector));
  }
})(this, function (deps) {
  const route = deps.route;

  const USERSCRIPT = '/userscript/roll-inspector.user.js';
  const EXTENSION = '/extension/roll-inspector.zip';
  const DEFAULT_EP = '/api/v1/page';
  const GROUPS = [
    { id: 'inspector', label: 'Inspector', heads: ['', 'openapi.json', 'overview'] },
    { id: 'requests', label: 'Requests', heads: ['page', 'requests'] },
    { id: 'traces', label: 'PHP traces', heads: ['traces', 'hotspots'] },
    { id: 'live', label: 'Live', heads: ['stream'] },
  ];
  const SERVICE_LABELS = { varnish: 'Varnish', redis: 'Valkey / Redis', search: 'OpenSearch', rabbitmq: 'RabbitMQ' };

  function headOf(path) {
    return String(path || '')
      .replace(/^\/api\/v1\/?/, '')
      .split('/')[0];
  }

  // Groups keep the index's order inside each group; services follow the
  // built-in groups in the order the adapters list them.
  function groupEndpoints(endpoints) {
    const out = GROUPS.map((g) => ({ id: g.id, label: g.label, items: [] }));
    const byService = new Map();
    for (const ep of endpoints || []) {
      const head = headOf(ep.path);
      const g = GROUPS.findIndex((x) => x.heads.includes(head));
      if (g >= 0) {
        out[g].items.push(ep);
        continue;
      }
      if (!byService.has(head)) {
        const grp = { id: head, label: SERVICE_LABELS[head] || head, items: [] };
        byService.set(head, grp);
        out.push(grp);
      }
      byService.get(head).items.push(ep);
    }
    return out.filter((g) => g.items.length);
  }

  // Fills an endpoint path with values from this env, so the example runs
  // as it stands.
  function exampleURL(base, path, ctx) {
    const c = ctx || {};
    let p = String(path)
      .replace('{id}', c.traceId || '<trace id>')
      .replace('{key}', encodeURIComponent(c.requestKey || '<key>'))
      .replace('{name}', '<name>');
    const q = [];
    switch (p) {
      case '/api/v1/page':
        q.push('url=' + encodeURIComponent(c.pageUrl || 'https://app.' + (c.domain || 'example.test') + '/'));
        break;
      case '/api/v1/requests':
      case '/api/v1/traces':
        q.push('limit=5');
        break;
      case '/api/v1/hotspots':
        q.push('limit=10');
        break;
      case '/api/v1/traces/metrics':
        q.push('window=5m');
        break;
      case '/api/v1/stream':
        q.push('sub=php%3Atraces');
        break;
      case '/api/v1/search/search':
        q.push('body=' + encodeURIComponent(JSON.stringify({ index: '<index>', term: 'shirt', size: 5 })));
        break;
      case '/api/v1/search/analyze':
        q.push('body=' + encodeURIComponent(JSON.stringify({ text: 'Running shoes' })));
        break;
      default:
    }
    return base + p + (q.length ? '?' + q.join('&') : '');
  }

  function shellQuote(s) {
    return "'" + String(s).replace(/'/g, "'\\''") + "'";
  }

  function isStream(path) {
    return path === '/api/v1/stream';
  }

  function curlSnippet(url, path) {
    if (isStream(path)) return 'curl -sN ' + shellQuote(url);
    return 'curl -s ' + shellQuote(url) + ' | jq';
  }

  function jsString(s) {
    return JSON.stringify(String(s));
  }

  // The page lookup reads the current tab's address, which is what a
  // userscript or console snippet on a shop page wants.
  function fetchSnippet(base, url, path) {
    if (isStream(path)) {
      return ['const es = new EventSource(' + jsString(url) + ');', "es.addEventListener('trace', (e) => console.log(JSON.parse(e.data)));"].join('\n');
    }
    if (path === '/api/v1/page') {
      return ['const res = await fetch(' + jsString(base + '/api/v1/page?url=') + ' + encodeURIComponent(location.href));', 'console.log(await res.json());'].join('\n');
    }
    return ['const res = await fetch(' + jsString(url) + ');', 'console.log(await res.json());'].join('\n');
  }

  function consoleSnippet(base) {
    return 'fetch(' + jsString(base + '/api/v1/') + ').then((r) => r.json()).then((j) => console.log(j.env, j.endpoints.length + " endpoints"))';
  }

  function needsValue(url) {
    return /<[^>]+>/.test(url) || /%3C[^%]+%3E/i.test(url);
  }

  // What the probe found, as label and value rows.
  function probeRows(r) {
    const rows = [];
    rows.push({ key: 'status', label: 'Status', value: String(r.status), tone: r.status === 200 ? 'ok' : 'error' });
    const acao = r.acao || '';
    rows.push({ key: 'acao', label: 'Access-Control-Allow-Origin', value: acao || 'missing', tone: acao === '*' ? 'ok' : 'error', mono: true });
    if (r.body && r.body.env) rows.push({ key: 'env', label: 'Env', value: r.body.env, tone: '', mono: true });
    if (r.body && Array.isArray(r.body.endpoints)) rows.push({ key: 'eps', label: 'Endpoints', value: String(r.body.endpoints.length), tone: '' });
    if (r.ms >= 0) rows.push({ key: 'ms', label: 'Answered in', value: Math.round(r.ms) + ' ms', tone: '' });
    return rows;
  }

  function probeVerdict(r) {
    if (r.error) return { tone: 'error', text: 'The call failed: ' + r.error };
    if (r.status === 200 && r.acao === '*') return { tone: 'ok', text: 'v1 answers with Access-Control-Allow-Origin: *, so shop pages, userscripts and extensions can read it.' };
    if (r.status === 200) return { tone: 'warn', text: 'v1 answered without Access-Control-Allow-Origin: *, so a shop page cannot read it. This Inspector predates the read API: run roll env pull and roll env up.' };
    return { tone: 'error', text: 'v1 answered ' + r.status + '. This Inspector may predate the read API: run roll env pull and roll env up.' };
  }

  function register(Alpine, I) {
    const store = () => Alpine.store('app');

    async function copy(text, what) {
      try {
        await navigator.clipboard.writeText(text);
        I.toast('Copied ' + what, 'ok');
      } catch (e) {
        I.toast('Copy failed. Select the text and copy it by hand.', 'warn');
      }
    }

    Alpine.data('connect', () => ({
      index: null,
      state: 'loading',
      error: '',
      ctx: {},
      probe: null,
      probing: false,

      init() {
        this.load();
      },

      async load() {
        try {
          this.index = await I.client.get('/api/v1/', { quiet: true });
          this.state = 'ready';
        } catch (e) {
          this.state = 'error';
          this.error = e.message;
          return;
        }
        // Real values for the examples, when the env has seen any traffic.
        try {
          const [reqs, traces] = await Promise.all([I.client.get('/api/v1/requests', { query: { limit: 1 }, quiet: true }), I.client.get('/api/v1/traces', { query: { limit: 1, sort: 'recent' }, quiet: true })]);
          const r = Array.isArray(reqs) && reqs[0];
          const t = Array.isArray(traces) && traces[0];
          this.ctx = {
            pageUrl: r && r.host ? 'https://' + r.host + r.url : t ? t.url : '',
            requestKey: r ? r.key : '',
            traceId: t ? t.id : r ? r.traceId || '' : '',
          };
        } catch (e) {
          this.ctx = {};
        }
      },

      get domain() {
        const s = store().session;
        return (this.index && this.index.domain) || (s && s.domain) || '';
      },
      get inspectorHost() {
        return 'inspector.' + this.domain;
      },
      get base() {
        return (this.index && this.index.inspectorUrl) || window.location.origin;
      },
      get userscriptHref() {
        return USERSCRIPT;
      },
      get extensionHref() {
        return EXTENSION;
      },
      get openapiHref() {
        return '/api/v1/openapi.json';
      },
      get matchText() {
        return 'https://*.' + this.domain + '/*';
      },
      get loading() {
        return this.state === 'loading';
      },
      get failed() {
        return this.state === 'error';
      },
      get ready() {
        return this.state === 'ready';
      },
      get endpointCount() {
        return this.index ? this.index.endpoints.length + ' endpoints' : '';
      },

      get selectedPath() {
        return store().route.query.ep || DEFAULT_EP;
      },
      get groups() {
        const sel = this.selectedPath;
        return groupEndpoints(this.index ? this.index.endpoints : []).map((g) => ({
          id: g.id,
          label: g.label,
          labelId: 'ep-group-' + g.id,
          items: g.items.map((ep) => ({
            key: ep.path,
            path: ep.path.replace(/^\/api\/v1/, '') || '/',
            summary: ep.summary,
            href: route.format({ tab: 'connect', query: { ep: ep.path } }),
            current: ep.path === sel ? 'page' : null,
          })),
        }));
      },
      get selected() {
        const eps = this.index ? this.index.endpoints : [];
        return eps.find((e) => e.path === this.selectedPath) || eps.find((e) => e.path === DEFAULT_EP) || null;
      },
      get hasSelected() {
        return !!this.selected;
      },
      get selectedTitle() {
        return this.selected ? this.selected.path : '';
      },
      get selectedSummary() {
        return this.selected ? this.selected.summary : '';
      },
      get exampleUrl() {
        if (!this.selected) return '';
        return exampleURL(this.base, this.selected.path, Object.assign({ domain: this.domain }, this.ctx));
      },
      get curl() {
        return this.selected ? curlSnippet(this.exampleUrl, this.selected.path) : '';
      },
      get fetchCode() {
        return this.selected ? fetchSnippet(this.base, this.exampleUrl, this.selected.path) : '';
      },
      get canOpen() {
        return !!this.selected && !isStream(this.selected.path) && !needsValue(this.exampleUrl);
      },
      get needsValueText() {
        return this.selected && needsValue(this.exampleUrl) ? 'Replace the value in angle brackets before running this.' : '';
      },
      get consoleCode() {
        return consoleSnippet(this.base);
      },

      async runProbe() {
        this.probing = true;
        const t0 = performance.now();
        const r = { status: 0, acao: '', body: null, ms: -1, error: '' };
        try {
          const res = await window.fetch('/api/v1/', { cache: 'no-store', credentials: 'omit' });
          r.status = res.status;
          r.acao = res.headers.get('Access-Control-Allow-Origin') || '';
          r.ms = performance.now() - t0;
          try {
            r.body = await res.json();
          } catch (e) {
            r.body = null;
          }
        } catch (e) {
          r.error = e.message;
        }
        this.probe = r;
        this.probing = false;
      },
      get probeLabel() {
        return this.probing ? 'Testing…' : this.probe ? 'Test again' : 'Run the test';
      },
      get hasProbe() {
        return !!this.probe;
      },
      get probeRows() {
        return this.probe ? probeRows(this.probe).map((x) => Object.assign(x, { ddCls: [x.tone ? 'tone-' + x.tone : '', x.mono ? 'mono' : ''].join(' ').trim() })) : [];
      },
      get probeVerdict() {
        return this.probe ? probeVerdict(this.probe).text : '';
      },
      get probeCls() {
        return this.probe ? 'tone-' + probeVerdict(this.probe).tone : '';
      },

      copyCurl() {
        return copy(this.curl, 'the curl command');
      },
      copyFetch() {
        return copy(this.fetchCode, 'the fetch snippet');
      },
      copyConsole() {
        return copy(this.consoleCode, 'the console line');
      },
      copyExtensions() {
        return copy('chrome://extensions', 'chrome://extensions');
      },
    }));
  }

  return { register, groupEndpoints, exampleURL, shellQuote, curlSnippet, fetchSnippet, consoleSnippet, needsValue, probeRows, probeVerdict, headOf };
});
