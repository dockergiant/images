(function (root, factory) {
  const mod = factory();
  if (typeof module === 'object' && module.exports) {
    module.exports = mod;
  } else {
    root.Inspector = root.Inspector || {};
    root.Inspector.charts = mod;
  }
})(this, function () {
  const SVG_NS = 'http://www.w3.org/2000/svg';
  const SLOTS = 8;
  const EMPTY_TEXT = 'Collecting: the first points arrive within 10 s';
  const WINDOWS = { '5m': 300, '15m': 900, '60m': 3600 };
  const TIME_STEPS = [10, 15, 30, 60, 120, 300, 600, 900, 1800, 3600];
  // Average advance of IBM Plex Sans and JetBrains Mono per px of font size, so
  // layout can be decided before the text is in the document.
  const SANS_EM = 0.56;
  const MONO_EM = 0.6;
  const GAP = 2;
  const RADIUS = 4;
  const groups = new Map();

  function isNum(n) {
    return typeof n === 'number' && Number.isFinite(n);
  }

  function trimZero(s) {
    return s.indexOf('.') >= 0 ? s.replace(/\.?0+$/, '') : s;
  }

  function compact(v) {
    if (!isNum(v)) return '';
    const a = Math.abs(v);
    const sign = v < 0 ? '-' : '';
    const units = [
      [1e9, 'B'],
      [1e6, 'M'],
      [1e3, 'K'],
    ];
    for (const [base, suffix] of units) {
      if (a >= base) {
        const x = a / base;
        return sign + (x < 10 ? trimZero(x.toFixed(1)) : String(Math.round(x))) + suffix;
      }
    }
    if (a === 0) return '0';
    if (a < 10 && !Number.isInteger(a)) return sign + trimZero(a.toFixed(a < 1 ? 2 : 1));
    return sign + String(Math.round(a));
  }

  function duration(ms) {
    if (!isNum(ms)) return '';
    if (ms === 0) return '0 ms';
    if (ms < 1) return trimZero(ms.toFixed(2)) + ' ms';
    if (ms < 1000) return Math.round(ms) + ' ms';
    return trimZero((ms / 1000).toFixed(ms < 10000 ? 1 : 0)) + ' s';
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
    return (v >= 10 ? String(Math.round(v)) : trimZero(v.toFixed(1))) + ' ' + units[i];
  }

  function pct(ratio, digits) {
    if (!isNum(ratio)) return '';
    return trimZero((ratio * 100).toFixed(digits)) + '%';
  }

  const UNITS = {
    count: { value: compact, tick: compact },
    rate: { value: (v) => compact(v) + '/s', tick: compact },
    rpm: { value: (v) => compact(v) + '/min', tick: compact },
    ms: { value: duration, tick: duration },
    bytes: { value: bytes, tick: bytes },
    pct: { value: (v) => pct(v, 1), tick: (v) => pct(v, 0) },
  };

  function unitOf(opts) {
    const u = UNITS[opts.unit] || UNITS.count;
    return {
      value: typeof opts.format === 'function' ? opts.format : u.value,
      tick: typeof opts.formatTick === 'function' ? opts.formatTick : u.tick,
    };
  }

  function formatValue(unit, v) {
    if (!isNum(v)) return '–';
    return (UNITS[unit] || UNITS.count).value(v);
  }

  function niceStep(raw) {
    const mag = Math.pow(10, Math.floor(Math.log10(raw)));
    const n = raw / mag;
    const nice = n <= 1 ? 1 : n <= 2 ? 2 : n <= 2.5 ? 2.5 : n <= 5 ? 5 : 10;
    return nice * mag;
  }

  // Ticks always start at the zero baseline, so bars and areas are never cut.
  function niceTicks(max, count) {
    if (!isNum(max) || max <= 0) max = 1;
    const step = niceStep(max / Math.max(1, count || 4));
    const top = Math.ceil(max / step - 1e-9) * step;
    const out = [];
    for (let v = 0; v <= top + step / 2; v += step) out.push(Number(v.toPrecision(12)));
    return out;
  }

  function yTicks(unit, max, count) {
    if (unit === 'bytes' && isNum(max) && max >= 1024) {
      let base = 1;
      while (max / base >= 1024 && base < Math.pow(1024, 4)) base *= 1024;
      return niceTicks(max / base, count).map((t) => t * base);
    }
    if (unit === 'pct' && isNum(max) && max <= 1 && max > 0.5) return [0, 0.25, 0.5, 0.75, 1];
    const ticks = niceTicks(max, count);
    // Counts are whole things: a connection count never reads 0.25.
    if (unit === 'count' && ticks[1] < 1) {
      const out = [];
      for (let v = 0; v <= Math.max(1, Math.ceil(isNum(max) ? max : 0)); v++) out.push(v);
      return out;
    }
    return ticks;
  }

  function pad2(n) {
    return n < 10 ? '0' + n : String(n);
  }

  function clock(t, seconds) {
    const d = new Date(t * 1000);
    const s = pad2(d.getHours()) + ':' + pad2(d.getMinutes());
    return seconds ? s + ':' + pad2(d.getSeconds()) : s;
  }

  function timeTicks(start, end, width) {
    const span = end - start;
    if (!(span > 0)) return [];
    const most = Math.max(2, Math.floor((width || 480) / 72));
    let step = TIME_STEPS[TIME_STEPS.length - 1];
    for (const s of TIME_STEPS) {
      if (span / s <= most) {
        step = s;
        break;
      }
    }
    const out = [];
    for (let t = Math.ceil(start / step) * step; t <= end; t += step) out.push({ t, label: clock(t, step < 60) });
    return out;
  }

  function windowSeconds(w) {
    if (isNum(w)) return w;
    return WINDOWS[w] || 0;
  }

  function scale(d0, d1, r0, r1) {
    const k = d1 === d0 ? 0 : (r1 - r0) / (d1 - d0);
    return (v) => r0 + (v - d0) * k;
  }

  function r1(n) {
    return Math.round(n * 10) / 10;
  }

  function linePath(points) {
    let d = '';
    let pen = false;
    for (const p of points) {
      if (p[1] === null || !isNum(p[1])) {
        pen = false;
        continue;
      }
      d += (pen ? 'L' : 'M') + r1(p[0]) + ' ' + r1(p[1]);
      pen = true;
    }
    return d;
  }

  // One closed shape per run of defined points, between lower and upper.
  function areaPath(xs, lower, upper) {
    let d = '';
    let run = [];
    const flush = () => {
      if (run.length > 1) {
        d += 'M' + run.map((i) => r1(xs[i]) + ' ' + r1(upper[i])).join('L');
        d += 'L' + run.slice().reverse().map((i) => r1(xs[i]) + ' ' + r1(lower[i])).join('L') + 'Z';
      }
      run = [];
    };
    for (let i = 0; i < xs.length; i++) {
      if (isNum(upper[i]) && isNum(lower[i])) run.push(i);
      else flush();
    }
    flush();
    return d;
  }

  // A horizontal bar: square at the baseline (x), rounded at the data end.
  function barPathH(x, y, w, h, r) {
    if (!(w > 0) || !(h > 0)) return '';
    r = Math.max(0, Math.min(r === undefined ? RADIUS : r, h / 2, w));
    if (!r) return 'M' + r1(x) + ' ' + r1(y) + 'H' + r1(x + w) + 'V' + r1(y + h) + 'H' + r1(x) + 'Z';
    return 'M' + r1(x) + ' ' + r1(y) + 'H' + r1(x + w - r) + 'A' + r + ' ' + r + ' 0 0 1 ' + r1(x + w) + ' ' + r1(y + r) +
      'V' + r1(y + h - r) + 'A' + r + ' ' + r + ' 0 0 1 ' + r1(x + w - r) + ' ' + r1(y + h) + 'H' + r1(x) + 'Z';
  }

  // A column: square at the baseline (bottom), rounded at the top.
  function barPathV(x, y, w, h, r) {
    if (!(w > 0) || !(h > 0)) return '';
    r = Math.max(0, Math.min(r === undefined ? RADIUS : r, w / 2, h));
    if (!r) return 'M' + r1(x) + ' ' + r1(y + h) + 'V' + r1(y) + 'H' + r1(x + w) + 'V' + r1(y + h) + 'Z';
    return 'M' + r1(x) + ' ' + r1(y + h) + 'V' + r1(y + r) + 'A' + r + ' ' + r + ' 0 0 1 ' + r1(x + r) + ' ' + r1(y) +
      'H' + r1(x + w - r) + 'A' + r + ' ' + r + ' 0 0 1 ' + r1(x + w) + ' ' + r1(y + r) + 'V' + r1(y + h) + 'Z';
  }

  // Colliding end labels are dropped, never nudged: a nudged label drifts off its line.
  function placeLabels(cands, minGap) {
    const order = cands.map((c, i) => i).sort((a, b) => (cands[b].priority || 0) - (cands[a].priority || 0));
    const kept = [];
    for (const i of order) {
      if (kept.every((k) => Math.abs(cands[k].y - cands[i].y) >= minGap)) kept.push(i);
    }
    return kept.sort((a, b) => a - b);
  }

  function nearestIndex(times, t) {
    if (!times.length) return -1;
    let lo = 0;
    let hi = times.length - 1;
    while (hi - lo > 1) {
      const mid = (lo + hi) >> 1;
      if (times[mid] <= t) lo = mid;
      else hi = mid;
    }
    return Math.abs(times[hi] - t) < Math.abs(times[lo] - t) ? hi : lo;
  }

  function slotOf(def, i) {
    if (def && def.slot === 'other') return 'other';
    const s = def && Number(def.slot);
    if (s >= 1 && s <= SLOTS) return s;
    return i < SLOTS ? i + 1 : 'other';
  }

  function seriesDefs(opts, keys) {
    const defs = opts.series && opts.series.length ? opts.series : keys.map((k) => ({ key: k }));
    return defs.map((d, i) => ({ key: d.key, label: d.label || d.key, slot: slotOf(d, i) }));
  }

  function seriesMap(data) {
    if (!data) return {};
    if (data.series && !Array.isArray(data.series)) return data.series;
    return data;
  }

  // Joins every series on one sorted time axis inside [end - window, end]. The
  // end is the newest sample of any series unless the caller pins it, so charts
  // fed from the same sampler share an axis.
  function prepTimeseries(data, defs, opts) {
    const map = seriesMap(data);
    const win = windowSeconds(opts.window);
    let end = isNum(opts.end) ? opts.end : -Infinity;
    if (!isNum(opts.end)) {
      for (const d of defs) for (const p of map[d.key] || []) if (isNum(p[0]) && p[0] > end) end = p[0];
    }
    if (!isNum(end)) return { times: [], values: defs.map(() => []), start: 0, end: 0 };
    const start = win > 0 ? end - win : Infinity;
    const index = new Map();
    for (const d of defs) {
      for (const p of map[d.key] || []) {
        if (!isNum(p[0]) || p[0] > end || (win > 0 && p[0] < start)) continue;
        index.set(p[0], true);
      }
    }
    const times = Array.from(index.keys()).sort((a, b) => a - b);
    const pos = new Map(times.map((t, i) => [t, i]));
    const values = defs.map((d) => {
      const out = new Array(times.length).fill(null);
      for (const p of map[d.key] || []) {
        const i = pos.get(p[0]);
        if (i !== undefined && isNum(p[1])) out[i] = p[1];
      }
      return out;
    });
    return { times, values, start: win > 0 ? start : times[0], end };
  }

  function stackValues(values) {
    const n = values.length ? values[0].length : 0;
    const lower = [];
    const upper = [];
    const acc = new Array(n).fill(0);
    for (const row of values) {
      const lo = acc.slice();
      for (let i = 0; i < n; i++) acc[i] += isNum(row[i]) && row[i] > 0 ? row[i] : 0;
      lower.push(lo);
      upper.push(acc.slice());
    }
    return { lower, upper, totals: acc };
  }

  // Ranked rows keep the top limit - 1 and fold the tail into one "Other" row,
  // so the chart never needs a ninth anything.
  function foldRows(rows, limit, sort) {
    const list = rows.filter((r) => r && isNum(r.value));
    if (sort !== false) list.sort((a, b) => b.value - a.value);
    if (!limit || list.length <= limit) return list;
    const head = list.slice(0, limit - 1);
    const tail = list.slice(limit - 1);
    head.push({ key: '__other', label: 'Other (' + tail.length + ')', value: tail.reduce((s, r) => s + r.value, 0), other: true });
    return head;
  }

  function ringDash(value, circumference) {
    const v = Math.max(0, Math.min(1, isNum(value) ? value : 0));
    return { len: v * circumference, full: v >= 0.9995, none: v <= 0 };
  }

  function textWidth(s, size, mono) {
    return String(s).length * size * (mono ? MONO_EM : SANS_EM);
  }

  function fit(s, px, size, mono) {
    s = String(s === undefined || s === null ? '' : s);
    if (textWidth(s, size, mono) <= px) return s;
    const n = Math.max(1, Math.floor(px / (size * (mono ? MONO_EM : SANS_EM))) - 1);
    return s.slice(0, n) + '…';
  }

  function reducedMotion() {
    try {
      return typeof matchMedia === 'function' && matchMedia('(prefers-reduced-motion: reduce)').matches;
    } catch (e) {
      return false;
    }
  }

  // DOM helpers. Everything goes through createElementNS, setAttribute and
  // textContent: the CSP blocks inline styles, and labels are untrusted.
  function svg(doc, tag, attrs, cls) {
    const n = doc.createElementNS(SVG_NS, tag);
    if (attrs) for (const k in attrs) if (attrs[k] !== undefined && attrs[k] !== null) n.setAttribute(k, String(attrs[k]));
    if (cls) n.setAttribute('class', cls);
    return n;
  }

  function node(doc, tag, cls, text) {
    const n = doc.createElement(tag);
    if (cls) n.className = cls;
    if (text !== undefined && text !== null) n.textContent = String(text);
    return n;
  }

  function slotClass(slot) {
    return 'chart-s' + slot;
  }

  function createChart(el, opts, form) {
    opts = Object.assign({}, form.defaults || {}, opts || {});
    const doc = el.ownerDocument;
    const listeners = [];
    const drawListeners = [];
    const state = { data: undefined, table: !!opts.table, width: 0, destroyed: false, frame: 0, hover: null };
    const unit = unitOf(opts);

    function on(list, target, type, fn) {
      target.addEventListener(type, fn);
      list.push([target, type, fn]);
    }

    function off(list) {
      while (list.length) {
        const [t, type, fn] = list.pop();
        t.removeEventListener(type, fn);
      }
    }

    el.classList.add('chart');
    if (form.cls) el.classList.add(form.cls);
    const head = node(doc, 'div', 'chart-head');
    const titles = node(doc, 'div', 'chart-titles');
    if (opts.title) {
      const h = node(doc, 'h3', 'chart-title', opts.title);
      titles.appendChild(h);
    }
    if (opts.subtitle) titles.appendChild(node(doc, 'p', 'chart-subtitle', opts.subtitle));
    head.appendChild(titles);
    let toggle = null;
    if (form.tableable !== false && opts.tableToggle !== false) {
      toggle = node(doc, 'button', 'btn btn-ghost btn-sm chart-toggle');
      toggle.setAttribute('type', 'button');
      head.appendChild(toggle);
      on(listeners, toggle, 'click', () => {
        const next = !state.table;
        if (typeof opts.onTableToggle === 'function') opts.onTableToggle(next);
        else api.table(next);
      });
    }
    if (opts.title || opts.subtitle || toggle) el.appendChild(head);
    const legend = node(doc, 'ul', 'chart-legend');
    el.appendChild(legend);
    const body = node(doc, 'div', 'chart-body');
    el.appendChild(body);
    const tip = node(doc, 'div', 'chart-tip');
    tip.setAttribute('aria-hidden', 'true');
    el.appendChild(tip);
    const live = node(doc, 'span', 'sr-only-text');
    live.setAttribute('aria-live', 'polite');
    el.appendChild(live);

    const tipApi = {
      show(svgEl, x, y, content, below) {
        tip.replaceChildren();
        if (content.title) tip.appendChild(node(doc, 'div', 'chart-tip-title', content.title));
        for (const row of content.rows || []) {
          const r = node(doc, 'div', 'chart-tip-row');
          if (row.slot !== undefined) r.appendChild(node(doc, 'span', 'chart-key chart-key-line ' + slotClass(row.slot)));
          r.appendChild(node(doc, 'span', 'chart-tip-value', row.value));
          r.appendChild(node(doc, 'span', 'chart-tip-label', row.label));
          tip.appendChild(r);
        }
        if (content.note) tip.appendChild(node(doc, 'div', 'chart-tip-note', content.note));
        tip.classList.add('is-open');
        const er = el.getBoundingClientRect();
        const sr = svgEl.getBoundingClientRect();
        const px = sr.left - er.left + x;
        const py = sr.top - er.top + y;
        const tw = tip.offsetWidth || 160;
        const th = tip.offsetHeight || 40;
        let left;
        let top;
        if (below) {
          // Under the row, so the tooltip never covers the label it explains.
          left = Math.max(0, Math.min(px - tw / 2, er.width - tw));
          top = py + 10;
          if (top + th > er.height) top = py - th - 30;
        } else {
          left = px + 14;
          if (left + tw > er.width) left = px - 14 - tw;
          left = Math.max(0, left);
          top = Math.max(0, Math.min(py - th / 2, er.height - th));
        }
        // CSSOM, not a style attribute: the CSP allows the first and blocks the second.
        tip.style.left = Math.round(left) + 'px';
        tip.style.top = Math.round(top) + 'px';
      },
      hide() {
        tip.classList.remove('is-open');
      },
      announce(text) {
        live.textContent = text;
      },
    };

    // The body is a stretched block, so its width is the host's content box
    // without the padding a panel adds.
    function measure() {
      const w = body.clientWidth || el.clientWidth || 0;
      return Math.floor(w);
    }

    function drawLegend(items) {
      legend.replaceChildren();
      legend.classList.toggle('is-empty', !items.length);
      for (const it of items) {
        const li = node(doc, 'li', 'chart-legend-item');
        li.appendChild(node(doc, 'span', 'chart-key chart-key-' + (it.mark || 'rect') + ' ' + slotClass(it.slot)));
        li.appendChild(node(doc, 'span', '', it.label));
        legend.appendChild(li);
      }
    }

    function drawTable(t) {
      const wrap = node(doc, 'div', 'chart-table-wrap');
      const table = node(doc, 'table', 'table chart-table');
      if (opts.title) table.appendChild(node(doc, 'caption', 'sr-only-text', opts.title));
      const thead = node(doc, 'thead');
      const hr = node(doc, 'tr');
      for (const c of t.columns) {
        const th = node(doc, 'th', c.num ? 'num' : '', c.label);
        th.setAttribute('scope', 'col');
        hr.appendChild(th);
      }
      thead.appendChild(hr);
      table.appendChild(thead);
      const tbody = node(doc, 'tbody');
      for (const row of t.rows) {
        const tr = node(doc, 'tr');
        row.forEach((cell, i) => tr.appendChild(node(doc, 'td', t.columns[i] && t.columns[i].num ? 'num' : '', cell)));
        tbody.appendChild(tr);
      }
      table.appendChild(tbody);
      wrap.appendChild(table);
      body.appendChild(wrap);
    }

    function render() {
      state.frame = 0;
      if (state.destroyed) return;
      off(drawListeners);
      tipApi.hide();
      body.replaceChildren();
      const ctx = { doc, opts, unit, data: state.data, width: state.width, tip: tipApi, state, el, chart: api };
      ctx.on = (target, type, fn) => on(drawListeners, target, type, fn);
      drawLegend(form.legend ? form.legend(ctx) : []);
      if (toggle) {
        toggle.textContent = state.table ? 'Chart' : 'Table';
        toggle.setAttribute('aria-pressed', state.table ? 'true' : 'false');
      }
      const empty = state.data === undefined || state.data === null || form.empty(ctx);
      el.classList.toggle('is-empty', empty);
      if (empty) {
        const e = node(doc, 'p', 'chart-empty', opts.emptyText || EMPTY_TEXT);
        body.appendChild(e);
        if (form.afterEmpty) form.afterEmpty(ctx);
        return;
      }
      if (state.table) {
        drawTable(form.table(ctx));
        return;
      }
      if (!state.width) return;
      form.draw(ctx, body);
    }

    function schedule() {
      if (state.frame || state.destroyed) return;
      if (typeof requestAnimationFrame === 'function') state.frame = requestAnimationFrame(render);
      else render();
    }

    let ro = null;
    if (typeof ResizeObserver === 'function') {
      ro = new ResizeObserver(() => {
        const w = measure();
        if (w && w !== state.width) {
          state.width = w;
          schedule();
        }
      });
      ro.observe(el);
    }

    const api = {
      update(data, more) {
        if (state.destroyed) return api;
        if (more && typeof more === 'object') {
          if ('table' in more) state.table = !!more.table;
          if ('window' in more) opts.window = more.window;
          if ('end' in more) opts.end = more.end;
          if ('emptyText' in more) opts.emptyText = more.emptyText;
        }
        state.data = data;
        state.width = measure() || state.width;
        render();
        return api;
      },
      table(on) {
        if (state.destroyed) return api;
        state.table = !!on;
        render();
        return api;
      },
      stale(on) {
        el.classList.toggle('is-stale', !!on);
        return api;
      },
      resize() {
        state.width = measure();
        render();
        return api;
      },
      destroy() {
        if (state.destroyed) return;
        state.destroyed = true;
        if (state.frame && typeof cancelAnimationFrame === 'function') cancelAnimationFrame(state.frame);
        if (ro) ro.disconnect();
        off(drawListeners);
        off(listeners);
        if (form.destroy) form.destroy(state);
        el.replaceChildren();
        el.classList.remove('chart', 'is-empty', 'is-stale');
        if (form.cls) el.classList.remove(form.cls);
      },
    };
    state.width = measure();
    if (opts.data !== undefined) api.update(opts.data);
    else render();
    return api;
  }

  // Shared plot frame for every form with a y axis.
  function yAxis(ctx, g, ys, ticks, left, right) {
    const { doc, unit } = ctx;
    for (const t of ticks) {
      const y = r1(ys(t)) + 0.5;
      g.appendChild(svg(doc, 'line', { x1: left, x2: right, y1: y, y2: y }, t === 0 ? 'chart-baseline' : 'chart-grid'));
      const label = svg(doc, 'text', { x: left - 8, y, dy: '0.32em', 'text-anchor': 'end' }, 'chart-axis');
      label.textContent = unit.tick(t);
      g.appendChild(label);
    }
  }

  function tickGutter(ctx, ticks) {
    let w = 0;
    for (const t of ticks) w = Math.max(w, textWidth(ctx.unit.tick(t), 11));
    return Math.ceil(w) + 12;
  }

  function hoverTarget(svgEl, ctx, evt) {
    const r = svgEl.getBoundingClientRect();
    return { x: evt.clientX - r.left, y: evt.clientY - r.top };
  }

  // ---- timeseries -------------------------------------------------------

  function tsDefs(ctx) {
    return seriesDefs(ctx.opts, Object.keys(seriesMap(ctx.data)));
  }

  const timeseriesForm = {
    cls: 'chart-timeseries',
    defaults: { mode: 'line', height: 160, window: '15m' },
    legend(ctx) {
      const defs = tsDefs(ctx);
      if (defs.length < 2) return [];
      return defs.map((d) => ({ label: d.label, slot: d.slot, mark: ctx.opts.mode === 'stack' ? 'rect' : 'line' }));
    },
    empty(ctx) {
      const prep = prepTimeseries(ctx.data, tsDefs(ctx), ctx.opts);
      return prep.times.length < 2;
    },
    table(ctx) {
      const defs = tsDefs(ctx);
      const prep = prepTimeseries(ctx.data, defs, ctx.opts);
      const columns = [{ label: 'Time' }].concat(defs.map((d) => ({ label: d.label, num: true })));
      const rows = [];
      for (let i = prep.times.length - 1; i >= 0; i--) {
        rows.push([clock(prep.times[i], true)].concat(prep.values.map((v) => (isNum(v[i]) ? ctx.unit.value(v[i]) : '–'))));
      }
      return { columns, rows };
    },
    draw(ctx, body) {
      const { doc, opts, width, unit } = ctx;
      const defs = tsDefs(ctx);
      const prep = prepTimeseries(ctx.data, defs, opts);
      const stacked = opts.mode === 'stack';
      const st = stacked ? stackValues(prep.values) : null;
      let max = 0;
      if (stacked) for (const v of st.totals) max = Math.max(max, v);
      else for (const row of prep.values) for (const v of row) if (isNum(v)) max = Math.max(max, v);
      if (isNum(opts.yMax)) max = Math.max(max, opts.yMax);
      const H = opts.height;
      const top = 10;
      const bottom = 24;
      const ticks = yTicks(opts.unit, max, H >= 140 ? 4 : 3);
      const left = tickGutter(ctx, ticks);
      const wantLabels = opts.labels !== false && defs.length <= 4 && width >= 360;
      let gutter = 12;
      let names = false;
      const lastIdx = prep.times.length - 1;
      if (wantLabels) {
        let full = 0;
        let values = 0;
        defs.forEach((d, i) => {
          const v = unit.value(lastValue(prep.values[i]));
          full = Math.max(full, textWidth(d.label, 11) + textWidth(v, 12) + 6);
          values = Math.max(values, textWidth(v, 12));
        });
        // A cut series name reads worse than none: the legend already names it,
        // so a narrow chart keeps only the values at the line ends.
        names = full + 26 <= width * 0.3;
        gutter = Math.ceil(names ? full : values) + 26;
      }
      const right = width - gutter;
      const xs = scale(prep.start, prep.end, left, right);
      const ys = scale(0, ticks[ticks.length - 1], top + H, top);
      const root = svg(doc, 'svg', { width, height: top + H + bottom, viewBox: '0 0 ' + width + ' ' + (top + H + bottom) }, 'chart-svg');
      root.setAttribute('role', 'img');
      root.setAttribute('tabindex', '0');
      const frame = svg(doc, 'g', null, 'chart-frame');
      yAxis(ctx, frame, ys, ticks, left, right);
      const tt = timeTicks(prep.start, prep.end, right - left);
      for (const t of tt) {
        const x = xs(t.t);
        const label = svg(doc, 'text', { x: r1(x), y: top + H + 17, 'text-anchor': x - left < 18 ? 'start' : right - x < 18 ? 'end' : 'middle' }, 'chart-axis');
        label.textContent = t.label;
        frame.appendChild(label);
      }
      root.appendChild(frame);

      const px = prep.times.map((t) => xs(t));
      const marks = svg(doc, 'g', null, 'chart-marks');
      const labelCands = [];
      if (stacked) {
        defs.forEach((d, i) => {
          const lo = st.lower[i].map(ys);
          const hi = st.upper[i].map(ys);
          marks.appendChild(svg(doc, 'path', { d: areaPath(px, lo, hi) }, 'chart-band ' + slotClass(d.slot)));
        });
        defs.forEach((d, i) => {
          marks.appendChild(svg(doc, 'path', { d: linePath(px.map((x, j) => [x, ys(st.upper[i][j])])) }, 'chart-line chart-edge ' + slotClass(d.slot)));
          const v = prep.values[i][lastIdx];
          const mid = (ys(st.lower[i][lastIdx]) + ys(st.upper[i][lastIdx])) / 2;
          labelCands.push({ y: mid, priority: isNum(v) ? v : 0, def: d, value: v });
        });
      } else {
        defs.forEach((d, i) => {
          const pts = px.map((x, j) => [x, isNum(prep.values[i][j]) ? ys(prep.values[i][j]) : null]);
          if (defs.length === 1) {
            const base = px.map(() => ys(0));
            const upper = pts.map((p) => p[1]);
            marks.appendChild(svg(doc, 'path', { d: areaPath(px, base, upper) }, 'chart-area ' + slotClass(d.slot)));
          }
          marks.appendChild(svg(doc, 'path', { d: linePath(pts) }, 'chart-line ' + slotClass(d.slot)));
          const li = lastDefined(prep.values[i]);
          if (li >= 0) {
            const dot = svg(doc, 'circle', { cx: r1(px[li]), cy: r1(pts[li][1]), r: 4 }, 'chart-dot ' + slotClass(d.slot));
            marks.appendChild(dot);
            labelCands.push({ y: pts[li][1], priority: prep.values[i][li], def: d, value: prep.values[i][li], dot });
          }
        });
      }
      root.appendChild(marks);

      // A line resting on the baseline looks like a chart that did not draw,
      // and the series under it are hidden, so the chart says it in words.
      const allZero = prep.values.every((row) => row.every((v) => !isNum(v) || v === 0));
      if (allZero) {
        const note = svg(doc, 'text', { x: r1((left + right) / 2), y: r1(top + H / 2), 'text-anchor': 'middle', dy: '0.32em' }, 'chart-zero-note');
        note.textContent = defs.length > 1 ? 'All zero in this window' : 'Zero in this window';
        root.appendChild(note);
      }

      if (wantLabels && labelCands.length) {
        const g = svg(doc, 'g', null, 'chart-direct');
        for (const i of placeLabels(labelCands, 15)) {
          const c = labelCands[i];
          // Where end dots overlap, the labelled series' dot comes to the
          // front, so the key beside the label matches the dot it points at.
          if (c.dot) marks.appendChild(c.dot);
          const y = Math.max(top + 6, Math.min(top + H - 2, c.y));
          const x0 = right + 8;
          g.appendChild(svg(doc, 'line', { x1: x0, x2: x0 + 8, y1: r1(y), y2: r1(y) }, 'chart-key-stroke ' + slotClass(c.def.slot)));
          const text = svg(doc, 'text', { x: x0 + 12, y: r1(y), dy: '0.32em' }, 'chart-direct-label');
          if (names) {
            const name = svg(doc, 'tspan', null, 'chart-direct-name');
            name.textContent = c.def.label;
            text.appendChild(name);
          }
          const val = svg(doc, 'tspan', names ? { dx: 5 } : null, 'chart-direct-value');
          val.textContent = unit.value(c.value);
          text.appendChild(val);
          g.appendChild(text);
        }
        root.appendChild(g);
      }

      const cross = svg(doc, 'g', null, 'chart-cross');
      const crossLine = svg(doc, 'line', { x1: 0, x2: 0, y1: top, y2: top + H }, 'chart-crossline');
      cross.appendChild(crossLine);
      const dots = defs.map((d) => {
        const c = svg(doc, 'circle', { cx: 0, cy: 0, r: 4 }, 'chart-dot ' + slotClass(d.slot));
        cross.appendChild(c);
        return c;
      });
      root.appendChild(cross);
      const hit = svg(doc, 'rect', { x: left, y: top, width: Math.max(0, right - left), height: H }, 'chart-hit');
      root.appendChild(hit);
      body.appendChild(root);

      const total = stacked ? st.totals : null;
      const summary = (opts.title ? opts.title + '. ' : '') + defs.map((d, i) => d.label + ' ' + unit.value(lastValue(prep.values[i]))).join(', ') +
        ' at ' + clock(prep.times[lastIdx]) + '. Arrow keys step through time.';
      root.setAttribute('aria-label', summary);

      function readout(i) {
        const rows = defs.map((d, k) => ({ slot: d.slot, value: unit.value(prep.values[k][i]), label: d.label }));
        // A stack reads top-down, so the readout lists it in the same order.
        if (stacked) rows.reverse();
        if (stacked && defs.length > 1) rows.push({ value: unit.value(total[i]), label: 'total' });
        return { title: clock(prep.times[i], true), rows };
      }

      function mark(i) {
        const x = r1(px[i]);
        crossLine.setAttribute('x1', x);
        crossLine.setAttribute('x2', x);
        defs.forEach((d, k) => {
          const v = stacked ? st.upper[k][i] : prep.values[k][i];
          const hide = !isNum(v) || (stacked && !isNum(prep.values[k][i]));
          dots[k].setAttribute('cx', x);
          dots[k].setAttribute('cy', hide ? -100 : r1(ys(v)));
        });
        cross.classList.add('is-on');
      }

      function show(i, fromKeys) {
        if (i < 0) return;
        ctx.state.hover = prep.times[i];
        mark(i);
        const content = readout(i);
        let y = top + H / 2;
        if (!stacked) {
          const vals = prep.values.map((v) => v[i]).filter(isNum);
          if (vals.length) y = ys(Math.max.apply(null, vals));
        } else y = ys(total[i]);
        ctx.tip.show(root, px[i], Math.max(top + 20, y), content);
        if (fromKeys) ctx.tip.announce(content.title + ': ' + content.rows.map((r) => r.label + ' ' + r.value).join(', '));
        broadcast(opts.group, ctx.chart, prep.times[i]);
      }

      function hide() {
        ctx.state.hover = null;
        cross.classList.remove('is-on');
        ctx.tip.hide();
        broadcast(opts.group, ctx.chart, null);
      }

      ctx.state.sync = (t) => {
        if (t === null) cross.classList.remove('is-on');
        else {
          const i = nearestIndex(prep.times, t);
          if (i >= 0 && Math.abs(prep.times[i] - t) <= windowSeconds(opts.window) / 20 + 10) mark(i);
          else cross.classList.remove('is-on');
        }
      };
      join(opts.group, ctx.chart, ctx.state);

      let cur = lastIdx;
      ctx.on(root, 'pointermove', (e) => {
        const p = hoverTarget(root, ctx, e);
        cur = nearestIndex(px, Math.max(left, Math.min(right, p.x)));
        show(cur, false);
      });
      ctx.on(root, 'pointerleave', hide);
      ctx.on(root, 'focus', () => show(cur, true));
      ctx.on(root, 'blur', hide);
      ctx.on(root, 'keydown', (e) => {
        let next = cur;
        if (e.key === 'ArrowLeft') next = Math.max(0, cur - 1);
        else if (e.key === 'ArrowRight') next = Math.min(lastIdx, cur + 1);
        else if (e.key === 'Home') next = 0;
        else if (e.key === 'End') next = lastIdx;
        else if (e.key === 'Escape') {
          hide();
          return;
        } else return;
        if (e.preventDefault) e.preventDefault();
        cur = next;
        show(cur, true);
      });
      if (ctx.state.hover !== null && ctx.state.hover !== undefined) {
        const i = nearestIndex(prep.times, ctx.state.hover);
        if (i >= 0) mark(i);
      }
    },
    destroy(state) {
      leave(state);
    },
  };

  function lastDefined(arr) {
    for (let i = arr.length - 1; i >= 0; i--) if (isNum(arr[i])) return i;
    return -1;
  }

  function lastValue(arr) {
    const i = lastDefined(arr);
    return i >= 0 ? arr[i] : null;
  }

  function join(group, chart, state) {
    if (!group) return;
    if (!groups.has(group)) groups.set(group, new Map());
    groups.get(group).set(chart, state);
    state.group = group;
  }

  function leave(state) {
    if (!state.group || !groups.has(state.group)) return;
    const g = groups.get(state.group);
    for (const [chart, s] of g) if (s === state) g.delete(chart);
    if (!g.size) groups.delete(state.group);
  }

  function broadcast(group, from, t) {
    if (!group || !groups.has(group)) return;
    for (const [chart, s] of groups.get(group)) if (chart !== from && s.sync) s.sync(t);
  }

  // ---- sparkline --------------------------------------------------------

  function sparkPoints(ctx) {
    const d = ctx.data;
    if (Array.isArray(d)) return d.filter((p) => isNum(p[0]) && isNum(p[1]));
    const map = seriesMap(d);
    const key = ctx.opts.key || Object.keys(map)[0];
    return (map[key] || []).filter((p) => isNum(p[0]) && isNum(p[1]));
  }

  const sparklineForm = {
    cls: 'chart-spark',
    tableable: false,
    defaults: { height: 32, slot: 1 },
    empty(ctx) {
      return sparkPoints(ctx).length < 2;
    },
    table(ctx) {
      return { columns: [{ label: 'Time' }, { label: 'Value', num: true }], rows: sparkPoints(ctx).slice().reverse().map((p) => [clock(p[0], true), ctx.unit.value(p[1])]) };
    },
    draw(ctx, body) {
      const { doc, opts, width, unit } = ctx;
      const pts = sparkPoints(ctx);
      const last = pts[pts.length - 1];
      const lastText = unit.value(last[1]);
      const labelW = opts.lastLabel === false ? 0 : Math.ceil(textWidth(lastText, 12)) + 10;
      const H = opts.height;
      const x0 = 2;
      const x1 = width - labelW - 6;
      let max = 0;
      let min = Infinity;
      for (const p of pts) {
        max = Math.max(max, p[1]);
        min = Math.min(min, p[1]);
      }
      if (isNum(opts.yMax)) max = Math.max(max, opts.yMax);
      const win = windowSeconds(opts.window);
      const start = win > 0 ? last[0] - win : pts[0][0];
      const xs = scale(start, last[0], x0, x1);
      const ys = scale(0, max || 1, H - 5, 5);
      const root = svg(doc, 'svg', { width, height: H, viewBox: '0 0 ' + width + ' ' + H }, 'chart-svg');
      root.setAttribute('role', 'img');
      root.setAttribute('tabindex', '0');
      const coords = pts.filter((p) => p[0] >= start).map((p) => [xs(p[0]), ys(p[1])]);
      root.appendChild(svg(doc, 'path', { d: linePath(coords) }, 'chart-line ' + slotClass(slotOf(opts, 0))));
      const end = coords[coords.length - 1];
      root.appendChild(svg(doc, 'circle', { cx: r1(end[0]), cy: r1(end[1]), r: 4 }, 'chart-dot ' + slotClass(slotOf(opts, 0))));
      if (labelW) {
        const t = svg(doc, 'text', { x: width - labelW + 4, y: r1(Math.max(10, Math.min(H - 6, end[1]))), dy: '0.32em' }, 'chart-direct-value');
        t.textContent = lastText;
        root.appendChild(t);
      }
      root.setAttribute('aria-label', (opts.label ? opts.label + ': ' : '') + 'now ' + lastText + ', low ' + unit.value(min) + ', high ' + unit.value(max));
      body.appendChild(root);
      const times = coords.map((c) => c[0]);
      const shown = pts.filter((p) => p[0] >= start);
      const show = (i) => ctx.tip.show(root, coords[i][0], coords[i][1], { title: clock(shown[i][0], true), rows: [{ value: unit.value(shown[i][1]), label: opts.label || '' }] });
      ctx.on(root, 'pointermove', (e) => show(nearestIndex(times, hoverTarget(root, ctx, e).x)));
      ctx.on(root, 'pointerleave', () => ctx.tip.hide());
      ctx.on(root, 'focus', () => show(shown.length - 1));
      ctx.on(root, 'blur', () => ctx.tip.hide());
    },
  };

  // ---- donut / ring -----------------------------------------------------

  const donutForm = {
    cls: 'chart-donut',
    defaults: { size: 168, thickness: 12, unit: 'pct', label: '' },
    empty(ctx) {
      return !ctx.data || !isNum(ctx.data.value);
    },
    table(ctx) {
      const d = ctx.data;
      const rows = [[ctx.opts.label || 'Value', ctx.unit.value(d.value)]];
      if (isNum(d.start)) rows.push([d.startLabel || 'At window start', ctx.unit.value(d.start)]);
      if (d.sub) rows.push([d.subLabel || 'Note', d.sub]);
      return { columns: [{ label: 'Measure' }, { label: 'Value', num: true }], rows };
    },
    draw(ctx, body) {
      const { doc, opts, unit } = ctx;
      const d = ctx.data;
      const S = Math.max(96, Math.min(opts.size, ctx.width));
      const t = opts.thickness;
      const c = S / 2;
      const r = (S - t) / 2 - 1;
      const C = 2 * Math.PI * r;
      const root = svg(doc, 'svg', { width: S, height: S, viewBox: '0 0 ' + S + ' ' + S }, 'chart-svg');
      root.appendChild(svg(doc, 'circle', { cx: c, cy: c, r: r1(r), 'stroke-width': t, fill: 'none' }, 'chart-track'));
      const dash = ringDash(d.value, C);
      const slot = slotOf(opts, 0);
      const arc = svg(doc, 'circle', { cx: c, cy: c, r: r1(r), 'stroke-width': t, fill: 'none', transform: 'rotate(-90 ' + c + ' ' + c + ')' }, 'chart-ring ' + slotClass(slot));
      if (!dash.full) arc.setAttribute('stroke-dasharray', r1(dash.len) + ' ' + r1(C));
      if (dash.none) arc.setAttribute('visibility', 'hidden');
      const g = svg(doc, 'g', null, 'chart-mark');
      g.setAttribute('tabindex', '0');
      g.setAttribute('role', 'img');
      g.appendChild(arc);
      // A transparent ring wider than the mark, so the whole band is the hit target.
      g.appendChild(svg(doc, 'circle', { cx: c, cy: c, r: r1(r), 'stroke-width': t + 12, fill: 'none' }, 'chart-hit-ring'));
      root.appendChild(g);

      const valueText = d.valueText || unit.value(d.value);
      const delta = isNum(d.start) ? d.value - d.start : null;
      const hasDelta = delta !== null && opts.unit === 'pct';
      const vy = hasDelta ? c - 6 : c;
      // A ring under 140 px has no room for the 28 px figure inside its band.
      const v = svg(doc, 'text', { x: c, y: r1(vy), 'text-anchor': 'middle', dy: '0.32em' }, S < 140 ? 'chart-hero chart-hero-sm' : 'chart-hero');
      v.textContent = valueText;
      root.appendChild(v);
      if (opts.label) {
        const l = svg(doc, 'text', { x: c, y: r1(vy + 21), 'text-anchor': 'middle', dy: '0.32em' }, 'chart-hero-label');
        l.textContent = fit(opts.label, r * 2 - t * 2 - 8, 11);
        root.appendChild(l);
      }
      let deltaText = '';
      if (hasDelta) {
        const pts = Math.abs(delta * 100);
        const arrow = pts < 0.05 ? '' : delta > 0 ? '▲ ' : '▼ ';
        deltaText = (pts < 0.05 ? '±0' : arrow + (pts < 10 ? pts.toFixed(1) : Math.round(pts))) + ' pts';
        let tone = 'chart-delta';
        if (pts >= 0.05 && opts.goodWhen) tone += (delta > 0) === (opts.goodWhen === 'up') ? ' chart-delta-good' : ' chart-delta-bad';
        const dt = svg(doc, 'text', { x: c, y: r1(vy + 38), 'text-anchor': 'middle', dy: '0.32em' }, tone);
        dt.textContent = deltaText;
        root.appendChild(dt);
      }
      const wrap = node(doc, 'div', 'chart-donut-wrap');
      wrap.appendChild(root);
      if (d.sub) wrap.appendChild(node(doc, 'p', 'chart-caption', d.sub));
      body.appendChild(wrap);

      const since = d.startLabel || 'at window start';
      const label = (opts.label || opts.title || 'Value') + ' ' + valueText + (hasDelta ? ', ' + deltaText + ' against ' + unit.value(d.start) + ' ' + since : '');
      g.setAttribute('aria-label', label);
      const content = {
        title: opts.label || opts.title || '',
        rows: [{ slot, mark: 'rect', value: valueText, label: 'now' }].concat(hasDelta ? [{ value: unit.value(d.start), label: since }] : []),
        note: d.sub || '',
      };
      const show = () => ctx.tip.show(root, S - 4, c, content);
      ctx.on(g, 'pointerenter', show);
      ctx.on(g, 'pointerleave', () => ctx.tip.hide());
      ctx.on(g, 'focus', show);
      ctx.on(g, 'blur', () => ctx.tip.hide());
    },
  };

  // ---- ranked bars and stacked bars -------------------------------------

  const ROW_H = 36;
  const BAR_H = 10;

  function rowSlot(row, opts) {
    return row.other ? 'other' : slotOf(opts, 0);
  }

  function drawRows(ctx, body, rows, segmentsOf, totalOf, tipOf) {
    const { doc, opts, width, unit } = ctx;
    const mono = !!opts.mono;
    let valueW = 0;
    for (const r of rows) valueW = Math.max(valueW, textWidth(unit.value(totalOf(r)), 12));
    const gutter = Math.ceil(valueW) + 12;
    const left = 0;
    const plotW = Math.max(40, width - gutter - left);
    let max = 0;
    for (const r of rows) max = Math.max(max, totalOf(r));
    const xs = scale(0, max || 1, left, left + plotW);
    const H = rows.length * ROW_H + 4;
    const root = svg(doc, 'svg', { width, height: H, viewBox: '0 0 ' + width + ' ' + H }, 'chart-svg');
    root.setAttribute('role', 'group');
    if (opts.title) root.setAttribute('aria-label', opts.title);
    rows.forEach((row, i) => {
      const y = i * ROW_H;
      const g = svg(doc, 'g', null, 'chart-row');
      g.setAttribute('tabindex', '0');
      g.setAttribute('role', opts.onSelect ? 'button' : 'img');
      g.appendChild(svg(doc, 'rect', { x: -6, y: y + 1, width: width + 12, height: ROW_H - 2, rx: 4 }, 'chart-hit'));
      let flagW = 0;
      if (row.flag) flagW = textWidth(row.flag, 11) + 22;
      const name = svg(doc, 'text', { x: left, y: y + 13 }, 'chart-row-label' + (mono ? ' chart-mono' : ''));
      name.textContent = fit(row.label, width - flagW - 4, 12, mono);
      g.appendChild(name);
      if (row.full || textWidth(row.label, 12, mono) > width - flagW - 4) {
        const title = svg(doc, 'title');
        title.textContent = row.full || row.label;
        name.appendChild(title);
      }
      if (row.flag) {
        const fx = width - flagW + 4;
        g.appendChild(svg(doc, 'path', { d: 'M' + (fx + 6) + ' ' + (y + 4) + 'L' + (fx + 12) + ' ' + (y + 14) + 'H' + fx + 'Z' }, 'chart-flag-icon'));
        const ft = svg(doc, 'text', { x: fx + 16, y: y + 13 }, 'chart-flag');
        ft.textContent = row.flag;
        g.appendChild(ft);
      }
      const by = y + 20;
      const segs = segmentsOf(row).filter((s) => s.value > 0);
      let acc = 0;
      segs.forEach((s, k) => {
        const x0 = xs(acc);
        acc += s.value;
        const isLast = k === segs.length - 1;
        const x1 = xs(acc) - (isLast ? 0 : GAP);
        const w = x1 - x0;
        if (w < 0.5) return;
        const d = isLast ? barPathH(x0, by, w, BAR_H) : barPathH(x0, by, w, BAR_H, 0);
        g.appendChild(svg(doc, 'path', { d }, 'chart-bar ' + slotClass(s.slot)));
      });
      const vt = svg(doc, 'text', { x: r1(xs(totalOf(row)) + 6), y: by + BAR_H / 2, dy: '0.32em' }, 'chart-bar-value');
      vt.textContent = unit.value(totalOf(row));
      g.appendChild(vt);
      const content = tipOf(row);
      g.setAttribute('aria-label', (row.full || row.label) + ': ' + content.rows.map((r) => (r.label ? r.label + ' ' : '') + r.value).join(', ') + (row.flag ? ', ' + row.flag : ''));
      const show = () => {
        g.classList.add('is-active');
        ctx.tip.show(root, Math.min(xs(totalOf(row)), width - 24), by + BAR_H, content, true);
      };
      const hide = () => {
        g.classList.remove('is-active');
        ctx.tip.hide();
      };
      ctx.on(g, 'pointerenter', show);
      ctx.on(g, 'pointerleave', hide);
      ctx.on(g, 'focus', show);
      ctx.on(g, 'blur', hide);
      if (typeof opts.onSelect === 'function' && !row.other) {
        g.classList.add('is-link');
        ctx.on(g, 'click', () => opts.onSelect(row));
        ctx.on(g, 'keydown', (e) => {
          if (e.key === 'Enter' || e.key === ' ') {
            if (e.preventDefault) e.preventDefault();
            opts.onSelect(row);
          }
        });
      }
      root.appendChild(g);
    });
    body.appendChild(root);
  }

  const barsForm = {
    cls: 'chart-bars',
    defaults: { limit: 8 },
    empty(ctx) {
      return !Array.isArray(ctx.data) || !foldRows(ctx.data, ctx.opts.limit, ctx.opts.sort).length;
    },
    table(ctx) {
      const rows = foldRows(ctx.data, 0, ctx.opts.sort);
      return {
        columns: [{ label: ctx.opts.nameLabel || 'Name' }, { label: ctx.opts.valueLabel || 'Value', num: true }],
        rows: rows.map((r) => [r.full || r.label + (r.flag ? ' (' + r.flag + ')' : ''), ctx.unit.value(r.value)]),
      };
    },
    draw(ctx, body) {
      const rows = foldRows(ctx.data, ctx.opts.limit, ctx.opts.sort);
      drawRows(
        ctx,
        body,
        rows,
        (r) => [{ value: r.value, slot: rowSlot(r, ctx.opts) }],
        (r) => r.value,
        (r) => ({ title: r.full || r.label, rows: [{ slot: rowSlot(r, ctx.opts), mark: 'rect', value: ctx.unit.value(r.value), label: ctx.opts.valueLabel || '' }], note: r.flag || '' }),
      );
    },
  };

  function stackDefs(ctx) {
    const keys = [];
    for (const r of Array.isArray(ctx.data) ? ctx.data : []) for (const k in r.values || {}) if (keys.indexOf(k) < 0) keys.push(k);
    return seriesDefs(ctx.opts, keys);
  }

  function stackTotal(row, defs) {
    let s = 0;
    for (const d of defs) {
      const v = row.values && row.values[d.key];
      if (isNum(v) && v > 0) s += v;
    }
    return s;
  }

  const stackedBarsForm = {
    cls: 'chart-stacked',
    defaults: { limit: 8 },
    legend(ctx) {
      const defs = stackDefs(ctx);
      return defs.length < 2 ? [] : defs.map((d) => ({ label: d.label, slot: d.slot, mark: 'rect' }));
    },
    empty(ctx) {
      return !Array.isArray(ctx.data) || !ctx.data.length;
    },
    table(ctx) {
      const defs = stackDefs(ctx);
      const rows = ctx.data.slice();
      if (ctx.opts.sort !== false) rows.sort((a, b) => stackTotal(b, defs) - stackTotal(a, defs));
      return {
        columns: [{ label: ctx.opts.nameLabel || 'Name' }].concat(defs.map((d) => ({ label: d.label, num: true })), [{ label: 'Total', num: true }]),
        rows: rows.map((r) => [r.full || r.label + (r.flag ? ' (' + r.flag + ')' : '')].concat(defs.map((d) => ctx.unit.value((r.values || {})[d.key])), [ctx.unit.value(stackTotal(r, defs))])),
      };
    },
    draw(ctx, body) {
      const defs = stackDefs(ctx);
      let rows = ctx.data.slice();
      if (ctx.opts.sort !== false) rows.sort((a, b) => stackTotal(b, defs) - stackTotal(a, defs));
      if (ctx.opts.limit) rows = rows.slice(0, ctx.opts.limit);
      drawRows(
        ctx,
        body,
        rows,
        (r) => defs.map((d) => ({ value: isNum((r.values || {})[d.key]) ? r.values[d.key] : 0, slot: d.slot })),
        (r) => stackTotal(r, defs),
        (r) => ({
          title: r.full || r.label,
          rows: defs.map((d) => ({ slot: d.slot, mark: 'rect', value: ctx.unit.value((r.values || {})[d.key]), label: d.label })).concat(defs.length > 1 ? [{ value: ctx.unit.value(stackTotal(r, defs)), label: 'total' }] : []),
          note: r.flag || '',
        }),
      );
    },
  };

  // ---- histogram --------------------------------------------------------

  const histogramForm = {
    cls: 'chart-histogram',
    defaults: { height: 150, unit: 'count' },
    empty(ctx) {
      if (!Array.isArray(ctx.data) || !ctx.data.length) return true;
      return !ctx.data.some((b) => isNum(b.value) && b.value > 0);
    },
    table(ctx) {
      const total = ctx.data.reduce((s, b) => s + (isNum(b.value) ? b.value : 0), 0);
      return {
        columns: [{ label: ctx.opts.bucketLabel || 'Bucket' }, { label: ctx.opts.valueLabel || 'Count', num: true }, { label: 'Share', num: true }],
        rows: ctx.data.map((b) => [b.full || b.label, ctx.unit.value(b.value), total ? pct(b.value / total, 1) : '–']),
      };
    },
    draw(ctx, body) {
      const { doc, opts, width, unit } = ctx;
      const data = ctx.data;
      const H = opts.height;
      const top = 18;
      const bottom = 24;
      let max = 0;
      let peak = 0;
      data.forEach((b, i) => {
        if (isNum(b.value) && b.value > max) {
          max = b.value;
          peak = i;
        }
      });
      const total = data.reduce((s, b) => s + (isNum(b.value) ? b.value : 0), 0);
      const ticks = yTicks(opts.unit, max, 3);
      const left = tickGutter(ctx, ticks);
      const right = width - 4;
      const band = (right - left) / data.length;
      const colW = Math.max(4, Math.min(24, band - GAP));
      const ys = scale(0, ticks[ticks.length - 1], top + H, top);
      const root = svg(doc, 'svg', { width, height: top + H + bottom, viewBox: '0 0 ' + width + ' ' + (top + H + bottom) }, 'chart-svg');
      root.setAttribute('role', 'group');
      if (opts.title) root.setAttribute('aria-label', opts.title);
      const frame = svg(doc, 'g', null, 'chart-frame');
      yAxis(ctx, frame, ys, ticks, left, right);
      root.appendChild(frame);
      let every = 1;
      let widest = 0;
      for (const b of data) widest = Math.max(widest, textWidth(b.label, 11));
      while (widest + 6 > band * every && every < data.length) every++;
      const slot = slotOf(opts, 0);
      data.forEach((b, i) => {
        const cx = left + band * i + band / 2;
        const v = isNum(b.value) ? b.value : 0;
        const g = svg(doc, 'g', null, 'chart-col');
        g.setAttribute('tabindex', '0');
        g.setAttribute('role', 'img');
        g.appendChild(svg(doc, 'rect', { x: r1(left + band * i), y: top, width: r1(band), height: H }, 'chart-hit'));
        const h = ys(0) - ys(v);
        if (h > 0) g.appendChild(svg(doc, 'path', { d: barPathV(cx - colW / 2, ys(v), colW, h) }, 'chart-bar ' + slotClass(slot)));
        if (i % every === 0) {
          const t = svg(doc, 'text', { x: r1(cx), y: top + H + 17, 'text-anchor': 'middle' }, 'chart-axis');
          t.textContent = b.label;
          g.appendChild(t);
        }
        if (i === peak && v > 0) {
          const t = svg(doc, 'text', { x: r1(cx), y: r1(ys(v) - 6), 'text-anchor': 'middle' }, 'chart-bar-value');
          t.textContent = unit.value(v);
          g.appendChild(t);
        }
        const share = total ? pct(v / total, 1) : '–';
        g.setAttribute('aria-label', (b.full || b.label) + ': ' + unit.value(v) + ', ' + share);
        const content = { title: b.full || b.label, rows: [{ slot, mark: 'rect', value: unit.value(v), label: opts.valueLabel || 'requests' }, { value: share, label: 'of all' }] };
        const show = () => {
          g.classList.add('is-active');
          ctx.tip.show(root, cx + colW / 2, Math.max(top + 20, ys(v)), content);
        };
        const hide = () => {
          g.classList.remove('is-active');
          ctx.tip.hide();
        };
        ctx.on(g, 'pointerenter', show);
        ctx.on(g, 'pointerleave', hide);
        ctx.on(g, 'focus', show);
        ctx.on(g, 'blur', hide);
        root.appendChild(g);
      });
      body.appendChild(root);
    },
  };

  // One row per category: the range from the fastest to the slowest sample,
  // every sample as a dot, and the median as the larger mark. One measure on
  // one axis, so it uses one slot; the labels carry the identity.
  function rangeRows(data) {
    return (Array.isArray(data) ? data : []).map((r) => {
      const samples = (r.samples || []).filter(isNum);
      return Object.assign({}, r, { samples, has: samples.length > 0 });
    });
  }

  function rangeText(unit, r) {
    if (!r.has) return '–';
    return unit.value(r.median) + (r.note ? ' · ' + r.note : '');
  }

  const rangesForm = {
    cls: 'chart-ranges',
    empty(ctx) {
      return !rangeRows(ctx.data).some((r) => r.has);
    },
    table(ctx) {
      const u = ctx.unit;
      return {
        columns: [{ label: ctx.opts.nameLabel || 'Setting' }, { label: 'Median', num: true }, { label: 'Fastest', num: true }, { label: 'Slowest', num: true }, { label: ctx.opts.noteLabel || 'Against off', num: true }],
        rows: rangeRows(ctx.data).map((r) => (r.has ? [r.label, u.value(r.median), u.value(r.min), u.value(r.max), r.note || ''] : [r.label, '–', '–', '–', ''])),
      };
    },
    draw(ctx, body) {
      const { doc, opts, width, unit } = ctx;
      const rows = rangeRows(ctx.data);
      const slot = slotOf(opts, 0);
      let valueW = 0;
      let max = 0;
      for (const r of rows) {
        valueW = Math.max(valueW, textWidth(rangeText(unit, r), 12));
        if (r.has) max = Math.max(max, r.max, ...r.samples);
      }
      const ticks = niceTicks(max, width < 420 ? 3 : 5);
      const gutter = Math.ceil(valueW) + 14;
      const left = 6;
      const plotW = Math.max(40, width - gutter - left);
      const xs = scale(0, ticks[ticks.length - 1] || 1, left, left + plotW);
      const top = 0;
      const H = rows.length * ROW_H;
      const axisH = 20;
      const root = svg(doc, 'svg', { width, height: H + axisH, viewBox: '0 0 ' + width + ' ' + (H + axisH) }, 'chart-svg');
      root.setAttribute('role', 'group');
      if (opts.title) root.setAttribute('aria-label', opts.title);
      const frame = svg(doc, 'g', null, 'chart-frame');
      for (const t of ticks) {
        const x = r1(xs(t)) + 0.5;
        frame.appendChild(svg(doc, 'line', { x1: x, x2: x, y1: top, y2: H }, t === 0 ? 'chart-baseline' : 'chart-grid'));
        const label = svg(doc, 'text', { x, y: H + 14, 'text-anchor': t === 0 ? 'start' : 'middle' }, 'chart-axis');
        label.textContent = unit.tick(t);
        frame.appendChild(label);
      }
      root.appendChild(frame);
      rows.forEach((row, i) => {
        const y = top + i * ROW_H;
        const cy = y + 25;
        const g = svg(doc, 'g', null, 'chart-row');
        g.setAttribute('tabindex', '0');
        g.setAttribute('role', 'img');
        g.appendChild(svg(doc, 'rect', { x: -6, y: y + 1, width: width + 12, height: ROW_H - 2, rx: 4 }, 'chart-hit'));
        const name = svg(doc, 'text', { x: left, y: y + 12 }, 'chart-row-label');
        name.textContent = fit(row.label, plotW, 12);
        g.appendChild(name);
        if (row.has) {
          const x0 = xs(row.min);
          const x1 = Math.max(xs(row.max), x0 + 1);
          g.appendChild(svg(doc, 'rect', { x: r1(x0), y: cy - 3, width: r1(x1 - x0), height: 6, rx: 3 }, 'chart-range ' + slotClass(slot)));
          for (const v of row.samples) g.appendChild(svg(doc, 'circle', { cx: r1(xs(v)), cy, r: 3 }, 'chart-sample ' + slotClass(slot)));
          g.appendChild(svg(doc, 'circle', { cx: r1(xs(row.median)), cy, r: 5.5 }, 'chart-median ' + slotClass(slot)));
        }
        const vt = svg(doc, 'text', { x: r1(left + plotW + 10), y: cy, dy: '0.32em' }, 'chart-bar-value');
        vt.textContent = rangeText(unit, row);
        g.appendChild(vt);
        const n = row.samples.length;
        const desc = row.has ? 'median ' + unit.value(row.median) + ', ' + unit.value(row.min) + ' to ' + unit.value(row.max) + ', ' + n + (n === 1 ? ' sample' : ' samples') + (row.note ? ', ' + row.note : '') : 'no samples yet';
        g.setAttribute('aria-label', row.label + ': ' + desc);
        const content = row.has
          ? {
              title: row.label,
              rows: [
                { slot, value: unit.value(row.median), label: 'median' },
                { value: unit.value(row.min) + ' – ' + unit.value(row.max), label: 'range' },
                { value: String(n), label: n === 1 ? 'sample' : 'samples' },
              ],
              note: row.note ? row.note + ' ' + (opts.noteLabel || 'against off').toLowerCase() : '',
            }
          : { title: row.label, rows: [{ value: 'no samples yet', label: '' }] };
        const show = () => {
          g.classList.add('is-active');
          ctx.tip.show(root, Math.min(row.has ? xs(row.median) : left, width - 24), cy + 6, content, true);
        };
        const hide = () => {
          g.classList.remove('is-active');
          ctx.tip.hide();
        };
        ctx.on(g, 'pointerenter', show);
        ctx.on(g, 'pointerleave', hide);
        ctx.on(g, 'focus', show);
        ctx.on(g, 'blur', hide);
        root.appendChild(g);
      });
      body.appendChild(root);
    },
  };

  function factory(form) {
    return (el, opts) => createChart(el, opts, form);
  }

  const donut = factory(donutForm);

  return {
    EMPTY_TEXT,
    WINDOWS,
    donut,
    ring: donut,
    timeseries: factory(timeseriesForm),
    bars: factory(barsForm),
    stackedBars: factory(stackedBarsForm),
    histogram: factory(histogramForm),
    ranges: factory(rangesForm),
    sparkline: factory(sparklineForm),
    compact,
    duration,
    bytes,
    formatValue,
    niceTicks,
    yTicks,
    timeTicks,
    windowSeconds,
    linePath,
    areaPath,
    barPathH,
    barPathV,
    placeLabels,
    nearestIndex,
    prepTimeseries,
    stackValues,
    foldRows,
    ringDash,
    fit,
    slotOf,
    reducedMotion,
    groupSize(name) {
      return groups.has(name) ? groups.get(name).size : 0;
    },
  };
});
