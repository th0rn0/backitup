// offsite-dir.js: show/hide and relabel the backup-location field based on
// the selected rclone remote type. Loaded by clients_new.html and
// client_detail.html; requires id="offsite_remote" on the select and
// id="offsite-dir-row/label/input/hint" on the field group.
(function () {
  function applyRemote(remote, clearValue) {
    var row   = document.getElementById('offsite-dir-row');
    var label = document.getElementById('offsite-dir-label');
    var input = document.getElementById('offsite-dir-input');
    var hint  = document.getElementById('offsite-dir-hint');
    if (!row) return;

    if (clearValue && input) input.value = '';

    if (remote === 'gdrive') {
      row.style.display = '';
      if (label) label.textContent = 'GDrive Folder ID (optional)';
      if (input) input.placeholder = '0BwwA4oUTeiV1TGRPeTVjaWRDY1E';
      if (hint)  hint.textContent  = 'Paste the folder ID from the Drive URL (.../drive/folders/{ID}). Leave empty to upload to the Drive root.';
    } else if (remote === 's3') {
      row.style.display = '';
      if (label) label.textContent = 'Backup path (optional)';
      if (input) input.placeholder = 'Backups/MyLaptop';
      if (hint)  hint.textContent  = 'Path prefix within the S3 bucket. Leave empty to use the client name.';
    } else {
      row.style.display = 'none';
    }
  }

  var sel = document.getElementById('offsite_remote');
  if (!sel) return;

  applyRemote(sel.value, false);
  sel.addEventListener('change', function () { applyRemote(this.value, true); });
}());
