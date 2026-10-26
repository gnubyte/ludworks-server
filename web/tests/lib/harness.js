// Manual/automated browser harness for the Markdown libraries (not part of the app bundle).
import { createRichEditor, domToMarkdown } from '../../assets/js/lib/richeditor.js';
import { createMarkdownEditor } from '../../assets/js/lib/mdeditor.js';
import { renderToDOM, toggleTask } from '../../assets/js/lib/markdown.js';

const SAMPLE = [
  '# Weekend plans',
  '',
  'A **bold** start with *emphasis*, ~~strike~~, `inline code` and a [link](https://example.com "Example").',
  'Second line of the same paragraph; bare link https://ludworks.example/docs.',
  '',
  '## Checklist',
  '',
  '- [ ] Buy groceries',
  '- [x] Clean the garage',
  '  - [ ] Sort the shelves',
  '- [ ]',
  '',
  '1. First',
  '2. Second',
  '   - nested bullet',
  '',
  '> Quote with a list:',
  '>',
  '> - one',
  '',
  '```js',
  'function greet(name) {',
  '  return `Hello, ${name}!`; // template',
  '}',
  '```',
  '',
  '| Item | Qty | Price |',
  '| :--- | :---: | ---: |',
  '| Apples | 3 | 1.20 |',
  '| Pears | 2 | 0.90 |',
  '',
  '---',
  '',
  'Unsafe [click](javascript:alert(1)) stays text. <script>alert(1)</script>',
].join('\n');

const violations = [];
document.addEventListener('securitypolicyviolation', (e) => {
  violations.push(`${e.violatedDirective} ${e.blockedURI}`);
  const el = document.getElementById('csp');
  el.textContent = `CSP violations: ${violations.length}`;
  el.className = 'hx-bad';
});

document.getElementById('theme').addEventListener('click', () => {
  const html = document.documentElement;
  const dark = html.getAttribute('data-theme') === 'dark' || (!html.getAttribute('data-theme') && matchMedia('(prefers-color-scheme: dark)').matches);
  html.setAttribute('data-theme', dark ? 'light' : 'dark');
});

const changes = [];
const upload = (file) => new Promise((resolve) => setTimeout(() => resolve(URL.createObjectURL(file)), 300));

const richOut = document.getElementById('rich-md');
const rich = createRichEditor({
  container: document.getElementById('rich'),
  markdown: SAMPLE,
  placeholder: 'Start writing your note' + String.fromCharCode(0x2026),
  onChange: (md) => { changes.push(md); richOut.textContent = md; },
  onUploadImage: upload,
});
richOut.textContent = rich.getMarkdown();

const source = createMarkdownEditor({
  container: document.getElementById('source'),
  markdown: SAMPLE,
  onChange: (md) => renderView(md),
  onUploadImage: upload,
});

const view = document.getElementById('view');
let viewMd = SAMPLE;
function renderView(md) {
  viewMd = md;
  view.replaceChildren(renderToDOM(md, document, {
    onTaskToggle: (line, checked) => {
      const next = toggleTask(viewMd, line, checked);
      source.setMarkdown(next);
      renderView(next);
    },
  }));
}
renderView(SAMPLE);

// Hooks for automated checks from the browser tools.
window.__harness = { rich, source, renderView, changes, violations, domToMarkdown, SAMPLE };
