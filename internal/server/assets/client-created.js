// Copy-to-clipboard for any element with data-target pointing to an input/textarea.
document.querySelectorAll('.copy').forEach(function (b) {
  b.addEventListener('click', function () {
    var el = document.getElementById(b.dataset.target);
    navigator.clipboard.writeText(el.value).then(function () {
      var t = b.textContent; b.textContent = 'Copied ✓';
      setTimeout(function () { b.textContent = t; }, 1200);
    });
  });
});

// Tab switching for docker run variants.
document.querySelectorAll('.tab').forEach(function (tab) {
  tab.addEventListener('click', function () {
    document.querySelectorAll('.tab').forEach(function (t) {
      t.classList.remove('active');
      t.setAttribute('aria-selected', 'false');
    });
    document.querySelectorAll('.tab-panel').forEach(function (p) { p.hidden = true; });
    tab.classList.add('active');
    tab.setAttribute('aria-selected', 'true');
    var panel = document.getElementById('tab-' + tab.dataset.tab);
    if (panel) panel.hidden = false;
  });
});

// Confirm before leaving until the admin clicks "done" (don't lose secrets).
var saved = false;
document.getElementById('done').addEventListener('click', function () { saved = true; });
window.addEventListener('beforeunload', function (e) {
  if (!saved) { e.preventDefault(); e.returnValue = ''; }
});
