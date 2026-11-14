// Small progressive enhancements. Every page works without this file.
document.addEventListener('submit', function (e) {
  var msg = e.target.getAttribute('data-confirm');
  if (msg && !window.confirm(msg)) e.preventDefault();
});

document.addEventListener('DOMContentLoaded', function () {
  // The header theme button returns to the page it was pressed on.
  document.querySelectorAll('[data-current-url]').forEach(function (el) {
    el.value = location.pathname + location.search;
  });

  // Live labels and preview bar for the warning/critical sliders.
  var warn = document.getElementById('warn'), crit = document.getElementById('crit');
  var bar = document.querySelector('[data-thresholds]');
  function sync() {
    if (!warn || !crit) return;
    document.querySelector('[data-out=warn]').textContent = warn.value + '%';
    document.querySelector('[data-out=crit]').textContent = crit.value + '%';
    if (bar) {
      bar.querySelector('.th-ok').style.flexBasis = warn.value + '%';
      bar.querySelector('.th-crit').style.flexBasis = (100 - crit.value) + '%';
    }
  }
  if (warn && crit) {
    warn.addEventListener('input', sync);
    crit.addEventListener('input', sync);
  }

  // Click a one-time token to select it for copying.
  document.querySelectorAll('[data-copy]').forEach(function (el) {
    el.addEventListener('click', function () {
      var r = document.createRange();
      r.selectNodeContents(el);
      var s = window.getSelection();
      s.removeAllRanges();
      s.addRange(r);
    });
  });

  // Only one pop-over open at a time.
  document.querySelectorAll('details.pop').forEach(function (d) {
    d.addEventListener('toggle', function () {
      if (!d.open) return;
      document.querySelectorAll('details.pop[open]').forEach(function (o) { if (o !== d) o.open = false; });
    });
  });
});
