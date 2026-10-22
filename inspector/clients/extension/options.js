(function () {
  const C = window.RollInspector;
  const $ = (id) => document.getElementById(id);
  const ALL = { origins: ['<all_urls>'] };

  function el(tag, cls, text) {
    const n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text) n.textContent = text;
    return n;
  }

  function load() {
    chrome.storage.local.get(null, (all) => {
      $('override').value = all.override || '';
      const list = $('found');
      while (list.firstChild) list.removeChild(list.firstChild);
      const keys = Object.keys(all)
        .filter((k) => k.startsWith('found:'))
        .sort();
      $('none').hidden = keys.length > 0;
      for (const k of keys) {
        const li = el('li', 'found-row');
        li.appendChild(el('span', 'mono', k.slice(6)));
        li.appendChild(el('span', 'soft', '→'));
        li.appendChild(el('span', 'mono grow', all[k]));
        const forget = el('button', 'btn btn-ghost', 'Forget');
        forget.type = 'button';
        forget.addEventListener('click', () => chrome.storage.local.remove(k, load));
        li.appendChild(forget);
        list.appendChild(li);
      }
    });
    chrome.permissions.contains(ALL, (yes) => {
      $('perm').textContent = yes ? 'Every site is allowed.' : 'Only https://*.test is allowed.';
      $('grant').hidden = yes;
      $('revoke').hidden = !yes;
    });
  }

  $('form').addEventListener('submit', (e) => {
    e.preventDefault();
    const raw = $('override').value;
    const base = C.normalizeBase(raw);
    if (raw.trim() && !base) {
      $('saved').textContent = 'That is not an http(s) address.';
      return;
    }
    chrome.storage.local.set({ override: base }, () => {
      $('saved').textContent = base ? 'Saved. Reopen DevTools on the shop page to use ' + base + '.' : 'Cleared.';
      load();
    });
  });

  $('reset').addEventListener('click', () => {
    chrome.storage.local.remove('override', () => {
      $('saved').textContent = 'Cleared. The panel discovers Inspector from the shop host again.';
      load();
    });
  });

  // Chrome asks for an optional permission only from a click.
  $('grant').addEventListener('click', () => chrome.permissions.request(ALL, load));
  $('revoke').addEventListener('click', () => chrome.permissions.remove(ALL, load));

  load();
})();
