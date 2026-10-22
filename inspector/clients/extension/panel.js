(function () {
  const C = window.RollInspector;
  const net = chrome.devtools.network;
  const MAX_ROWS = 300;

  const $ = (id) => document.getElementById(id);
  const ui = {
    status: $('status'),
    preserve: $('preserve'),
    clear: $('clear'),
    open: $('open'),
    options: $('options'),
    rows: $('rows'),
    empty: $('empty'),
    detail: $('detail'),
  };

  const state = {
    host: '',
    base: '',
    domain: '',
    env: '',
    ready: null,
    finding: null,
    rows: [],
    selected: '',
    seq: 0,
    generation: 0,
  };

  function el(tag, cls, text) {
    const n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text !== undefined && text !== null && text !== '') n.textContent = String(text);
    return n;
  }

  function link(href, text, cls) {
    const a = el('a', cls || '', text);
    a.href = href;
    a.target = '_blank';
    a.rel = 'noopener';
    return a;
  }

  function clearNode(n) {
    while (n.firstChild) n.removeChild(n.firstChild);
  }

  function setStatus(text, tone) {
    ui.status.textContent = text;
    ui.status.className = 'status' + (tone ? ' status-' + tone : '');
  }

  function storage(keys) {
    return new Promise((resolve) => {
      try {
        chrome.storage.local.get(keys, (v) => resolve(v || {}));
      } catch (e) {
        resolve({});
      }
    });
  }

  function remember(obj) {
    try {
      chrome.storage.local.set(obj);
    } catch (e) {
      // Without storage the panel discovers again next time.
    }
  }

  async function getJSON(url) {
    const res = await fetch(url, { credentials: 'omit', cache: 'no-store' });
    let body = null;
    try {
      body = await res.json();
    } catch (e) {
      body = null;
    }
    return { status: res.status, body };
  }

  // ---- discovery ----

  function find(host) {
    if (!host || C.isInspectorHost(host)) return Promise.resolve();
    if (host === state.host && (state.base || state.finding)) return state.finding || Promise.resolve();
    state.host = host;
    state.base = '';
    state.domain = '';
    state.finding = (async () => {
      const key = 'found:' + host;
      const saved = await storage(['override', key]);
      const plan = C.plan({ host, override: saved.override, cached: saved[key] });
      setStatus('Looking for Inspector for ' + host + '…');
      const tried = [];
      for (const base of plan.bases) {
        tried.push(base.replace(/^https?:\/\//, ''));
        try {
          const r = await getJSON(base + '/api/v1/');
          if (r.status === 200 && C.confirms(base, r.body)) {
            state.base = base;
            state.domain = String(r.body.domain).toLowerCase();
            state.env = r.body.env || '';
            if (plan.source !== 'override') remember({ [key]: base });
            ui.open.href = base + '/';
            ui.open.hidden = false;
            setStatus(base.replace(/^https?:\/\//, '') + (state.env ? ' · ' + state.env : '') + (plan.source === 'override' ? ' · set in options' : ''), 'ok');
            return;
          }
        } catch (e) {
          // Not this candidate; the next one may answer.
        }
      }
      ui.open.hidden = true;
      setStatus(tried.length ? 'No Inspector at ' + tried.join(' or ') + '. Set ROLL_INSPECTOR=1 and run roll env up, or set the address in Options.' : 'No shop page inspected yet.', 'warn');
    })();
    const p = state.finding;
    return p.finally(() => {
      if (state.finding === p) state.finding = null;
    });
  }

  // ---- rows ----

  function addEntry(entry) {
    if (!C.tracked(entry)) return;
    const row = C.rowFromEntry(entry, ++state.seq);
    row.state = 'waiting';
    row.page = null;
    row.tries = 0;
    state.rows.push(row);
    if (state.rows.length > MAX_ROWS) {
      const gone = state.rows.shift();
      if (gone.node) gone.node.remove();
    }
    drawRow(row);
    lookup(row, state.generation);
  }

  async function lookup(row, gen) {
    if (state.ready) await state.ready;
    if (state.finding) await state.finding.catch(() => {});
    if (gen !== state.generation) return;
    if (C.isAsset(row.path)) {
      row.state = 'asset';
      drawRow(row);
      return;
    }
    if (!state.base) {
      row.state = 'no-inspector';
      drawRow(row);
      return;
    }
    // The page picks the hosts it calls; their URLs, query strings included,
    // stay out of Inspector and out of any host that claims to be one.
    if (!C.onDomain(row.url, state.domain)) {
      row.state = 'other-host';
      drawRow(row);
      return;
    }
    row.tries++;
    let r;
    try {
      r = await getJSON(C.pageURL(state.base, row));
    } catch (e) {
      row.state = 'error';
      row.error = 'Inspector is not reachable: ' + e.message;
      drawRow(row);
      return;
    }
    if (gen !== state.generation) return;
    const again = row.tries < C.BACKOFF.length;
    if (r.status === 200) {
      row.page = r.body;
      row.state = 'ready';
      drawRow(row);
      // A trace can still be on its way for a MISS; a HIT never gets one.
      if (!r.body.trace && C.handlingOf(row, r.body) !== 'HIT' && again) setTimeout(() => lookup(row, gen), C.BACKOFF[row.tries - 1]);
      return;
    }
    if (again) {
      setTimeout(() => lookup(row, gen), C.BACKOFF[row.tries - 1]);
      return;
    }
    row.state = 'missing';
    row.error = (r.body && (r.body.hint || r.body.error)) || 'Inspector answered ' + r.status;
    drawRow(row);
  }

  function strip(segments, cls) {
    const s = el('div', 'strip ' + (cls || ''));
    if (!segments.length) return s;
    s.setAttribute('role', 'img');
    s.setAttribute('aria-label', segments.map(C.segmentTitle).join(', '));
    for (const seg of segments) {
      const part = el('span', 'seg seg-' + seg.key);
      part.style.width = seg.width + '%';
      part.title = C.segmentTitle(seg);
      s.appendChild(part);
    }
    return s;
  }

  function drawRow(row) {
    const sum = C.summary(row, row.page);
    const tr = row.node || el('tr', 'req-row');
    if (!row.node) {
      tr.tabIndex = 0;
      tr.addEventListener('click', () => select(row.id));
      tr.addEventListener('keydown', (e) => onRowKey(e, row));
      row.node = tr;
      ui.rows.appendChild(tr);
    }
    clearNode(tr);
    tr.setAttribute('aria-selected', state.selected === row.id ? 'true' : 'false');
    const v = el('td', 'c-varnish');
    v.appendChild(el('span', 'pill tone-' + sum.tone, sum.handling));
    tr.appendChild(v);
    const req = el('td', 'c-req');
    req.appendChild(el('span', 'method', row.method));
    const p = el('span', 'mono path', row.path);
    p.title = row.url;
    req.appendChild(p);
    if (sum.route) req.appendChild(el('span', 'route mono', sum.route));
    tr.appendChild(req);
    tr.appendChild(el('td', 'num' + (row.status >= 400 ? ' tone-error-text' : ''), row.status || '–'));
    tr.appendChild(el('td', 'num', C.formatMs(row.ms)));
    const php = el('td', 'num');
    if (row.state === 'ready' || sum.php) php.textContent = sum.php || '–';
    else php.appendChild(el('span', 'muted', stateText(row)));
    if (row.error) php.title = row.error;
    tr.appendChild(php);
    tr.appendChild(el('td', 'num', sum.sql));
    const cost = el('td', 'c-cost');
    cost.appendChild(strip(sum.segments));
    tr.appendChild(cost);
    tr.appendChild(el('td', 'c-n1 warn-text', sum.nplus));
    ui.empty.hidden = state.rows.length > 0;
    if (state.selected === row.id) drawDetail();
  }

  function stateText(row) {
    switch (row.state) {
      case 'waiting':
        return 'asking…';
      case 'no-inspector':
        return 'no Inspector';
      case 'missing':
        return 'not seen';
      case 'asset':
        return 'static file';
      case 'other-host':
        return 'other site';
      case 'error':
        return 'offline';
      default:
        return '';
    }
  }

  function onRowKey(e, row) {
    const i = state.rows.indexOf(row);
    let next = null;
    if (e.key === 'ArrowDown' || e.key === 'j') next = state.rows[i + 1];
    else if (e.key === 'ArrowUp' || e.key === 'k') next = state.rows[i - 1];
    else if (e.key === 'Enter' || e.key === ' ') {
      e.preventDefault();
      select(row.id);
      return;
    }
    if (next) {
      e.preventDefault();
      select(next.id);
      next.node.focus();
    }
  }

  function select(id) {
    const prev = state.rows.find((r) => r.id === state.selected);
    state.selected = id;
    if (prev && prev.node) prev.node.setAttribute('aria-selected', 'false');
    const row = state.rows.find((r) => r.id === id);
    if (row && row.node) row.node.setAttribute('aria-selected', 'true');
    drawDetail();
  }

  // ---- detail ----

  function drawDetail() {
    const row = state.rows.find((r) => r.id === state.selected);
    const d = ui.detail;
    clearNode(d);
    d.hidden = !row;
    if (!row) return;
    const page = row.page;
    const sum = C.summary(row, page);

    const head = el('div', 'd-head');
    head.appendChild(el('span', 'pill tone-' + sum.tone, sum.handling));
    head.appendChild(el('span', 'method', row.method));
    const u = el('span', 'mono d-url', row.path);
    u.title = row.url;
    head.appendChild(u);
    const close = el('button', 'btn btn-ghost btn-icon', '✕');
    close.type = 'button';
    close.setAttribute('aria-label', 'Close detail');
    close.addEventListener('click', () => select(''));
    head.appendChild(close);
    d.appendChild(head);

    const facts = el('div', 'facts');
    const fact = (label, value, mono) => {
      if (!value) return;
      const f = el('span', 'fact');
      f.appendChild(el('span', 'fact-label', label));
      f.appendChild(el('span', 'fact-value' + (mono ? ' mono' : ''), value));
      facts.appendChild(f);
    };
    fact('Status', String(row.status || ''));
    fact('Browser', C.formatMs(row.ms));
    fact('PHP', sum.php);
    fact('SQL', sum.sql);
    fact('Route', sum.route, true);
    fact('X-Varnish', row.xVarnish, true);
    if (page) fact('Joined by', page.match === 'xid' ? 'Varnish xid' : 'URL');
    d.appendChild(facts);

    if (!page) {
      d.appendChild(el('p', 'soft', row.error || (row.state === 'no-inspector' ? ui.status.textContent : 'Asking Inspector…')));
      return;
    }
    if (page.why && page.why.value) {
      const why = el('p', 'why');
      why.appendChild(el('span', 'why-label', page.why.label));
      why.appendChild(document.createTextNode(' ' + page.why.value));
      if (page.why.sub) why.appendChild(el('span', 'mono soft why-sub', page.why.sub));
      d.appendChild(why);
    }
    if (sum.segments.length) {
      d.appendChild(strip(sum.segments, 'strip-wide'));
      const legend = el('div', 'legend');
      for (const s of sum.segments) {
        const item = el('span', 'lg lg-' + s.key, s.label + ' ' + C.formatMs(s.ms) + ' · ' + Math.round(s.share * 100) + '%');
        legend.appendChild(item);
      }
      d.appendChild(legend);
    }

    const hot = C.hotRows(page, 5);
    if (hot.length) {
      d.appendChild(el('h2', 'h2', 'Hot spans'));
      const list = el('ol', 'hot');
      for (const h of hot) {
        const li = el('li');
        const item = h.href ? link(h.href, null, 'hot-item') : el('div', 'hot-item');
        item.title = h.label;
        item.appendChild(el('span', 'rank heat-' + h.heat, h.rank));
        const body = el('span', 'hot-body');
        body.appendChild(el('span', 'mono hot-label', h.label));
        if (h.path) body.appendChild(el('span', 'hot-path', h.path));
        const bar = el('span', 'share');
        const fill = el('span', 'share-fill heat-' + h.heat);
        fill.style.width = h.bar + '%';
        bar.appendChild(fill);
        body.appendChild(bar);
        item.appendChild(body);
        item.appendChild(el('span', 'hot-cost', h.self + ' · ' + h.share + (h.calls ? ' · ' + h.calls : '')));
        li.appendChild(item);
        list.appendChild(li);
      }
      d.appendChild(list);
    } else if (!page.trace) {
      d.appendChild(el('p', 'soft', sum.handling === 'HIT' ? 'Served from the Varnish cache, so PHP did not run.' : 'No PHP trace for this request. Switch tracing on with roll inspector trace on.'));
    }

    const reps = C.repeatRows(page);
    if (reps.length) {
      d.appendChild(el('h2', 'h2', 'Repeated calls'));
      const list = el('ul', 'repeats');
      for (const r of reps) {
        const li = el('li', 'repeat');
        li.appendChild(el('span', 'repeat-count', r.count));
        const t = el('span', 'mono repeat-text', r.text);
        t.title = r.text;
        li.appendChild(t);
        li.appendChild(el('span', 'repeat-cost', r.total + ' · ' + r.avg + ' avg'));
        list.appendChild(li);
      }
      d.appendChild(list);
    }

    const links = el('div', 'links');
    const l = page.links || {};
    if (l.bottlenecks) links.appendChild(link(l.bottlenecks, 'Open the trace in Inspector', 'btn btn-accent'));
    if (l.trace) links.appendChild(link(l.trace, 'Waterfall', 'btn'));
    if (l.request) links.appendChild(link(l.request, 'Timeline entry', 'btn btn-ghost'));
    d.appendChild(links);
  }

  // ---- lifecycle ----

  function reset() {
    state.generation++;
    state.rows = [];
    state.selected = '';
    clearNode(ui.rows);
    ui.detail.hidden = true;
    ui.empty.hidden = false;
  }

  ui.clear.addEventListener('click', reset);
  ui.options.href = chrome.runtime.getURL('options.html');

  net.onNavigated.addListener((url) => {
    if (!ui.preserve.checked) reset();
    find(C.hostOf(url));
  });
  // Inspector is found from the inspected main frame only: a HAR entry or an
  // iframe may belong to any site the page embeds or calls.
  state.ready = new Promise((resolve) => {
    try {
      chrome.devtools.inspectedWindow.eval('location.hostname', (h, exc) => resolve(!exc && typeof h === 'string' ? h : ''));
    } catch (e) {
      resolve('');
    }
  }).then((host) => {
    if (!host) {
      setStatus('Reload the inspected page to see its requests.');
      return;
    }
    return find(host).catch(() => {});
  });
  net.onRequestFinished.addListener((entry) => addEntry(entry));
  state.ready.then(() => {
    net.getHAR((har) => {
      ((har && har.entries) || []).filter(C.tracked).forEach(addEntry);
    });
  });
})();
