// remote-form.js: shows the correct backend-specific fieldset when the user
// picks a backend from the "Add remote" dropdown.
(function () {
  var sel = document.getElementById('add-remote-backend');
  if (!sel) return;

  function switchBackend(backend) {
    document.querySelectorAll('[data-backend-fields]').forEach(function (fs) {
      var match = fs.getAttribute('data-backend-fields') === backend;
      fs.style.display = match ? '' : 'none';
      fs.querySelectorAll('input,textarea,select').forEach(function (el) {
        el.disabled = !match;
      });
    });
    var submit = document.getElementById('add-remote-submit');
    if (submit) submit.style.display = backend ? '' : 'none';
  }

  switchBackend(sel.value);
  sel.addEventListener('change', function () { switchBackend(this.value); });
}());
