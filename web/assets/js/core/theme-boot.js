// Classic (non-module) script loaded synchronously in <head>: applies a stored
// light/dark override before first paint so there is no theme flash. Kept tiny
// and external because the CSP forbids inline scripts.
(function () {
  try {
    var t = window.localStorage.getItem('lw.theme');
    if (t === 'light' || t === 'dark') document.documentElement.setAttribute('data-theme', t);
  } catch (e) { /* storage blocked: fall back to the system theme */ }
})();
