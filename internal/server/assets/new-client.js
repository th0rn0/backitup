// new-client.js: live Docker command preview on the add-client form.
// Reads server config from data-* attributes on #docker-meta, then rebuilds
// the preview textarea on every change to backup_path, secrets_path, or
// api_scheme.
(function () {
  var meta = document.getElementById('docker-meta');
  if (!meta) return;

  var publicAPI   = meta.getAttribute('data-public-api')   || '';
  var publicHost  = meta.getAttribute('data-public-host')  || 'YOUR-SERVER:2222';
  var clientImage = meta.getAttribute('data-client-image') || 'th0rn0/backitup-client:latest';

  function apiBase() {
    if (publicAPI) return publicAPI;
    var radio = document.querySelector('input[name="api_scheme"]:checked');
    var scheme = radio ? radio.value : 'http';
    // publicHost is host:sshport; for the API URL strip the ssh port and use 8080
    var host = publicHost.split(':')[0] || publicHost;
    return scheme + '://' + host + ':8080';
  }

  function val(id, fallback) {
    var el = document.getElementById(id);
    return (el && el.value.trim()) ? el.value.trim() : fallback;
  }

  function buildCmd(backupPath, secretsPath) {
    var api = apiBase();
    return [
      'docker run --rm \\',
      '  --user $(id -u):$(id -g) \\',
      '  --mount type=bind,src=' + backupPath + ',dst=/source,readonly \\',
      '  -v ' + secretsPath + ':/secrets:ro \\',
      '  -e BACKITUP_API=' + api + ' \\',
      '  -e BACKITUP_SERVER=' + publicHost + ' \\',
      '  -e BACKITUP_TOKEN=<YOUR_TOKEN> \\',
      '  -e BACKITUP_SSH_KEY=/secrets/id \\',
      '  -e BACKITUP_KNOWN_HOSTS=/secrets/known_hosts \\',
      '  ' + clientImage,
    ].join('\n');
  }

  function update() {
    var nameEl     = document.getElementById('name');
    var clientName = (nameEl && nameEl.value.trim()) ? nameEl.value.trim() : 'client-name';
    var backupPath  = val('backup_path',  '/PATH/TO/BACKUP');
    var secretsPath = val('secrets_path', '/etc/backitup/' + clientName);
    var preview = document.getElementById('docker-preview');
    if (preview) preview.value = buildCmd(backupPath, secretsPath);

    // Update secrets_path placeholder to track the client name.
    var spEl = document.getElementById('secrets_path');
    if (spEl && !spEl.value) spEl.placeholder = '/etc/backitup/' + clientName;
  }

  ['backup_path', 'secrets_path', 'name'].forEach(function (id) {
    var el = document.getElementById(id);
    if (el) el.addEventListener('input', update);
  });
  document.querySelectorAll('input[name="api_scheme"]').forEach(function (r) {
    r.addEventListener('change', update);
  });

  update();
}());
