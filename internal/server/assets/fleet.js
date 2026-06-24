(function () {
  'use strict';
  var POLL_MS = 5000;

  function updateFleetMeta(summary) {
    var meta = document.querySelector('.fleet-meta');
    if (!meta) return;

    var ok = meta.querySelector('.ok');
    var stale = meta.querySelector('.stale');
    var failed = meta.querySelector('.failed');
    var never = meta.querySelector('.never');
    if (ok) ok.textContent = summary.ok + ' OK';
    if (stale) stale.textContent = summary.stale + ' stale';
    if (failed) failed.textContent = summary.failed + ' failed';
    if (never) never.textContent = summary.never + ' never';

    var running = meta.querySelector('.running');
    if (summary.running > 0) {
      if (!running) {
        running = document.createElement('span');
        running.className = 'running';
        meta.insertBefore(running, meta.firstChild);
      }
      running.textContent = summary.running + ' running';
    } else if (running) {
      running.parentNode.removeChild(running);
    }
  }

  function updateRow(client) {
    var row = document.querySelector('tr[data-slug="' + client.slug + '"]');
    if (!row) return;

    var statusCell = row.querySelector('[data-field="status"]');
    if (statusCell) {
      var badge = statusCell.querySelector('.status');
      if (badge) {
        badge.className = 'status ' + client.health;
        var dot = badge.querySelector('.dot');
        if (dot) dot.textContent = client.icon;
        var label = badge.querySelector('.label');
        if (label) label.textContent = client.health_label;
      }
    }

    var lbCell = row.querySelector('[data-field="last_backup"]');
    if (lbCell) lbCell.textContent = client.last_backup;

    var szCell = row.querySelector('[data-field="size"]');
    if (szCell) szCell.textContent = client.size;
  }

  function poll() {
    fetch('/api/v1/fleet', { credentials: 'same-origin' })
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (data) {
        if (!data) return;
        updateFleetMeta(data.summary);
        for (var i = 0; i < data.clients.length; i++) {
          updateRow(data.clients[i]);
        }
      })
      .catch(function () {})
      .then(function () { setTimeout(poll, POLL_MS); });
  }

  setTimeout(poll, POLL_MS);
})();
