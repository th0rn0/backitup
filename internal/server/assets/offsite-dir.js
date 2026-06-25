// offsite-dir.js: show/hide and relabel the backup-location field based on
// the selected rclone remote type. Loaded by clients_new.html and
// client_detail.html; requires id="offsite_remote" on the select and
// id="offsite-dir-row/label/input/hint" on the field group.
(function () {
  function applyRemote(remote, clearValue) {
    var row      = document.getElementById('offsite-dir-row');
    var label    = document.getElementById('offsite-dir-label');
    var input    = document.getElementById('offsite-dir-input');
    var hint     = document.getElementById('offsite-dir-hint');
    var intRow   = document.getElementById('offsite-interval-row');
    if (!row) return;

    if (clearValue && input) input.value = '';

    if (remote === 'gdrive') {
      row.style.display = '';
      if (label) label.textContent = 'GDrive Folder ID (optional)';
      if (input) input.placeholder = '0BwwA4oUTeiV1TGRPeTVjaWRDY1E';
      if (hint)  hint.textContent  = 'Paste the folder ID from the Drive URL (.../drive/folders/{ID}). Leave empty to upload to the Drive root.';
      if (intRow) intRow.style.display = '';
    } else if (remote === 's3') {
      row.style.display = '';
      if (label) label.textContent = 'Backup path (optional)';
      if (input) input.placeholder = 'Backups/MyLaptop';
      if (hint)  hint.textContent  = 'Path prefix within the S3 bucket. Leave empty to use the client name.';
      if (intRow) intRow.style.display = '';
    } else {
      row.style.display = 'none';
      if (intRow) intRow.style.display = 'none';
    }
  }

  var sel = document.getElementById('offsite_remote');
  if (sel) {
    applyRemote(sel.value, false);
    sel.addEventListener('change', function () { applyRemote(this.value, true); });
  }
}());

// Tab switcher for elements with data-tab-group / data-tab attributes.
// Clicking a [data-tab-btn] within a [data-tab-group] shows the matching
// [data-tab] panel and hides the rest. No global state; each group is independent.
(function () {
  document.addEventListener('click', function (e) {
    var btn = e.target.closest('[data-tab-btn]');
    if (!btn) return;
    var group = btn.closest('[data-tab-group]');
    if (!group) return;
    var target = btn.getAttribute('data-tab-btn');

    group.querySelectorAll('[data-tab-btn]').forEach(function (b) {
      b.classList.toggle('tab-active', b.getAttribute('data-tab-btn') === target);
    });
    group.querySelectorAll('[data-tab]').forEach(function (p) {
      p.style.display = p.getAttribute('data-tab') === target ? '' : 'none';
    });
  });

  // Activate the first tab in each group on load.
  document.querySelectorAll('[data-tab-group]').forEach(function (group) {
    var first = group.querySelector('[data-tab-btn]');
    if (first) first.click();
  });
}());
