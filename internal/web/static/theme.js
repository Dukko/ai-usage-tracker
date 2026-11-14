// Runs before first paint: when "follow system" is on, pick the light or dark theme.
(function () {
  var h = document.documentElement, light = h.dataset.themeLight, dark = h.dataset.themeDark;
  if (!light || !dark) return;
  var mq = window.matchMedia('(prefers-color-scheme: dark)');
  function apply() { h.dataset.theme = mq.matches ? dark : light; }
  apply();
  mq.addEventListener('change', apply);
})();
