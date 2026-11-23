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
    if (!row) return;

    var opt     = sel.options[sel.selectedIndex];
    var backend = opt ? (opt.getAttribute('data-backend') || '') : '';
    var remote  = sel.value;

    if (clearValue && input) input.value = '';

    var show = !!remote;
    row.classList.toggle('hidden', !show);
    if (intRow) intRow.classList.toggle('hidden', !show);

    if (!show) return;

    var info = HINTS[backend] || { label: 'Backup path (optional)', placeholder: '', hint: '' };
    if (label) label.textContent = info.label;
    if (input) input.placeholder = info.placeholder;
    if (hint)  hint.textContent  = info.hint;
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
(function () {
  function scopedQuery(group, selector) {
    return Array.from(group.querySelectorAll(selector)).filter(function (el) {
      return el.closest('[data-tab-group]') === group;
    });
  }

  document.addEventListener('click', function (e) {
    var btn = e.target.closest('[data-tab-btn]');
    if (!btn) return;
    var group = btn.closest('[data-tab-group]');
    if (!group) return;
    var target = btn.getAttribute('data-tab-btn');

    scopedQuery(group, '[data-tab-btn]').forEach(function (b) {
      b.classList.toggle('tab-active', b.getAttribute('data-tab-btn') === target);
    });
    scopedQuery(group, '[data-tab]').forEach(function (p) {
      p.style.display = p.getAttribute('data-tab') === target ? '' : 'none';
    });
  });

  // Initialise each group by clicking its first directly-owned button.
  document.querySelectorAll('[data-tab-group]').forEach(function (group) {
    var first = scopedQuery(group, '[data-tab-btn]')[0];
    if (first) first.click();
  });
}());
