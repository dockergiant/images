// ==UserScript==
// @name         Roll Inspector badge (__ROLL_DOMAIN__)
// @namespace    https://inspector.__ROLL_DOMAIN__/
// @version      1.0.0
// @description  Shows how each shop page was served (Varnish result, PHP time, SQL calls, N+1) and its hottest spans, from Roll Inspector.
// @match        https://*.__ROLL_DOMAIN__/*
// @match        https://__ROLL_DOMAIN__/*
// @exclude      https://inspector.__ROLL_DOMAIN__/*
// @connect      inspector.__ROLL_DOMAIN__
// @grant        GM_xmlhttpRequest
// @grant        GM_getValue
// @grant        GM_setValue
// @grant        GM_registerMenuCommand
// @run-at       document-idle
// @noframes
// @downloadURL  https://inspector.__ROLL_DOMAIN__/userscript/roll-inspector.user.js
// @updateURL    https://inspector.__ROLL_DOMAIN__/userscript/roll-inspector.user.js
// ==/UserScript==

// Inspector serves this file with the env's domain filled in. The pure helpers
// sit in the factory so node --test can load them without a browser.
(function (factory) {
  const core = factory();
  if (typeof module === 'object' && module.exports) {
    module.exports = core;
  } else {
    core.main(window);
  }
})(function () {
  const DOMAIN = '__ROLL_DOMAIN__';
  // 250 ms, then 500 ms, 1 s and 2 s: the trace lands just after the page.
  const BACKOFF = [250, 500, 1000, 2000, 2000];
  const CATEGORIES = [
    { key: 'sql', label: 'SQL' },
    { key: 'cache', label: 'Cache' },
    { key: 'search', label: 'Search' },
    { key: 'http', label: 'HTTP' },
    { key: 'php', label: 'PHP' },
  ];
  const MIN_SEGMENT = 0.6;

  function inspectorBase(domain) {
    return 'https://inspector.' + domain;
  }

  function isInspectorHost(host, domain) {
    return String(host || '').toLowerCase() === 'inspector.' + String(domain || '').toLowerCase();
  }

  function pageURL(base, href, since) {
    let u = base + '/api/v1/page?url=' + encodeURIComponent(String(href || '').split('#')[0]);
    if (since) u += '&since=' + Math.floor(since);
    return u;
  }

  // The wait before each try; five tries span about 6 s.
  function delays(tries) {
    const out = [];
    for (let i = 0; i < tries; i++) out.push(BACKOFF[Math.min(i, BACKOFF.length - 1)]);
    return out;
  }

  function formatMs(ms) {
    if (!(ms >= 0)) return '';
    if (ms >= 1000) return (ms / 1000).toFixed(ms >= 10000 ? 1 : 2) + ' s';
    if (ms >= 10) return Math.round(ms) + ' ms';
    return ms.toFixed(1) + ' ms';
  }

  function handlingOf(page) {
    const h = String((page && page.handling) || '').toUpperCase();
    if (h === 'HITMISS') return 'MISS';
    if (h === 'HITPASS') return 'PASS';
    return h;
  }

  function toneOf(page) {
    switch (handlingOf(page)) {
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

  function durationMs(page) {
    if (page && page.trace && page.trace.duration > 0) return page.trace.duration / 1e6;
    if (page && page.request && page.request.duration > 0) return page.request.duration / 1e6;
    return -1;
  }

  // "MISS · 961 ms · 56 SQL · N+1 ×3". Without a Varnish row the request
  // reached PHP directly, so the badge says PHP.
  function badgeText(page) {
    const parts = [handlingOf(page) || (page && page.trace ? 'PHP' : '–')];
    const ms = durationMs(page);
    if (ms >= 0) parts.push(formatMs(ms));
    const counts = (page && page.trace && page.trace.counts) || {};
    if (counts.sql) parts.push(counts.sql + ' SQL');
    const repeats = (page && page.repeatCount) || ((page && page.repeats) || []).length;
    if (repeats) parts.push('N+1 ×' + repeats);
    return parts.join(' · ');
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

  function itemLabel(item) {
    const res = String(item.resource || '');
    if (item.kind === 'sql' || item.kind === 'cache' || item.kind === 'search' || item.kind === 'http') return res || item.name;
    return res && res !== item.name ? item.name + ' ' + res : item.name;
  }

  function topItems(page, n) {
    const base = (page && page.links && page.links.bottlenecks) || '';
    return ((page && page.hot) || []).slice(0, n || 3).map((h) => ({
      rank: h.rank,
      kind: h.kind,
      label: itemLabel(h),
      self: formatMs(h.selfMs),
      share: Math.round((h.share || 0) * 100) + '%',
      heat: heatOf(h.share),
      calls: h.calls > 1 ? '×' + h.calls : '',
      href: base ? base + '?focus=' + encodeURIComponent(h.spanId) : '',
    }));
  }

  // The heat ramp marks cost only: the share of the request decides the step.
  function heatOf(share) {
    if (share >= 0.25) return 4;
    if (share >= 0.1) return 3;
    if (share >= 0.03) return 2;
    return 1;
  }

  function repeatLine(r) {
    return r.count + '× ' + (r.example || r.key) + ' · ' + formatMs(r.totalMs);
  }

  function hideKey(host) {
    return 'hidden:' + String(host || '').toLowerCase();
  }

  function waitingText(tries, max) {
    return tries >= max ? 'not seen yet' : 'waiting…';
  }

  const CSS = `
:host { all: initial; }
.root { position: fixed; right: 16px; bottom: 16px; z-index: 2147483647; font: 500 12px/1.4 "IBM Plex Sans", system-ui, -apple-system, "Segoe UI", sans-serif; color: #E4E8EC; font-variant-numeric: tabular-nums; }
.badge { display: inline-flex; align-items: center; gap: 8px; min-height: 28px; padding: 0 12px 0 10px; border: 1px solid #28313B; border-left: 3px solid var(--tone); border-radius: 6px; background: #171D23; color: inherit; font: inherit; cursor: pointer; box-shadow: 0 6px 20px rgb(5 7 10 / 0.55); }
.badge:hover { background: #1E252D; }
.badge:focus-visible, a:focus-visible, button:focus-visible { outline: 2px solid #6CB6FF; outline-offset: 2px; }
.dot { width: 8px; height: 8px; border-radius: 50%; background: var(--tone); flex: none; }
.tone-hit { --tone: #7EE2A8; }
.tone-miss { --tone: #FFB86B; }
.tone-pass { --tone: #C3CAD2; }
.tone-none { --tone: #8B96A3; }
.pop { position: absolute; right: 0; bottom: 36px; width: min(380px, calc(100vw - 32px)); padding: 12px; border: 1px solid #28313B; border-radius: 8px; background: #171D23; box-shadow: 0 12px 32px rgb(5 7 10 / 0.6); display: flex; flex-direction: column; gap: 10px; }
.pop[hidden] { display: none; }
.head { display: flex; align-items: baseline; gap: 8px; }
.title { font-weight: 600; font-size: 13px; }
.sub { color: #8B96A3; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.mono { font-family: "JetBrains Mono", ui-monospace, SFMono-Regular, Menlo, monospace; font-weight: 400; }
.strip { display: flex; gap: 2px; height: 8px; }
.seg { height: 100%; border-radius: 2px; min-width: 2px; }
.seg-sql { background: #3987e5; } .seg-cache { background: #d95926; } .seg-search { background: #199e70; } .seg-http { background: #c98500; } .seg-php { background: #d55181; }
.legend { display: flex; flex-wrap: wrap; gap: 4px 12px; color: #8B96A3; font-size: 11px; }
.legend span::before { content: ""; display: inline-block; width: 8px; height: 8px; margin-right: 4px; border-radius: 2px; background: var(--c); vertical-align: -1px; }
.lg-sql { --c: #3987e5; } .lg-cache { --c: #d95926; } .lg-search { --c: #199e70; } .lg-http { --c: #c98500; } .lg-php { --c: #d55181; }
.label { color: #8B96A3; font-size: 11px; text-transform: uppercase; letter-spacing: 0.06em; }
ol { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; gap: 2px; }
.item { display: grid; grid-template-columns: 20px 1fr auto; gap: 8px; align-items: center; padding: 6px; border-radius: 4px; color: inherit; text-decoration: none; }
a.item:hover { background: #1E252D; }
.rank { display: inline-grid; place-items: center; width: 20px; height: 20px; border-radius: 4px; font-size: 11px; font-weight: 600; }
.heat-1 { background: #3A2A22; } .heat-2 { background: #7A3B26; } .heat-3 { background: #C2502E; } .heat-4 { background: #FF6A3D; color: #101418; }
.what { min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-size: 12px; }
.cost { color: #C3CAD2; white-space: nowrap; }
.repeat { color: #C3CAD2; font-size: 11px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.why { color: #C3CAD2; }
.foot { display: flex; gap: 8px; align-items: center; justify-content: space-between; border-top: 1px solid #28313B; padding-top: 10px; }
.foot a { color: #6CB6FF; text-decoration: none; }
.foot a:hover { text-decoration: underline; }
.ghost { border: 0; background: none; color: #8B96A3; font: inherit; cursor: pointer; padding: 4px; }
.ghost:hover { color: #E4E8EC; }
.empty { color: #8B96A3; }
@media (prefers-reduced-motion: no-preference) { .pop { animation: rise 120ms ease-out; } @keyframes rise { from { opacity: 0; transform: translateY(4px); } } }
`;

  function main(win) {
    const doc = win.document;
    if (win.top !== win.self || isInspectorHost(win.location.host, DOMAIN)) return;
    const base = inspectorBase(DOMAIN);
    const host = win.location.host;
    const gm = typeof GM_xmlhttpRequest === 'function';

    function load(key, def) {
      try {
        if (typeof GM_getValue === 'function') return GM_getValue(key, def);
        const v = win.localStorage.getItem('roll-inspector:' + key);
        return v === null ? def : JSON.parse(v);
      } catch (e) {
        return def;
      }
    }

    function save(key, value) {
      try {
        if (typeof GM_setValue === 'function') GM_setValue(key, value);
        else win.localStorage.setItem('roll-inspector:' + key, JSON.stringify(value));
      } catch (e) {
        // A blocked storage only means the choice is not remembered.
      }
    }

    // GM_xmlhttpRequest needs no CORS; plain fetch relies on Inspector's
    // Access-Control-Allow-Origin: * on /api/v1.
    function getJSON(url) {
      if (gm) {
        return new Promise((resolve, reject) => {
          GM_xmlhttpRequest({
            method: 'GET',
            url,
            anonymous: true,
            timeout: 5000,
            onload: (r) => {
              try {
                resolve({ status: r.status, body: JSON.parse(r.responseText) });
              } catch (e) {
                reject(new Error('Inspector answered ' + r.status + ' without JSON'));
              }
            },
            onerror: () => reject(new Error('Inspector is not reachable at ' + base)),
            ontimeout: () => reject(new Error('Inspector did not answer within 5 s')),
          });
        });
      }
      return win.fetch(url, { credentials: 'omit', cache: 'no-store' }).then((r) => r.json().then((body) => ({ status: r.status, body })));
    }

    function el(tag, cls, text) {
      const n = doc.createElement(tag);
      if (cls) n.className = cls;
      if (text !== undefined && text !== null) n.textContent = String(text);
      return n;
    }

    let mount = null;
    let shadow = null;
    let badge = null;
    let label = null;
    let pop = null;
    let page = null;

    function build() {
      mount = el('div');
      mount.id = 'roll-inspector-badge';
      shadow = mount.attachShadow({ mode: 'open' });
      if ('adoptedStyleSheets' in shadow && typeof CSSStyleSheet === 'function' && CSSStyleSheet.prototype.replaceSync) {
        const sheet = new CSSStyleSheet();
        sheet.replaceSync(CSS);
        shadow.adoptedStyleSheets = [sheet];
      } else {
        shadow.appendChild(el('style', '', CSS));
      }
      const root = el('div', 'root');
      pop = el('div', 'pop');
      pop.hidden = true;
      pop.id = 'pop';
      pop.setAttribute('role', 'dialog');
      pop.setAttribute('aria-label', 'Roll Inspector');
      badge = el('button', 'badge tone-none');
      badge.type = 'button';
      badge.setAttribute('aria-expanded', 'false');
      badge.setAttribute('aria-controls', 'pop');
      badge.title = 'Roll Inspector: how this page was served';
      badge.appendChild(el('span', 'dot'));
      label = el('span', '', 'Inspector · waiting…');
      badge.appendChild(label);
      badge.addEventListener('click', toggle);
      root.appendChild(pop);
      root.appendChild(badge);
      shadow.appendChild(root);
      doc.documentElement.appendChild(mount);
      doc.addEventListener('keydown', (e) => {
        if (e.key === 'Escape' && !pop.hidden) close(true);
      });
      doc.addEventListener('click', (e) => {
        if (!pop.hidden && !e.composedPath().includes(mount)) close(false);
      });
    }

    function setTone(tone) {
      badge.className = 'badge tone-' + tone;
    }

    function toggle() {
      if (pop.hidden) open();
      else close(true);
    }

    function open() {
      render();
      pop.hidden = false;
      badge.setAttribute('aria-expanded', 'true');
    }

    function close(focus) {
      pop.hidden = true;
      badge.setAttribute('aria-expanded', 'false');
      if (focus) badge.focus();
    }

    function link(href, text, cls) {
      const a = el('a', cls || '', text);
      a.href = href;
      a.target = '_blank';
      a.rel = 'noopener';
      return a;
    }

    function hide() {
      save(hideKey(host), true);
      mount.remove();
    }

    function render() {
      while (pop.firstChild) pop.removeChild(pop.firstChild);
      const headRow = el('div', 'head');
      headRow.appendChild(el('span', 'title', page ? badgeText(page) : 'Roll Inspector'));
      const sub = page && page.trace ? page.trace.route : '';
      if (sub) headRow.appendChild(el('span', 'sub mono', sub));
      pop.appendChild(headRow);

      if (!page) {
        pop.appendChild(el('p', 'empty', label.textContent));
      } else {
        if (page.why && page.why.value) pop.appendChild(el('div', 'why', page.why.label + ' ' + page.why.value));
        const segs = segments(page.trace && page.trace.times);
        if (segs.length) {
          const strip = el('div', 'strip');
          strip.setAttribute('role', 'img');
          strip.setAttribute('aria-label', segs.map(segmentTitle).join(', '));
          for (const s of segs) {
            const seg = el('span', 'seg seg-' + s.key);
            seg.style.width = s.width + '%';
            seg.title = segmentTitle(s);
            strip.appendChild(seg);
          }
          pop.appendChild(strip);
          const legend = el('div', 'legend');
          for (const s of segs) legend.appendChild(el('span', 'lg-' + s.key, s.label + ' ' + formatMs(s.ms)));
          pop.appendChild(legend);
        }
        const items = topItems(page, 3);
        if (items.length) {
          pop.appendChild(el('div', 'label', 'Where the time went'));
          const list = el('ol');
          for (const it of items) {
            const li = el('li');
            const row = it.href ? link(it.href, null, 'item') : el('div', 'item');
            row.title = it.label;
            row.appendChild(el('span', 'rank heat-' + it.heat, it.rank));
            row.appendChild(el('span', 'what mono', it.label));
            row.appendChild(el('span', 'cost', it.self + ' · ' + it.share + (it.calls ? ' · ' + it.calls : '')));
            li.appendChild(row);
            list.appendChild(li);
          }
          pop.appendChild(list);
        } else if (!page.trace) {
          pop.appendChild(el('p', 'empty', handlingOf(page) === 'HIT' ? 'Served from the Varnish cache, so PHP did not run.' : 'No PHP trace for this request. Switch tracing on with roll inspector trace on.'));
        }
        for (const r of (page.repeats || []).slice(0, 2)) {
          const line = el('div', 'repeat mono', 'N+1 ' + repeatLine(r));
          line.title = r.example || r.key;
          pop.appendChild(line);
        }
      }

      const foot = el('div', 'foot');
      const target = (page && page.links && (page.links.bottlenecks || page.links.request)) || base;
      foot.appendChild(link(target, page && page.links && page.links.bottlenecks ? 'Open the trace in Inspector' : 'Open Inspector'));
      const hideBtn = el('button', 'ghost', 'Hide on this site');
      hideBtn.type = 'button';
      hideBtn.addEventListener('click', hide);
      foot.appendChild(hideBtn);
      pop.appendChild(foot);
    }

    function show(p) {
      page = p;
      label.textContent = badgeText(p);
      setTone(toneOf(p));
      if (!pop.hidden) render();
    }

    function wait(ms) {
      return new Promise((r) => win.setTimeout(r, ms));
    }

    async function poll() {
      const since = win.performance && win.performance.timeOrigin;
      const url = pageURL(base, win.location.href, since);
      const steps = delays(BACKOFF.length);
      for (let i = 0; i < steps.length; i++) {
        await wait(steps[i]);
        let res;
        try {
          res = await getJSON(url);
        } catch (e) {
          label.textContent = 'Inspector · offline';
          badge.title = e.message;
          return;
        }
        if (res.status === 200) {
          show(res.body);
          // A trace can join a Varnish row a moment later; one more look
          // picks that up without polling forever.
          if (!res.body.trace && i < steps.length - 1) continue;
          return;
        }
        label.textContent = 'Inspector · ' + waitingText(i + 1, steps.length);
        badge.title = (res.body && (res.body.hint || res.body.error)) || 'Inspector answered ' + res.status;
      }
    }

    if (typeof GM_registerMenuCommand === 'function') {
      GM_registerMenuCommand('Show the Inspector badge on ' + host, () => {
        save(hideKey(host), false);
        if (!mount || !mount.isConnected) {
          build();
          poll();
        }
      });
    }
    if (load(hideKey(host), false)) return;
    build();
    poll();
  }

  return {
    DOMAIN,
    BACKOFF,
    inspectorBase,
    isInspectorHost,
    pageURL,
    delays,
    formatMs,
    handlingOf,
    toneOf,
    badgeText,
    segments,
    segmentTitle,
    topItems,
    heatOf,
    repeatLine,
    hideKey,
    waitingText,
    main,
  };
});
