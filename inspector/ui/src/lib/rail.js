(function (root, factory) {
  const isNode = typeof module === 'object' && module.exports;
  const deps = isNode ? { fmt: require('./format.js'), route: require('./route.js') } : root.Inspector;
  const mod = factory(deps);
  if (isNode) {
    module.exports = mod;
  } else {
    root.Inspector.rail = mod;
  }
})(this, function (deps) {
  const fmt = deps.fmt;
  const route = deps.route;

  // Rail order differs from route.TABS: PHP traces sits right after Overview
  // because that is where a slow page gets answered.
  const ORDER = ['overview', 'traces', 'varnish', 'redis', 'search', 'rabbitmq'];
  const SECTION_LABELS = { health: 'Health', browse: 'Browse', live: 'Live', actions: 'Actions' };
  const TRACE_SECTIONS = [
    { id: 'hotspots', label: 'Hotspots', section: '' },
    { id: 'requests', label: 'Requests', section: 'requests' },
  ];
  const TONES = ['ok', 'warn', 'error'];
  const FOOTER = { connect: 'API & extensions', performance: 'Performance' };
  const RAW_LABELS = { varnish: 'Raw counters', redis: 'Raw keys', search: 'Raw JSON', rabbitmq: 'Raw JSON' };

  function adapterOf(list, kind) {
    return (list || []).find((a) => a.kind === kind) || null;
  }

  function isPresent(session, kind) {
    if (!kind || kind === 'php') return true;
    const a = adapterOf(session && session.adapters, kind);
    return !!(a && a.info && a.info.present);
  }

  function labelOf(session, kind) {
    if (kind === 'php') return 'PHP traces';
    if (!kind) return 'Overview';
    const a = adapterOf(session && session.adapters, kind);
    return fmt.serviceLabel(kind, a && a.info);
  }

  // The dot follows the health tone the overview poll reports. A service that
  // is not in the env shows as absent; one without a health answer yet as idle.
  function toneOf(session, overview, kind) {
    if (!kind || kind === 'php') return '';
    if (session && !isPresent(session, kind)) return 'absent';
    const a = adapterOf(overview && overview.adapters, kind);
    const st = a && a.health && a.health.status;
    return TONES.includes(st) ? st : 'idle';
  }

  function sectionsOf(r, tab) {
    if (r.tab !== tab) return [];
    if (tab === 'traces') {
      const tv = route.traceView(r);
      const out = TRACE_SECTIONS.map((s) => ({
        id: s.id,
        label: s.label,
        href: route.format({ tab: 'traces', section: s.section }),
        current: tv.page === s.id ? 'page' : null,
      }));
      if (tv.page === 'trace') {
        out.push({ id: 'trace', label: 'Trace ' + shortId(tv.id), href: route.format({ tab: 'traces', section: tv.id }), current: 'page', mono: true });
      }
      return out;
    }
    if (!route.isService(tab)) return [];
    return route.SECTIONS.map((id) => ({
      id,
      label: SECTION_LABELS[id],
      href: route.format({ tab, section: id }),
      current: r.section === id ? 'page' : null,
    }));
  }

  function shortId(id) {
    const s = String(id || '');
    return s.length > 10 ? s.slice(0, 8) + '…' : s;
  }

  function services(input) {
    const r = input.route;
    const session = input.session;
    return ORDER.map((tab) => {
      const kind = route.kindOf(tab);
      const present = !session || isPresent(session, kind);
      const label = labelOf(session, kind);
      const tone = toneOf(session, input.overview, kind);
      const sections = present ? sectionsOf(r, tab) : [];
      return {
        id: tab,
        kind,
        label,
        href: route.format({ tab, section: route.isService(tab) ? 'health' : '' }),
        // The open section is the page; its service is only the way there,
        // so a screen reader does not hear "current page" twice.
        current: sections.some((s) => s.current) ? 'true' : r.tab === tab ? 'page' : null,
        active: r.tab === tab,
        absent: present ? 'false' : 'true',
        tone,
        toneLabel: toneLabel(tone),
        title: present ? label : label + ' is not part of this env',
        sections,
        hasSections: sections.length > 0,
        icon: '#i-' + tab,
      };
    });
  }

  function toneLabel(tone) {
    switch (tone) {
      case 'ok':
        return 'healthy';
      case 'warn':
        return 'needs attention';
      case 'error':
        return 'failing';
      case 'absent':
        return 'not in this env';
      case 'idle':
        return 'checking';
      default:
        return '';
    }
  }

  // The rail's Live group is narrow and its dots carry the service hue, so a
  // row reads "Slowlog tail" rather than "OpenSearch slowlog tail". A one-word
  // title keeps its service so "Monitor" does not stand alone.
  function shortStream(service, title) {
    const t = String(title || '').replace(/\s*\([^)]*\)\s*$/, '');
    return /\s/.test(t) ? t : service + ' ' + t.toLowerCase();
  }

  function streamList(session) {
    if (!session) return [];
    const out = [];
    for (const a of session.adapters || []) {
      if (!a.info || !a.info.present) continue;
      const service = fmt.serviceLabel(a.kind, a.info);
      for (const st of a.streams || []) {
        out.push({ sub: a.kind + ':' + st.name, kind: a.kind, label: fmt.streamLabel(service, st.title), short: shortStream(service, st.title), optIn: !!st.optIn, title: st.title });
      }
    }
    out.push({ sub: 'php:traces', kind: 'php', label: 'PHP traces', short: 'PHP traces', optIn: false, title: 'Traces as they arrive from php-fpm' });
    return out;
  }

  // Every stream with its on/off state. A stream counts as on while the drawer
  // is open and has it selected; filtered subs a view opened ("varnish:log;url=/a")
  // get their own row so they can be switched off from the rail too.
  function live(input) {
    const session = input.session;
    const d = input.drawer || { open: false, selected: [] };
    const lan = !!(session && session.lan);
    const known = new Set();
    const out = streamList(session).map((st) => {
      known.add(st.sub);
      const on = !!(d.open && d.selected.includes(st.sub));
      const disabled = st.optIn && lan && !on;
      let title = st.title;
      if (st.optIn) title += disabled ? '. Refused: ' + (session.lanReason || 'Traefik listens beyond loopback') : '. Changes the service while it runs.';
      return { sub: st.sub, kind: st.kind, label: st.label, short: st.short, optIn: st.optIn, on: on ? 'true' : 'false', disabled, title, filtered: false };
    });
    for (const sub of d.selected) {
      if (known.has(sub)) continue;
      const kind = sub.split(':')[0];
      const fl = sub.split(';').slice(1).join(' · ') || sub;
      out.push({ sub, kind, label: fl, short: fl, optIn: false, on: d.open ? 'true' : 'false', disabled: false, title: 'Filtered stream opened from a view: ' + sub, filtered: true });
    }
    return out;
  }

  function liveCount(input) {
    return live(input).filter((s) => s.on === 'true').length;
  }

  // Breadcrumb: service › section › item. `item` is what a view names the
  // current thing (a trace's route, a key); without one the route's id stands in.
  function crumbs(input) {
    const r = input.route;
    const session = input.session;
    const out = [];
    const tab = r.tab;
    if (FOOTER[tab]) {
      out.push({ label: FOOTER[tab], href: route.format({ tab }) });
      return mark(out);
    }
    const kind = route.kindOf(tab);
    out.push({ label: labelOf(session, kind), href: route.format({ tab, section: route.isService(tab) ? 'health' : '' }) });
    if (tab === 'traces') {
      const tv = route.traceView(r);
      if (tv.page === 'hotspots') out.push({ label: 'Hotspots', href: route.format({ tab: 'traces' }) });
      else if (tv.page === 'requests') out.push({ label: 'Requests', href: route.format({ tab: 'traces', section: 'requests' }) });
      else out.push({ label: input.item || shortId(tv.id), href: route.format({ tab: 'traces', section: tv.id }), mono: true });
    } else if (route.isService(tab)) {
      out.push({ label: SECTION_LABELS[r.section] || r.section, href: route.format({ tab, section: r.section }) });
      if (input.item) out.push({ label: input.item, href: route.format(r), mono: true });
    } else if (input.item) {
      out.push({ label: input.item, href: route.format(r), mono: true });
    }
    return mark(out);
  }

  function mark(list) {
    return list.map((c, i) => Object.assign({ key: i, last: i === list.length - 1, current: i === list.length - 1 ? 'page' : null, mono: !!c.mono }, c));
  }

  function pageTitle(list, env) {
    const parts = (list || []).map((c) => c.label).reverse();
    if (env) parts.push(env);
    return parts.length ? parts.join(' · ') : 'Roll Inspector';
  }

  function rawLabel(tab) {
    return RAW_LABELS[tab] || '';
  }

  return { ORDER, services, streamList, live, liveCount, crumbs, pageTitle, rawLabel, toneOf, isPresent, labelOf, shortId };
});
