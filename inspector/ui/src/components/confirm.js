document.addEventListener('alpine:init', () => {
  const Alpine = window.Alpine;
  const I = window.Inspector;

  Alpine.store('confirm', {
    request: null,
    busy: false,

    open(opts) {
      const spec = I.actionSpec(opts.kind, opts.action) || {};
      const level = opts.confirm || spec.confirm || 'plain';
      const req = {
        kind: opts.kind,
        action: opts.action,
        target: opts.target === undefined || opts.target === null ? '' : String(opts.target),
        params: I.api.stringParams(opts.params),
        level,
        destructive: opts.destructive === undefined ? !!spec.destructive : !!opts.destructive,
        title: opts.title || spec.title || opts.action,
        effect: opts.effect || '',
        call: opts.call || '',
        button: opts.button || spec.title || 'Confirm',
        typedLabel: opts.typedLabel || 'Type ' + (opts.target || 'the name') + ' to confirm',
      };
      if (level === 'none' && !opts.always) return runDirect(req);
      if (this.busy) {
        I.toast('Another action is still running.', 'warn');
        return Promise.resolve(null);
      }
      if (this.request) this.request.resolve(null);
      return new Promise((resolve) => {
        req.resolve = resolve;
        this.request = req;
      });
    },

    finish(result) {
      const r = this.request;
      this.request = null;
      this.busy = false;
      if (r) r.resolve(result);
    },
  });

  async function runDirect(req) {
    try {
      const res = await I.client.action({ kind: req.kind, action: req.action, target: req.target, params: req.params, confirm: 'none' });
      if (res && res.message) I.toast(res.message, 'ok');
      return res;
    } catch (e) {
      return null;
    }
  }

  I.confirm = (opts) => Alpine.store('confirm').open(opts);

  // A dialog opened from the palette starts while the palette hands focus
  // back, and the control it came from may be gone or hidden by the end.
  function restoreFocus(el) {
    if (el && el !== document.body && document.body.contains(el) && el.getClientRects().length && typeof el.focus === 'function') {
      el.focus();
      return;
    }
    const main = document.getElementById('content');
    if (main) main.focus({ preventScroll: true });
  }

  Alpine.data('confirmDialog', () => {
    let fx = null;
    let returnFocus = null;
    return {
      typed: '',
      error: '',

      init() {
        fx = Alpine.effect(() => {
          const r = Alpine.store('confirm').request;
          this.typed = '';
          this.error = '';
          if (r) {
            this.$nextTick(() => {
              if (!returnFocus) returnFocus = document.activeElement;
              this.focusFirst();
            });
          } else if (returnFocus) {
            const el = returnFocus;
            returnFocus = null;
            this.$nextTick(() => restoreFocus(el));
          }
        });
      },

      destroy() {
        if (fx) Alpine.release(fx);
      },

      focusFirst() {
        const el = this.$refs.typed && this.needsTyped ? this.$refs.typed : this.$refs.cancel;
        if (el) el.focus();
      },

      get busy() {
        return Alpine.store('confirm').busy;
      },

      get req() {
        return Alpine.store('confirm').request;
      },

      get visible() {
        return !!this.req;
      },

      get title() {
        return this.req ? this.req.title : '';
      },

      get effect() {
        return this.req ? this.req.effect : '';
      },

      get call() {
        return this.req ? this.req.call : '';
      },

      get hasCall() {
        return !!this.call;
      },

      get needsTyped() {
        return !!this.req && this.req.level === 'typed';
      },

      get typedLabel() {
        return this.req ? this.req.typedLabel : '';
      },

      get typedPlaceholder() {
        return this.req ? this.req.target : '';
      },

      get button() {
        return this.req ? this.req.button : '';
      },

      get buttonCls() {
        return this.req && this.req.destructive ? 'btn-danger-strong' : 'btn-accent';
      },

      get lanReason() {
        const s = Alpine.store('app').session;
        return s && s.lan ? s.lanReason || 'Writes are refused while Traefik listens beyond loopback.' : '';
      },

      get valid() {
        if (!this.req || this.lanReason) return false;
        if (!this.needsTyped) return true;
        return this.req.target !== '' && this.typed === this.req.target;
      },

      get disabled() {
        return !this.valid || this.busy;
      },

      get hasError() {
        return !!this.error;
      },

      async submit() {
        const r = this.req;
        if (!r || this.disabled) return;
        const st = Alpine.store('confirm');
        st.busy = true;
        this.error = '';
        try {
          const res = await I.client.action({ kind: r.kind, action: r.action, target: r.target, params: r.params, typed: this.typed, confirm: r.level }, { quiet: true });
          if (res && res.message) I.toast(res.message, 'ok');
          if (st.request === r) st.finish(res);
          else {
            st.busy = false;
            r.resolve(res);
          }
        } catch (e) {
          st.busy = false;
          this.error = e.message;
        }
      },

      cancel() {
        if (this.busy) return;
        Alpine.store('confirm').finish(null);
      },
    };
  });
});
