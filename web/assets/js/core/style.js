// Per-app stylesheet loading. Apps keep their CSS in /assets/css/apps/<id>.css and call
// ensureStyle() from mount(); the <link> is added once and kept (cheap, and avoids a flash
// of unstyled content when the user navigates back to the app).

const loaded = new Map();

/**
 * Adds <link rel="stylesheet" href="..."> to <head> once and resolves when it has loaded
 * (or failed — a missing stylesheet must never block an app from mounting).
 * @param {string} href absolute path under /assets/
 * @returns {Promise<void>}
 */
export function ensureStyle(href) {
  let p = loaded.get(href);
  if (p) return p;
  p = new Promise((resolve) => {
    const link = document.createElement('link');
    link.rel = 'stylesheet';
    link.href = href;
    link.addEventListener('load', () => resolve(), { once: true });
    link.addEventListener('error', () => resolve(), { once: true });
    document.head.appendChild(link);
  });
  loaded.set(href, p);
  return p;
}
