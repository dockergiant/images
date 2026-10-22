(function (root, factory) {
  const isNode = typeof module === 'object' && module.exports;
  const deps = isNode ? { fmt: require('../lib/format.js'), view: require('./overview.js').view } : root.Inspector;
  const mod = factory(deps);
  if (isNode) {
    module.exports = mod;
  } else {
    document.addEventListener('alpine:init', () => mod.register(window.Alpine, root.Inspector));
  }
})(this, function (deps) {
  const fmt = deps.fmt;
  const view = deps.view;

  const TABS = [
    { id: 'consumers', label: 'Consumers' },
    { id: 'topology', label: 'Topology' },
    { id: 'dbqueues', label: 'Database queues' },
    { id: 'peek', label: 'Peek messages' },
  ];

  const WHAT = {
    'async.operations.all': 'Bulk and async API jobs',
    'product_action_attribute.update': 'Mass attribute update',
    'product_action_attribute.website.update': 'Mass website assignment',
    'inventory.reservations.updateSalabilityStatus': 'Recalculate salable qty',
    'inventory.reservations.update': 'Update stock reservations',
    'inventory.reservations.cleanup': 'Clean up stock reservations',
    'inventory.source.items.cleanup': 'Clean up source items',
    'inventory.indexer.sourceItem': 'Reindex source items',
    'inventory.indexer.stock': 'Reindex stock',
    'inventory.mass.update': 'Mass stock update',
    'media.storage.catalog.image.resize': 'Resize product images',
    'media.content.synchronization': 'Sync media content',
    'media.gallery.synchronization': 'Sync media gallery',
    'media.gallery.renditions.update': 'Update gallery renditions',
    catalog_product_generate_urls: 'Generate product URLs',
    catalog_website_attribute_value_sync: 'Sync website attribute values',
    codegenerator: 'Generate cart rule coupons',
    export: 'Admin data export',
    'sales.rule.update.coupon.usage': 'Update coupon usage',
    'sales.rule.quote.trigger.recollect': 'Recollect quotes after a rule change',
    'product_alert.queue': 'Send product alerts',
    saveConfig: 'Save configuration',
    pbtemplate: 'Page Builder templates',
  };

  function queueRows(list) {
    return (list || []).map((q) => ({
      key: q.name,
      name: q.name,
      what: WHAT[q.name] || '',
      ready: fmt.num(q.ready),
      readyCls: q.stuck ? 'text-warn font-semibold' : '',
      unacked: fmt.num(q.unacked),
      consumers: q.consumers > 0 ? q.consumers + ' running' : 'none',
      consumersCls: q.consumers > 0 ? 'text-ok' : q.ready > 0 ? 'text-warn' : 'text-muted',
      published: fmt.num(q.published),
      oldest: q.oldest || '',
      raw: q,
    }));
  }

  function consumerRows(list) {
    return (list || []).map((c, i) => ({
      key: i,
      queue: c.queue,
      consumer: c.consumer || c.tag,
      prefetch: c.prefetch,
      process: [c.container, c.pid ? 'pid ' + c.pid : '', c.cmd].filter(Boolean).join(' · ') || c.peerHost + ':' + c.peerPort,
      active: c.active ? 'active' : 'idle',
    }));
  }

  function bindingRows(list) {
    return (list || []).map((b, i) => ({ key: i, source: b.source, key2: b.routing_key, dest: b.destination, type: b.destination_type }));
  }

  function dbQueueRows(list) {
    return (list || []).map((q) => ({
      key: q.name,
      name: q.name,
      total: fmt.num(q.total),
      statuses: Object.keys(q.statuses || {})
        .sort()
        .map((k) => k + ' ' + q.statuses[k])
        .join(' · '),
      oldest: q.oldestUpdate ? fmt.ago(q.oldestUpdate) : '',
    }));
  }

  function peekText(m) {
    const head = ['exchange ' + (m.exchange || '(default)'), 'key ' + m.routing_key];
    if (m.redelivered) head.push('redelivered');
    head.push(fmt.bytes(m.payload_bytes));
    let body = m.payload;
    if (m.payload_encoding === 'string') {
      try {
        body = JSON.stringify(JSON.parse(m.payload), null, 2);
      } catch (e) {
        body = m.payload;
      }
    }
    return head.join(' · ') + '\n' + body;
  }

  function purgeEffect(q) {
    const n = q ? q.ready : 0;
    return fmt.num(n) + (n === 1 ? ' waiting message is' : ' waiting messages are') + ' deleted. Magento does not republish them, so bulk actions in the admin can stay "in progress" for good because their operations never run.';
  }

  function register(Alpine, I) {
    Alpine.data('rabbitmq', () => {
      let off = null;
      let stopBrowse = null;
      // Any hash change replaces the route object, so the effect re-runs on
      // a Raw toggle too; only entering the section should fetch.
      let browseKey = '';
      return view.compose(view.serviceBase('rabbitmq', I, Alpine), {
        tabs: TABS,
        tab: 'consumers',
        error: '',
        queues: null,
        consumers: null,
        topology: null,
        dbqueues: null,
        dbError: '',
        peekQueue: '',
        peekCount: 5,
        peeked: null,
        peekError: '',
        purgeQueue: '',

        init() {
          this.startPolling();
          this.loadQueues();
          off = setInterval(() => {
            if (!document.hidden && (this.isHealth || this.isActions)) this.loadQueues();
          }, view.POLL_MS);
          stopBrowse = Alpine.effect(() => {
            const r = Alpine.store('app').route;
            const key = r.tab === 'rabbitmq' ? r.section : '';
            if (key === browseKey) return;
            browseKey = key;
            if (key === 'browse') setTimeout(() => this.loadTab(), 0);
          });
        },

        destroy() {
          this.stopPolling();
          if (off) clearInterval(off);
          if (stopBrowse) Alpine.release(stopBrowse);
          stopBrowse = null;
        },

        async loadQueues() {
          try {
            const r = await I.client.get('/api/rabbitmq/queues', { quiet: true });
            this.queues = r.queues;
            if (!this.peekQueue && r.queues.length) this.peekQueue = (r.queues.find((q) => q.ready > 0) || r.queues[0]).name;
            if (!this.purgeQueue && r.queues.length) this.purgeQueue = (r.queues.find((q) => q.ready > 0) || r.queues[0]).name;
          } catch (e) {
            this.error = e.message;
          }
        },

        get queueList() {
          return queueRows(this.queues);
        },
        get hasQueues() {
          return this.queueList.length > 0;
        },
        get queueOptions() {
          return this.queueList.map((q) => ({ name: q.name, label: q.name + (q.raw.ready ? ' (' + q.raw.ready + ' waiting)' : '') }));
        },
        get hasError() {
          return !!this.error;
        },

        get tabList() {
          return TABS.map((t) => ({ id: t.id, label: t.label, pressed: t.id === this.tab ? 'true' : 'false' }));
        },
        get isTabConsumers() {
          return this.tab === 'consumers';
        },
        get isTabTopology() {
          return this.tab === 'topology';
        },
        get isTabDb() {
          return this.tab === 'dbqueues';
        },
        get isTabPeek() {
          return this.tab === 'peek';
        },
        selectTab(t) {
          this.tab = t.id;
          this.loadTab();
        },
        async loadTab() {
          try {
            if (this.tab === 'consumers') this.consumers = (await I.client.get('/api/rabbitmq/consumers', { quiet: true })).consumers;
            else if (this.tab === 'topology') this.topology = await I.client.get('/api/rabbitmq/topology', { quiet: true });
            else if (this.tab === 'dbqueues') {
              this.dbError = '';
              try {
                this.dbqueues = (await I.client.get('/api/rabbitmq/dbqueues', { quiet: true })).queues;
              } catch (e) {
                this.dbqueues = [];
                this.dbError = e.message;
              }
            }
            this.error = '';
          } catch (e) {
            this.error = e.message;
          }
        },
        get consumerList() {
          return consumerRows(this.consumers);
        },
        get exchangeList() {
          return ((this.topology && this.topology.exchanges) || []).map((e) => ({ key: e.name, name: e.name, type: e.type || '' }));
        },
        get bindingList() {
          return bindingRows(this.topology && this.topology.bindings);
        },
        get dbList() {
          return dbQueueRows(this.dbqueues);
        },
        get hasDbError() {
          return !!this.dbError;
        },
        get noConsumers() {
          return this.consumers !== null && this.consumers.length === 0;
        },

        peekFrom(q) {
          this.peekQueue = q.name;
          this.peeked = null;
          this.tab = 'peek';
          I.go('rabbitmq', 'browse');
        },
        async doPeek() {
          if (!this.peekQueue) return;
          this.peekError = '';
          this.peeked = null;
          try {
            const r = await I.client.post('/api/rabbitmq/peek', { queue: this.peekQueue, count: Number(this.peekCount) || 5 }, { quiet: true });
            this.peeked = (r.messages || []).map((m, i) => ({ key: i, text: peekText(m) }));
          } catch (e) {
            this.peekError = e.message;
          }
        },
        get hasPeeked() {
          return this.peeked !== null;
        },
        get peekEmpty() {
          return this.peeked !== null && this.peeked.length === 0;
        },
        get hasPeekError() {
          return !!this.peekError;
        },
        get peekTitle() {
          return 'Oldest messages in ' + this.peekQueue;
        },

        get liveRates() {
          return Alpine.store('drawer').has('rabbitmq:rates');
        },
        get liveFirehose() {
          return Alpine.store('drawer').has('rabbitmq:firehose');
        },
        get ratesLabel() {
          return this.liveRates ? 'Showing publish rates' : 'Show publish rates';
        },
        get firehoseLabel() {
          return this.liveFirehose ? 'Firehose is on' : 'Turn the firehose on';
        },
        openRates() {
          this.openDrawer('rabbitmq:rates');
        },
        openFirehose() {
          this.openDrawer('rabbitmq:firehose');
        },

        get tracingOn() {
          const a = I.adapter('rabbitmq');
          return !!(a && a.info && a.info.features && a.info.features.tracing);
        },
        get tracingText() {
          return this.tracingOn ? 'Tracing is on for this vhost. The broker copies every message to amq.rabbitmq.trace.' : 'Tracing is off.';
        },
        get tracingWarning() {
          return this.hv.warnings.some((w) => w.id === 'tracing-no-consumer');
        },
        async doTracingOff() {
          const res = await I.confirm({
            kind: 'rabbitmq',
            action: 'tracing-off',
            target: '',
            title: 'Switch RabbitMQ tracing off?',
            effect: 'The broker stops copying every message to amq.rabbitmq.trace. The firehose in the Live drawer goes quiet.',
            call: 'rabbitmqctl trace_off',
            button: 'Switch tracing off',
          });
          if (res) {
            I.loadSession(true).catch(() => {});
            this.refreshHealth();
          }
        },

        get purgeTarget() {
          return this.queueList.find((q) => q.name === this.purgeQueue) || null;
        },
        get canPurge() {
          return !this.lan && !!this.purgeTarget;
        },
        async doPurge() {
          const q = this.purgeTarget;
          if (!q) return;
          const n = q.raw.ready;
          const res = await I.confirm({
            kind: 'rabbitmq',
            action: 'purge-queue',
            target: q.name,
            title: 'Purge ' + q.name + '?',
            effect: purgeEffect(q.raw),
            call: 'DELETE /api/queues/%2F/' + encodeURIComponent(q.name) + '/contents',
            button: 'Purge ' + fmt.num(n) + (n === 1 ? ' message' : ' messages'),
            typedLabel: 'Type the queue name to confirm',
          });
          if (res) {
            this.loadQueues();
            this.refreshHealth();
          }
        },
      });
    });
  }

  return { register, queueRows, consumerRows, dbQueueRows, bindingRows, peekText, purgeEffect };
});
