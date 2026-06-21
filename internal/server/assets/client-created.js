document.querySelectorAll('.copy').forEach(function (b) {
  b.addEventListener('click', function () {
    var el = document.getElementById(b.dataset.target);
    navigator.clipboard.writeText(el.value).then(function () {
      var t = b.textContent; b.textContent = 'Copied ✓';
      setTimeout(function () { b.textContent = t; }, 1200);
    });
  });
});
// Confirm before leaving until the admin clicks "done" (don't lose secrets).
var saved = false;
document.getElementById('done').addEventListener('click', function () { saved = true; });
window.addEventListener('beforeunload', function (e) {
  if (!saved) { e.preventDefault(); e.returnValue = ''; }
});
