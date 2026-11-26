// offsite-dir.js: show/hide and relabel the backup-location field based on
// the selected remote's backend type (read from data-backend on the option).
// Loaded by clients_new.html and client_detail.html.
(function () {
  // Hints keyed by rclone backend type. Offsite storage is B2 only; anything
  // else here (a remote an older backitup configured) falls through to the
  // generic label, and the remote page tells the operator to recreate it.
  var HINTS = {
    b2:         { label: 'Path inside the bucket (optional)', placeholder: 'clients', hint: 'Path below the remote\'s bucket — and below its prefix, if it has one. Leave empty to use the client name.' },
    crypt:      { label: 'Encrypted path (optional)',   placeholder: 'clients',                     hint: 'Path inside the encrypted remote. Leave empty to store the client name as an encrypted directory. Everything under it — names included — reaches the provider as ciphertext.' },
  };

  function applyRemote(sel, clearValue) {
    var row    = document.getElementById('offsite-dir-row');
    var label  = document.getElementById('offsite-dir-label');
    var input  = document.getElementById('offsite-dir-input');
    var hint   = document.getElementById('offsite-dir-hint');
    var intRow = document.getElementById('offsite-interval-row');
    var prefixRow = document.getElementById('offsite-prefix-row');
    if (!row) return;

    var opt     = sel.options[sel.selectedIndex];
    var backend = opt ? (opt.getAttribute('data-backend') || '') : '';
    var remote  = sel.value;

    if (clearValue && input) input.value = '';

    var show = !!remote;
    row.classList.toggle('hidden', !show);
    if (intRow) intRow.classList.toggle('hidden', !show);
    if (prefixRow) prefixRow.classList.toggle('hidden', !show);

    if (!show) return;

    var info = HINTS[backend] || { label: 'Backup path (optional)', placeholder: '', hint: '' };
    if (label) label.textContent = info.label;
    if (input) input.placeholder = info.placeholder;
    if (hint)  hint.textContent  = info.hint;
  }

  // The backup-name prefix field: show what the object will be called as the
  // operator types, or while the "no prefix" box is ticked. The directory and
  // the unprefixed name come from the server (data-*), so this never re-derives
  // the naming rule in the browser — it only moves the prefix in front of it.
  function updatePrefixExample() {
    var ex     = document.getElementById('offsite-prefix-example');
    var input  = document.getElementById('name_prefix');
    if (!ex || !input) return;
    var none   = document.getElementById('name_prefix_none');
    var legacy = ex.getAttribute('data-legacy') || '';
    var fallback = ex.getAttribute('data-fallback') || '';
    var prefix = (none && none.checked) ? '' : (input.value.trim() || fallback);
    var name = prefix ? prefix + '-' + legacy : legacy;
    var dir = ex.getAttribute('data-dir') || '';
    if (dir) name = dir + '/' + name;
    ex.textContent = name;
  }

  var prefixInput = document.getElementById('name_prefix');
  var prefixNone  = document.getElementById('name_prefix_none');
  if (prefixInput) {
    prefixInput.addEventListener('input', updatePrefixExample);
    if (prefixNone) prefixNone.addEventListener('change', updatePrefixExample);
    updatePrefixExample();
  }

  var sel = document.getElementById('offsite_remote');
  if (sel) {
    applyRemote(sel, false);
    sel.addEventListener('change', function () { applyRemote(this, true); });
  }
}());

// Tab switcher for elements with data-tab-group / data-tab-btn / data-tab.
// scopedQuery only returns elements whose nearest data-tab-group ancestor is
// exactly `group`, so nested tab groups don't bleed into each other.
// Unselected panels use the `hidden` attribute so they are absent from the
// accessibility tree, not merely invisible.
(function () {
  function scopedQuery(group, selector) {
    return Array.from(group.querySelectorAll(selector)).filter(function (el) {
      return el.closest('[data-tab-group]') === group;
    });
  }

  function activate(group, target) {
    scopedQuery(group, '[data-tab-btn]').forEach(function (b) {
      var isActive = b.getAttribute('data-tab-btn') === target;
      b.classList.toggle('tab-active', isActive);
      b.setAttribute('aria-selected', isActive ? 'true' : 'false');
    });
    scopedQuery(group, '[data-tab]').forEach(function (p) {
      if (p.getAttribute('data-tab') === target) {
        p.removeAttribute('hidden');
      } else {
        p.setAttribute('hidden', '');
      }
    });
  }

  document.addEventListener('click', function (e) {
    var btn = e.target.closest('[data-tab-btn]');
    if (!btn) return;
    var group = btn.closest('[data-tab-group]');
    if (!group) return;
    var target = btn.getAttribute('data-tab-btn');
    activate(group, target);
  });

  // Keyboard navigation: arrow keys move between tabs in the same tablist.
  document.addEventListener('keydown', function (e) {
    var btn = e.target.closest('[role="tab"]');
    if (!btn) return;
    var tablist = btn.closest('[role="tablist"]');
    if (!tablist) return;
    var tabs = Array.from(tablist.querySelectorAll('[role="tab"]'));
    var idx = tabs.indexOf(btn);
    if (idx < 0) return;
    var next = -1;
    if (e.key === 'ArrowRight' || e.key === 'ArrowDown') {
      next = (idx + 1) % tabs.length;
    } else if (e.key === 'ArrowLeft' || e.key === 'ArrowUp') {
      next = (idx - 1 + tabs.length) % tabs.length;
    } else if (e.key === 'Home') {
      next = 0;
    } else if (e.key === 'End') {
      next = tabs.length - 1;
    }
    if (next >= 0) {
      e.preventDefault();
      tabs[next].focus();
      tabs[next].click();
    }
  });

  // Initialise each group by activating its first directly-owned button.
  document.querySelectorAll('[data-tab-group]').forEach(function (group) {
    var first = scopedQuery(group, '[data-tab-btn]')[0];
    if (first) {
      activate(group, first.getAttribute('data-tab-btn'));
    }
  });
}());
