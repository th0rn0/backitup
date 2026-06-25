(function () {
  // Select-all checkbox wires to row checkboxes in the same data-select-group.
  document.querySelectorAll('[data-select-all]').forEach(function (master) {
    var group = master.dataset.selectAll;
    master.addEventListener('change', function () {
      document.querySelectorAll('input[data-select-group="' + group + '"]').forEach(function (cb) {
        cb.checked = master.checked;
      });
    });
    // Keep master in sync when individual boxes change.
    document.querySelectorAll('input[data-select-group="' + group + '"]').forEach(function (cb) {
      cb.addEventListener('change', function () {
        var all = document.querySelectorAll('input[data-select-group="' + group + '"]');
        var checked = document.querySelectorAll('input[data-select-group="' + group + '"]:checked');
        master.indeterminate = checked.length > 0 && checked.length < all.length;
        master.checked = checked.length === all.length;
      });
    });
  });

  // Bulk-delete button: collect checked boxes and POST their values.
  document.querySelectorAll('[data-bulk-action]').forEach(function (btn) {
    btn.addEventListener('click', function () {
      var group = btn.dataset.bulkGroup;
      var checked = Array.from(
        document.querySelectorAll('input[data-select-group="' + group + '"]:checked')
      ).map(function (cb) { return cb.value; });
      if (checked.length === 0) {
        alert('Select at least one item.');
        return;
      }
      if (!confirm('Delete ' + checked.length + ' item(s)? This cannot be undone.')) return;
      var form = document.createElement('form');
      form.method = 'POST';
      form.action = btn.dataset.bulkAction;
      checked.forEach(function (id) {
        var inp = document.createElement('input');
        inp.type = 'hidden';
        inp.name = 'ids';
        inp.value = id;
        form.appendChild(inp);
      });
      document.body.appendChild(form);
      form.submit();
    });
  });

  // data-confirm on forms replaces inline onsubmit confirm().
  document.querySelectorAll('form[data-confirm]').forEach(function (form) {
    form.addEventListener('submit', function (e) {
      if (!confirm(form.dataset.confirm)) e.preventDefault();
    });
  });
})();
