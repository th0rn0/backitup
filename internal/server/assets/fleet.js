(function () {
  'use strict';
  var POLL_MS = 5000;

  // State colour map — must match the CSS pill classes.
  var stateColours = {
    ok:      '#3ad29f',
    stale:   '#e8b64b',
    failed:  '#ff6b6b',
    never:   '#63707e',
    running: '#7c93ff'
  };

  // renderSparkline reads data-spark (JSON array of bytes, oldest first) from
  // a card and draws an SVG polyline. Returns nothing if fewer than 3 points.
  function renderSparkline(card) {
    var raw = card.getAttribute('data-spark');
    if (!raw) return;
    var data;
    try { data = JSON.parse(raw); } catch (e) { return; }
    if (!Array.isArray(data) || data.length < 3) return;

    // Remove any existing sparkline.
    var old = card.querySelector('.sparkline');
    if (old) old.parentNode.removeChild(old);

    var w = card.offsetWidth || 220;
    var h = 22;
    var min = data[0], max = data[0];
    for (var i = 1; i < data.length; i++) {
      if (data[i] < min) min = data[i];
      if (data[i] > max) max = data[i];
    }
    var range = max - min || 1;
    var step = w / (data.length - 1);
    var points = [];
    for (var j = 0; j < data.length; j++) {
      var x = j * step;
      var y = h - 2 - ((data[j] - min) / range) * (h - 4);
      points.push(x.toFixed(1) + ',' + y.toFixed(1));
    }

    var health = card.className.match(/health-(\w+)/);
    var colour = stateColours[health ? health[1] : 'ok'] || stateColours.ok;

    var svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    svg.setAttribute('class', 'sparkline');
    svg.setAttribute('viewBox', '0 0 ' + w + ' ' + h);
    svg.setAttribute('preserveAspectRatio', 'none');
    var poly = document.createElementNS('http://www.w3.org/2000/svg', 'polyline');
    poly.setAttribute('points', points.join(' '));
    poly.setAttribute('fill', 'none');
    poly.setAttribute('stroke', colour);
    poly.setAttribute('stroke-width', '1.6');
    poly.setAttribute('opacity', '0.75');
    svg.appendChild(poly);
    card.appendChild(svg);
  }

  function renderAllSparklines() {
    var cards = document.querySelectorAll('.fleet-card[data-spark]');
    for (var i = 0; i < cards.length; i++) {
      renderSparkline(cards[i]);
    }
  }

  function updateAggrTiles(summary) {
    var tiles = document.querySelectorAll('.aggr-tile');
    if (tiles.length < 4) return;
    var clientsTile = tiles[0];
    var needsYouTile = tiles[3];
    if (clientsTile) {
      var v = clientsTile.querySelector('.v');
      if (v) v.textContent = summary.total;
    }
    if (needsYouTile) {
      var v2 = needsYouTile.querySelector('.v');
      if (v2) v2.textContent = (summary.failed || 0) + (summary.stale || 0);
    }
  }

  function updateFleetMeta(summary) {
    updateAggrTiles(summary);
  }

  function updateCard(client) {
    var card = document.querySelector('.fleet-card[data-slug="' + client.slug + '"]');
    if (!card) return;

    // Update health class.
    card.className = card.className.replace(/health-\w+/g, '') + ' health-' + client.health;

    // Update pill.
    var pill = card.querySelector('.pill');
    if (pill) {
      pill.className = 'pill pill-' + client.health;
      pill.textContent = client.icon + ' ' + client.health_label;
    }

    var lbCell = card.querySelector('[data-field="last_backup"]');
    if (lbCell) lbCell.textContent = client.last_backup;

    var szCell = card.querySelector('[data-field="size"]');
    if (szCell) szCell.textContent = client.size;
  }

  function poll() {
    fetch('/api/v1/fleet', { credentials: 'same-origin' })
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (data) {
        if (!data) return;
        updateFleetMeta(data.summary);
        for (var i = 0; i < data.clients.length; i++) {
          updateCard(data.clients[i]);
        }
      })
      .catch(function () {})
      .then(function () { setTimeout(poll, POLL_MS); });
  }

  // Render sparklines on load.
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', renderAllSparklines);
  } else {
    renderAllSparklines();
  }

  setTimeout(poll, POLL_MS);
})();
