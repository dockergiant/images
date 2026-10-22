(function (root, factory) {
  const mod = factory();
  if (typeof module === 'object' && module.exports) {
    module.exports = mod;
  } else {
    root.Inspector = root.Inspector || {};
    root.Inspector.api = mod;
  }
})(this, function () {
  const EVENT_TYPES = [
    'timeline',
    'trace',
    'varnish.req',
    'redis.cmd',
    'redis.monitor',
    'search.query',
    'search.capture',
    'mq.pub',
    'nginx.access',
    'benchmark',
    'stream.error',
    'gap',
  ];
  const CLOSED = 2;

  class ApiError extends Error {
    constructor(status, message) {
      super(message);
      this.name = 'ApiError';
      this.status = status;
    }
  }

  function query(params) {
    const parts = [];
    for (const [k, v] of Object.entries(params || {})) {
      if (v === undefined || v === null || v === '') continue;
      parts.push(encodeURIComponent(k) + '=' + encodeURIComponent(String(v)));
    }
    return parts.length ? '?' + parts.join('&') : '';
  }

  function stringParams(p) {
    const out = {};
    for (const [k, v] of Object.entries(p || {})) {
      out[k] = v === undefined || v === null ? '' : String(v);
    }
    return out;
  }

  function canonicalSub(sub) {
    const parts = String(sub || '').trim().split(';');
    const head = parts[0].trim();
    const pairs = parts.slice(1).filter((p) => p !== '');
    pairs.sort((a, b) => {
      const ka = a.split('=')[0];
      const kb = b.split('=')[0];
      return ka < kb ? -1 : ka > kb ? 1 : 0;
    });
    return [head].concat(pairs).join(';');
  }

  function streamURL(subs) {
    const set = new Set();
    for (const s of subs || []) {
      const c = canonicalSub(s);
      if (c) set.add(c);
    }
    const list = Array.from(set).sort();
    if (!list.length) return '/api/stream';
    return '/api/stream?' + list.map((s) => 'sub=' + encodeURIComponent(s)).join('&');
  }

  function kindOfSub(sub) {
    return String(sub || '').split(':')[0];
  }

  function normalizeEvent(type, raw, id) {
    let obj;
    try {
      obj = JSON.parse(raw);
    } catch (e) {
      return null;
    }
    const now = new Date().toISOString();
    if (type === 'gap') {
      return { sub: '', type: 'gap', kind: '', time: now, data: null, id: id || '' };
    }
    if (type === 'stream.error' && obj && obj.type === undefined && obj.error !== undefined) {
      return { sub: obj.sub || '', type: 'stream.error', kind: kindOfSub(obj.sub), time: now, data: String(obj.error), id: id || '' };
    }
    if (!obj || typeof obj !== 'object') return null;
    return Object.assign({}, obj, { id: id || '' });
  }

  async function errorMessage(res) {
    let text = '';
    try {
      text = await res.text();
    } catch (e) {
      text = '';
    }
    try {
      const obj = JSON.parse(text);
      if (obj && typeof obj.error === 'string') return obj.error;
    } catch (e) {
      text = text || '';
    }
    text = text.trim();
    if (text) return text;
    return ('HTTP ' + res.status + ' ' + (res.statusText || '')).trim();
  }

  function createClient(opts) {
    const fetchFn = opts.fetch;
    const csrf = opts.csrf || (() => '');
    const refresh = opts.refresh;
    const onError = opts.onError;

    async function request(method, path, o) {
      o = o || {};
      const url = path + query(o.query);
      const write = method !== 'GET' && method !== 'HEAD';
      const send = () => {
        const headers = { Accept: 'application/json' };
        if (write) {
          headers['Content-Type'] = 'application/json';
          headers['X-Inspector-Csrf'] = csrf();
        }
        const init = { method, headers, credentials: 'same-origin' };
        if (write) init.body = JSON.stringify(o.body === undefined ? {} : o.body);
        if (o.signal) init.signal = o.signal;
        return fetchFn(url, init);
      };
      try {
        let res;
        try {
          res = await send();
        } catch (e) {
          if (e && e.name === 'AbortError') throw e;
          throw new ApiError(0, 'Inspector is unreachable: ' + (e && e.message ? e.message : e));
        }
        if (res.status === 403 && write && refresh) {
          const msg = await errorMessage(res.clone());
          if (msg.includes('X-Inspector-Csrf')) {
            await refresh();
            res = await send();
          }
        }
        if (!res.ok) throw new ApiError(res.status, await errorMessage(res));
        if (res.status === 204) return null;
        const ct = (res.headers && res.headers.get('Content-Type')) || '';
        return ct.includes('json') ? res.json() : res.text();
      } catch (e) {
        if (!o.quiet && onError && !(e && e.name === 'AbortError')) onError(e);
        throw e;
      }
    }

    function get(path, o) {
      return request('GET', path, o);
    }

    function post(path, body, o) {
      return request('POST', path, Object.assign({}, o, { body }));
    }

    async function action(a, o) {
      const params = stringParams(a.params);
      const target = a.target === undefined || a.target === null ? '' : String(a.target);
      let nonce = '';
      if (a.confirm !== 'none') {
        const c = await post('/api/confirm', {
          kind: a.kind,
          action: a.action,
          target,
          params,
          typed: a.typed === undefined || a.typed === null ? '' : String(a.typed),
        }, o);
        nonce = c.nonce;
      }
      return post('/api/actions/' + encodeURIComponent(a.kind) + '/' + encodeURIComponent(a.action), { target, params, nonce }, o);
    }

    return { request, get, post, action };
  }

  function createStream(opts) {
    const ES = opts.EventSource;
    const fetchFn = opts.fetch;
    const retryBase = opts.retryDelay === undefined ? 1000 : opts.retryDelay;
    const MAX_BACKOFF = 30000;
    const delay = opts.delay === undefined ? 30 : opts.delay;
    const consumers = new Set();
    const watchers = new Set();
    if (opts.onStatus) watchers.add(opts.onStatus);
    let es = null;
    let current = '';
    let timer = null;
    let backoffTimer = null;
    let backoff = 0;
    let gen = 0;
    let status = { state: 'idle', error: '' };

    function setStatus(state, error) {
      status = { state, error: error || '' };
      for (const fn of watchers) fn(status);
    }

    function union() {
      const all = [];
      for (const c of consumers) for (const s of c.subs) all.push(s);
      return all;
    }

    function clearBackoff() {
      if (backoffTimer !== null) clearTimeout(backoffTimer);
      backoffTimer = null;
    }

    function closeES() {
      if (es) es.close();
      es = null;
    }

    async function probe(url) {
      if (!fetchFn) return { status: 0, msg: 'The live stream closed.' };
      const ctl = typeof AbortController === 'function' ? new AbortController() : null;
      try {
        const res = await fetchFn(url, { headers: { Accept: 'text/event-stream' }, signal: ctl ? ctl.signal : undefined });
        if (res.ok) {
          if (ctl) ctl.abort();
          return { status: 200, msg: 'The live stream closed. Pick a stream again to retry.' };
        }
        return { status: res.status, msg: await errorMessage(res) };
      } catch (e) {
        return { status: 0, msg: 'Inspector is unreachable: ' + (e && e.message ? e.message : e) };
      }
    }

    function dispatch(ev) {
      if (!ev) return;
      for (const c of consumers) {
        if (ev.type === 'gap' || c.all || c.subs.has(ev.sub)) {
          try {
            c.fn(ev);
          } catch (e) {
            if (typeof console !== 'undefined') console.error(e);
          }
        }
      }
    }

    function open(url) {
      setStatus('connecting');
      const src = new ES(url);
      es = src;
      src.addEventListener('open', () => {
        if (es !== src) return;
        backoff = 0;
        setStatus('open');
      });
      src.addEventListener('error', () => {
        if (es !== src) return;
        if (src.readyState !== CLOSED) {
          setStatus('reconnecting');
          return;
        }
        es = null;
        current = '';
        const mine = ++gen;
        probe(url).then(({ status: code, msg }) => {
          if (mine !== gen || es) return;
          if (code === 400 || code === 423) {
            setStatus('closed', msg);
            return;
          }
          setStatus('reconnecting', msg);
          backoff = backoff ? Math.min(backoff * 2, MAX_BACKOFF) : retryBase;
          clearBackoff();
          backoffTimer = setTimeout(() => {
            backoffTimer = null;
            sync();
          }, backoff);
        });
      });
      for (const type of EVENT_TYPES) {
        src.addEventListener(type, (m) => {
          if (es === src) dispatch(normalizeEvent(type, m.data, m.lastEventId));
        });
      }
    }

    function sync() {
      clearBackoff();
      gen++;
      if (consumers.size === 0) {
        closeES();
        current = '';
        setStatus('idle');
        return;
      }
      const url = streamURL(union());
      if (es && url === current) return;
      closeES();
      current = url;
      open(url);
    }

    function schedule() {
      if (timer !== null) return;
      timer = setTimeout(() => {
        timer = null;
        sync();
      }, delay);
    }

    function subscribe(subs, fn) {
      const list = (subs || []).map(canonicalSub);
      const c = { subs: new Set(list), all: list.includes('*'), fn };
      c.subs.delete('*');
      consumers.add(c);
      schedule();
      return () => {
        if (consumers.delete(c)) schedule();
      };
    }

    function watch(fn) {
      watchers.add(fn);
      fn(status);
      return () => watchers.delete(fn);
    }

    function retry() {
      clearBackoff();
      backoff = 0;
      gen++;
      closeES();
      current = '';
      schedule();
    }

    return {
      subscribe,
      watch,
      retry,
      status: () => status,
      url: () => current,
    };
  }

  return {
    EVENT_TYPES,
    ApiError,
    query,
    stringParams,
    canonicalSub,
    streamURL,
    normalizeEvent,
    errorMessage,
    createClient,
    createStream,
  };
});
