// Directory entry point so `node --test web/tests/vault/` works: Node 24's test runner treats each
// argument as a file or glob, so a directory resolves to this index.js, which loads every suite.
// Equivalent (one process per file): node --test "web/tests/vault/*.test.mjs"
const { readdirSync } = require('node:fs');
const { join } = require('node:path');
const { pathToFileURL } = require('node:url');

(async () => {
  const suites = readdirSync(__dirname).filter((n) => n.endsWith('.test.mjs')).sort();
  for (const name of suites) await import(pathToFileURL(join(__dirname, name)).href);
})();
