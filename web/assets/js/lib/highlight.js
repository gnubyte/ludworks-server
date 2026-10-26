/**
 * highlight.js — small, dependency-free, tokenizer-based syntax highlighter.
 *
 * Produces DOM (text nodes + <span class="hl-*">) and never touches innerHTML, so it is safe
 * under the app's strict CSP. Token classes: hl-keyword, hl-string, hl-number, hl-comment,
 * hl-function, hl-type, hl-operator, hl-punctuation, hl-variable, hl-tag, hl-attr, hl-meta,
 * plus hl-inserted / hl-deleted for diffs.
 *
 * Every regex here is sticky and written so that it cannot backtrack catastrophically on
 * untrusted input (notes and shared files are user content); lookaheads are bounded. A rule
 * that can fail must also fail cheaply: one that scans to the end of the line and then gives
 * up, consuming a single character, is retried from the next character and turns quadratic
 * on one long line (e.g. a megabyte of "["). Unterminated constructs therefore consume to
 * the end of the line in one match, and the Markdown inline scanner caches its look-aheads.
 *
 * @module lib/highlight
 */

/**
 * Inputs longer than this are rendered as plain text to keep the UI responsive. Highlighting
 * runs synchronously on the main thread, so this also bounds how long a crafted shared file
 * can block the page.
 */
export const MAX_HIGHLIGHT_CHARS = 256 * 1024;

/**
 * Wall-clock budget for one tokenize() call. Exceeding it renders the input as plain text: a
 * last line of defence should some lexer still turn out to be super-linear on crafted input.
 */
export const HIGHLIGHT_BUDGET_MS = 500;

const words = (s) => new Set((s || '').split(/\s+/).filter(Boolean));
const lower = (set) => new Set([...set].map((w) => w.toLowerCase()));

const clock = () => (globalThis.performance?.now ? globalThis.performance.now() : Date.now());
const OVER_BUDGET = Symbol('highlight budget exceeded');

/** Accumulates tokens, merging adjacent tokens of the same type. */
class TokenList {
  constructor(deadline = Infinity) {
    this.tokens = [];
    this.deadline = deadline;
    this.pushes = 0;
  }
  push(type, value) {
    if (!value) return;
    if ((++this.pushes & 0x3ff) === 0 && this.deadline !== Infinity && clock() > this.deadline) throw OVER_BUDGET;
    const last = this.tokens[this.tokens.length - 1];
    if (last && last.type === type) last.value += value;
    else this.tokens.push({ type, value });
  }
}

const reWs = /[ \t\r\n\f\v]+/y;
const reNumber = /(?:0[xX][0-9a-fA-F_]+|0[bB][01_]+|0[oO][0-7_]+|(?:\d[\d_]*(?:\.[\d_]*)?|\.\d[\d_]*)(?:[eE][+-]?\d[\d_]*)?)[a-zA-Z]{0,4}/y;
const reOperator = /[+\-*/%=&|^!<>?~]+/y;
const rePunct = /[()[\]{}.,;:@\\]/y;
const reIdent = /[\p{L}_$][\p{L}\p{N}_$]*/uy;

const isDigit = (c) => c >= '0' && c <= '9';

/** Next non-blank char on the same line (bounded look-ahead). */
function nextNonSpace(src, i) {
  const end = Math.min(src.length, i + 64);
  for (; i < end; i++) {
    const c = src[i];
    if (c !== ' ' && c !== '\t') return c;
  }
  return '';
}

function isCapitalized(w) {
  const c = w.charCodeAt(0);
  // PascalCase identifiers (but not ALL_CAPS constants) are treated as type names.
  return c >= 65 && c <= 90 && /[a-z]/.test(w);
}

function sticky(re) {
  return new RegExp(re.source, re.flags.includes('y') ? re.flags : re.flags + 'y');
}

/**
 * Builds a lexer for C-family-ish languages from a declarative config.
 * cfg.rules: [regex, type | fn(match, prev) -> type | [[type, text], ...], {first?, when?}]
 */
function makeLexer(cfg) {
  const ci = !!cfg.caseInsensitive;
  const fold = ci ? (w) => w.toLowerCase() : (w) => w;
  let kw = words(cfg.keywords), lit = words(cfg.literals), types = words(cfg.types);
  if (ci) { kw = lower(kw); lit = lower(lit); types = lower(types); }
  const funcKw = words(cfg.funcKeywords), typeKw = words(cfg.typeKeywords);
  const rules = (cfg.rules || []).map(([re, type, o = {}]) => ({ re: sticky(re), type, first: o.first, when: o.when }));
  const ident = cfg.ident ? sticky(cfg.ident) : reIdent;
  const operator = cfg.operator ? sticky(cfg.operator) : reOperator;
  const classifyIdent = cfg.classifyIdent;

  return function lex(src, out) {
    let pos = 0, prev = null;
    const n = src.length;
    main: while (pos < n) {
      const ch = src[pos];
      if (ch === ' ' || ch === '\t' || ch === '\n' || ch === '\r') {
        reWs.lastIndex = pos; reWs.exec(src);
        out.push(null, src.slice(pos, reWs.lastIndex));
        pos = reWs.lastIndex;
        continue;
      }
      for (const r of rules) {
        if (r.first && !r.first.includes(ch)) continue;
        if (r.when && !r.when(prev, src, pos)) continue;
        r.re.lastIndex = pos;
        const m = r.re.exec(src);
        if (!m || !m[0]) continue;
        const t = typeof r.type === 'function' ? r.type(m, prev, src, pos) : r.type;
        if (t === undefined) continue; // rule declined
        let lastType = t;
        if (Array.isArray(t)) { for (const [tt, s] of t) out.push(tt, s); lastType = t.length ? t[t.length - 1][0] : null; }
        else out.push(t, m[0]);
        if (lastType !== 'comment') prev = { type: lastType || 'text', text: m[0] };
        pos += m[0].length;
        continue main;
      }
      ident.lastIndex = pos;
      let m = ident.exec(src);
      if (m) {
        const w = m[0], key = fold(w), end = pos + w.length;
        let type = classifyIdent ? classifyIdent(w, prev, src, end) : undefined;
        const member = prev && prev.type === 'punctuation' && prev.text === '.';
        if (type === undefined && member) {
          // obj.type / obj.get(): member names are never keywords.
          type = nextNonSpace(src, end) === '(' ? 'function' : null;
        } else if (type === undefined) {
          type = null;
          if (kw.has(key) || lit.has(key)) type = 'keyword';
          else if (prev && prev.type === 'keyword' && funcKw.has(fold(prev.text))) type = 'function';
          else if (prev && prev.type === 'keyword' && typeKw.has(fold(prev.text))) type = 'type';
          else if (types.has(key)) type = 'type';
          else if (nextNonSpace(src, end) === '(') type = 'function';
          else if (cfg.capitalTypes && isCapitalized(w)) type = 'type';
        }
        out.push(type, w);
        prev = { type: type || 'ident', text: w };
        pos = end;
        continue;
      }
      if (isDigit(ch) || (ch === '.' && isDigit(src[pos + 1] || ''))) {
        reNumber.lastIndex = pos; m = reNumber.exec(src);
        if (m) { out.push('number', m[0]); prev = { type: 'number', text: m[0] }; pos += m[0].length; continue; }
      }
      operator.lastIndex = pos; m = operator.exec(src);
      if (m) { out.push('operator', m[0]); prev = { type: 'operator', text: m[0] }; pos += m[0].length; continue; }
      rePunct.lastIndex = pos; m = rePunct.exec(src);
      if (m) { out.push('punctuation', m[0]); prev = { type: 'punctuation', text: m[0] }; pos += 1; continue; }
      // Unknown char: keep surrogate pairs together.
      const cc = src.charCodeAt(pos);
      const len = cc >= 0xd800 && cc <= 0xdbff && pos + 1 < n ? 2 : 1;
      out.push(null, src.slice(pos, pos + len));
      prev = { type: 'text', text: src.slice(pos, pos + len) };
      pos += len;
    }
  };
}

// ---------------------------------------------------------------------------------------------
// Shared rule fragments

const R = {
  slashComment: [/\/\/[^\n]*/, 'comment', { first: '/' }],
  blockComment: [/\/\*[\s\S]*?(?:\*\/|$)/, 'comment', { first: '/' }],
  hashComment: [/#[^\n]*/, 'comment', { first: '#' }],
  dq: [/"(?:[^"\\\n]|\\[\s\S])*"?/, 'string', { first: '"' }],
  sq: [/'(?:[^'\\\n]|\\[\s\S])*'?/, 'string', { first: "'" }],
  dqMulti: [/"(?:[^"\\]|\\[\s\S])*"?/, 'string', { first: '"' }],
  sqMulti: [/'(?:[^'\\]|\\[\s\S])*'?/, 'string', { first: "'" }],
  backtick: [/`(?:[^`\\]|\\[\s\S])*`?/, 'string', { first: '`' }],
  tripleDq: [/"""[\s\S]*?(?:"""|$)/, 'string', { first: '"' }],
  annotation: [/@[\p{L}_][\p{L}\p{N}_.]*/u, 'meta', { first: '@' }],
};

const JS_REGEX_AFTER_KW = words('return typeof case do else in of new delete void throw yield await instanceof');
function jsRegexAllowed(prev) {
  if (!prev) return true;
  if (prev.type === 'operator') return true;
  if (prev.type === 'punctuation') return !/[)\]}]$/.test(prev.text);
  if (prev.type === 'keyword') return JS_REGEX_AFTER_KW.has(prev.text);
  return false;
}

const JS_KEYWORDS = 'break case catch class const continue debugger default delete do else export extends finally for from function if import in instanceof let new of return static super switch this throw try typeof var void while with yield async await get set as';
const TS_KEYWORDS = ' interface type enum implements namespace declare abstract private protected public readonly keyof infer is unique satisfies override module asserts';

const javascript = makeLexer({
  keywords: JS_KEYWORDS + TS_KEYWORDS,
  literals: 'true false null undefined NaN Infinity',
  types: 'string number boolean object symbol bigint any unknown never',
  funcKeywords: 'function',
  typeKeywords: 'class interface type enum extends implements new',
  capitalTypes: true,
  rules: [
    R.slashComment, R.blockComment,
    [/\/(?![*/])(?:[^/\\\n[]|\\.|\[(?:[^\]\\\n]|\\.){0,200}\]){1,1000}\/[a-z]*/, 'string', { first: '/', when: jsRegexAllowed }],
    R.dq, R.sq, R.backtick, R.annotation,
    [/#!\/[^\n]*/, 'meta', { first: '#', when: (p) => !p }],
  ],
});

const json = makeLexer({
  literals: 'true false null',
  rules: [
    R.slashComment, R.blockComment,
    [/"(?:[^"\\\n]|\\.)*"?/, (m, prev, src, pos) => (nextNonSpace(src, pos + m[0].length) === ':' ? 'attr' : 'string'), { first: '"' }],
  ],
});

const python = makeLexer({
  keywords: 'and as assert async await break class continue def del elif else except finally for from global if import in is lambda nonlocal not or pass raise return try while with yield match case self cls',
  literals: 'True False None NotImplemented Ellipsis',
  types: 'int float str bytes bool list dict set tuple frozenset object complex bytearray type',
  funcKeywords: 'def',
  typeKeywords: 'class',
  rules: [
    R.hashComment,
    [/(?:[rRbBuUfF]{1,2})?(?:"""[\s\S]*?(?:"""|$)|'''[\s\S]*?(?:'''|$)|"(?:[^"\\\n]|\\[\s\S])*"?|'(?:[^'\\\n]|\\[\s\S])*'?)/, 'string', { first: 'rRbBuUfF"\'' }],
    R.annotation,
  ],
});

const go = makeLexer({
  keywords: 'break case chan const continue default defer else fallthrough for func go goto if import interface map package range return select struct switch type var',
  literals: 'true false nil iota',
  types: 'bool byte complex64 complex128 error float32 float64 int int8 int16 int32 int64 rune string uint uint8 uint16 uint32 uint64 uintptr any comparable',
  funcKeywords: 'func',
  typeKeywords: 'type struct interface',
  rules: [R.slashComment, R.blockComment, R.dq, [/`[^`]*`?/, 'string', { first: '`' }], R.sq],
});

const rust = makeLexer({
  keywords: 'as async await break const continue crate dyn else enum extern fn for if impl in let loop match mod move mut pub ref return self Self static struct super trait type unsafe use where while yield',
  literals: 'true false',
  types: 'i8 i16 i32 i64 i128 isize u8 u16 u32 u64 u128 usize f32 f64 bool char str String Vec Option Result Box Rc Arc',
  funcKeywords: 'fn',
  typeKeywords: 'struct enum trait type impl',
  capitalTypes: true,
  classifyIdent: (w, prev, src, end) => (src[end] === '!' && src[end + 1] !== '=' ? 'function' : undefined),
  rules: [
    R.slashComment, R.blockComment,
    // Unterminated raw strings run to end of input so a failed scan is never repeated.
    [/b?r(#{0,8})"[\s\S]*?(?:"\1|$)/, 'string', { first: 'br' }],
    [/b?"(?:[^"\\]|\\[\s\S])*"?/, 'string', { first: 'b"' }],
    [/b?'(?:[^'\\\n]|\\(?:[^\n]|u\{[0-9a-fA-F]{1,6}\}))'/, 'string', { first: "b'" }],
    [/'[\p{L}_][\p{L}\p{N}_]*/u, 'variable', { first: "'" }],
    [/#!?\[[^\]\n]*\]?/, 'meta', { first: '#' }],
  ],
});

const JAVA_KW = 'abstract assert break case catch class continue default do else enum extends final finally for if implements import instanceof interface native new package private protected public return static strictfp super switch synchronized this throw throws transient try volatile while var record sealed permits yield';
const java = makeLexer({
  keywords: JAVA_KW,
  literals: 'true false null',
  types: 'boolean byte char double float int long short void',
  typeKeywords: 'class interface enum extends implements new record',
  capitalTypes: true,
  rules: [R.slashComment, R.blockComment, R.tripleDq, R.dq, R.sq, R.annotation],
});

const kotlin = makeLexer({
  keywords: 'as break class continue do else for fun if in interface is object package return super this throw try typealias typeof val var when while by catch constructor finally get import init set where actual abstract annotation companion const crossinline data enum expect external final infix inline inner internal lateinit noinline open operator out override private protected public reified sealed suspend tailrec vararg',
  literals: 'true false null',
  types: 'Int Long Short Byte Double Float Boolean Char String Unit Any Nothing',
  funcKeywords: 'fun',
  typeKeywords: 'class interface object typealias',
  capitalTypes: true,
  rules: [R.slashComment, R.blockComment, R.tripleDq, R.dq, R.sq, R.annotation],
});

const C_KW = 'auto break case const continue default do else enum extern for goto if inline register restrict return sizeof static struct switch typedef union volatile while _Alignas _Alignof _Atomic _Generic _Noreturn _Static_assert _Thread_local';
const C_TYPES = 'char double float int long short signed unsigned void bool _Bool _Complex size_t ssize_t ptrdiff_t intptr_t uintptr_t int8_t int16_t int32_t int64_t uint8_t uint16_t uint32_t uint64_t wchar_t FILE';
const cPreproc = [/#[ \t]*[a-z]+(?:[^\n\\]|\\[\s\S])*/, 'meta', { first: '#' }];
const cStr = [/(?:L|u8|u|U)?"(?:[^"\\\n]|\\[\s\S])*"?/, 'string', { first: 'Lu8U"' }];
const cChar = [/(?:L|u8|u|U)?'(?:[^'\\\n]|\\[\s\S])*'?/, 'string', { first: "Lu8U'" }];

const c = makeLexer({
  keywords: C_KW,
  literals: 'true false NULL',
  types: C_TYPES,
  typeKeywords: 'struct union enum',
  rules: [R.slashComment, R.blockComment, cPreproc, cStr, cChar],
});

const cpp = makeLexer({
  keywords: C_KW + ' alignas alignof and and_eq asm catch class co_await co_return co_yield concept consteval constexpr constinit const_cast decltype delete dynamic_cast explicit export final friend mutable namespace new noexcept not not_eq operator or or_eq override private protected public reinterpret_cast requires static_assert static_cast template this thread_local throw try typeid typename using virtual xor xor_eq',
  literals: 'true false nullptr NULL',
  types: C_TYPES + ' char8_t char16_t char32_t string vector map set unordered_map unique_ptr shared_ptr std',
  typeKeywords: 'struct union enum class typename namespace',
  capitalTypes: true,
  rules: [
    R.slashComment, R.blockComment, cPreproc,
    [/(?:L|u8|u|U)?R"([^()\\\s"]{0,16})\([\s\S]*?(?:\)\1"|$)/, 'string', { first: 'LuUR' }],
    cStr, cChar,
  ],
});

const csharp = makeLexer({
  keywords: 'abstract as base break case catch checked class const continue default delegate do else enum event explicit extern finally fixed for foreach goto if implicit in interface internal is lock namespace new operator out override params private protected public readonly ref return sealed sizeof stackalloc static struct switch this throw try typeof unchecked unsafe using virtual volatile while add alias ascending async await by descending dynamic equals from get global group into join let nameof on orderby partial remove select set value var when where yield record init required with not and or',
  literals: 'true false null',
  types: 'bool byte char decimal double float int long object sbyte short string uint ulong ushort void nint nuint',
  typeKeywords: 'class interface struct enum record new',
  capitalTypes: true,
  rules: [
    R.slashComment, R.blockComment,
    [/#[ \t]*(?:if|else|elif|endif|define|undef|warning|error|line|region|endregion|pragma|nullable)\b[^\n]*/, 'meta', { first: '#' }],
    [/"""[\s\S]*?(?:"""|$)/, 'string', { first: '"' }],
    [/\$?@"(?:[^"]|"")*"?|@\$"(?:[^"]|"")*"?/, 'string', { first: '$@' }],
    [/\$?"(?:[^"\\\n]|\\[\s\S])*"?/, 'string', { first: '$"' }],
    R.sq,
  ],
});

const php = makeLexer({
  caseInsensitive: true,
  keywords: 'abstract and array as break callable case catch class clone const continue declare default do echo else elseif empty enddeclare endfor endforeach endif endswitch endwhile enum extends final finally fn for foreach function global goto if implements include include_once instanceof insteadof interface isset list match namespace new or print private protected public readonly require require_once return static switch throw trait try unset use var while xor yield',
  literals: 'true false null',
  types: 'int float string bool array object mixed void never iterable self parent',
  funcKeywords: 'function fn',
  typeKeywords: 'class interface trait enum extends implements new',
  capitalTypes: true,
  rules: [
    [/<\?(?:php|=)?|\?>/, 'meta', { first: '<?' }],
    R.slashComment, R.blockComment, R.hashComment,
    [/\$[\p{L}_][\p{L}\p{N}_]*/u, 'variable', { first: '$' }],
    R.dqMulti, R.sqMulti,
  ],
});

const ruby = makeLexer({
  keywords: 'BEGIN END alias and begin break case class def defined? do else elsif end ensure for if in module next not or redo rescue retry return self super then undef unless until when while yield __FILE__ __LINE__ __method__ attr_accessor attr_reader attr_writer require require_relative include extend raise lambda proc private protected public puts',
  literals: 'true false nil',
  funcKeywords: 'def',
  typeKeywords: 'class module',
  capitalTypes: true,
  ident: /[\p{L}_][\p{L}\p{N}_]*[?!]?/u,
  rules: [
    [/^=begin[\s\S]*?(?:^=end|$(?![\s\S]))/m, 'comment', { first: '=' }],
    R.hashComment,
    [/@@?[\p{L}_][\p{L}\p{N}_]*|\$[\p{L}_][\p{L}\p{N}_]*/u, 'variable', { first: '@$' }],
    [/:[\p{L}_][\p{L}\p{N}_]*[?!=]?/u, 'variable', { first: ':', when: (p) => !p || p.type !== 'ident' }],
    R.dqMulti, R.sqMulti, R.backtick,
  ],
});

const swift = makeLexer({
  keywords: 'associatedtype class deinit enum extension fileprivate func import init inout internal let open operator private precedencegroup protocol public rethrows static struct subscript typealias var break case catch continue default defer do else fallthrough for guard if in repeat return throw switch where while as await async is self Self super throws try some any actor nonisolated isolated mutating nonmutating override final lazy weak unowned required convenience dynamic optional get set willSet didSet',
  literals: 'true false nil',
  types: 'Int Double Float String Bool Character Array Dictionary Set Optional Any AnyObject Void',
  funcKeywords: 'func',
  typeKeywords: 'class struct enum protocol extension typealias actor',
  capitalTypes: true,
  rules: [
    R.slashComment, R.blockComment, R.tripleDq, R.dq, R.annotation,
    [/#(?:if|elseif|else|endif|available|selector|keyPath|warning|error|file|line|function)\b/, 'meta', { first: '#' }],
  ],
});

const sql = makeLexer({
  caseInsensitive: true,
  keywords: 'select from where and or not insert into values update set delete create table drop alter add column index primary key foreign references unique null is in like ilike between join inner left right full outer cross natural on as group by order having limit offset distinct union all except intersect exists case when then else end begin commit rollback transaction view trigger procedure function returns return declare if asc desc default constraint check with recursive replace pragma explain analyze grant revoke database schema cascade restrict temporary temp returning conflict do nothing using window over partition rows range unbounded preceding following current row fetch first next only top vacuum attach detach',
  literals: 'true false',
  types: 'int integer bigint smallint tinyint decimal numeric float real double precision char varchar nvarchar text blob date time timestamp timestamptz datetime interval boolean bool serial bigserial uuid json jsonb bytea',
  rules: [
    [/--[^\n]*/, 'comment', { first: '-' }],
    R.blockComment,
    [/'(?:[^']|'')*'?/, 'string', { first: "'" }],
    // An unclosed [identifier runs to the end of the line in one match (like "…" and `…`
    // above); requiring the "]" made every "[" rescan the rest of the line (quadratic).
    [/"(?:[^"]|"")*"?|`[^`]*`?|\[[^\]\n]*\]?/, 'variable', { first: '"`[' }],
    [/[?]\d*|[:@$][\p{L}_\d][\p{L}\p{N}_]*/u, 'variable', { first: '?:@$' }],
  ],
});

// Shell: command words are coloured as functions when they start a command.
const SHELL_KW = words('if then else elif fi case esac for while until do done in function select time coproc return exit break continue local export readonly declare typeset unset shift source alias trap eval exec set');
function shellLexer(src, out, startsCommand = true) {
  let pos = 0, cmdStart = startsCommand !== false;
  const n = src.length;
  const reVar = /\$(?:\{[^}\n]*\}?|[\p{L}_][\p{L}\p{N}_]*|[0-9@#?$!*-])/uy;
  const reWord = /[^\s'"`$;&|<>()#\\]+/y;
  const reOps = /&&|\|\||;;|[;&|<>()]+/y;
  while (pos < n) {
    const ch = src[pos];
    let m;
    if (ch === '\n') { out.push(null, ch); pos++; cmdStart = true; continue; }
    if (ch === ' ' || ch === '\t' || ch === '\r') { reWs.lastIndex = pos; reWs.exec(src); const s = src.slice(pos, reWs.lastIndex); const nl = s.includes('\n'); out.push(null, s); pos = reWs.lastIndex; if (nl) cmdStart = true; continue; }
    if (ch === '#' && (pos === 0 || /\s/.test(src[pos - 1]))) {
      const e = src.indexOf('\n', pos); const end = e < 0 ? n : e;
      out.push(pos === 0 && src[1] === '!' ? 'meta' : 'comment', src.slice(pos, end)); pos = end; continue;
    }
    if (ch === '\\') { out.push(null, src.slice(pos, pos + 2)); pos += 2; continue; }
    if (ch === "'") { const e = src.indexOf("'", pos + 1); const end = e < 0 ? n : e + 1; out.push('string', src.slice(pos, end)); pos = end; cmdStart = false; continue; }
    if (ch === '"' || ch === '`') {
      let i = pos + 1;
      while (i < n && src[i] !== ch) i += src[i] === '\\' ? 2 : 1;
      const end = Math.min(n, i + 1);
      out.push('string', src.slice(pos, end)); pos = end; cmdStart = false; continue;
    }
    if (ch === '$') {
      if (src[pos + 1] === '(') { out.push('punctuation', '$('); pos += 2; cmdStart = true; continue; }
      if (src[pos + 1] === "'") { let i = pos + 2; while (i < n && src[i] !== "'") i += src[i] === '\\' ? 2 : 1; const end = Math.min(n, i + 1); out.push('string', src.slice(pos, end)); pos = end; continue; }
      reVar.lastIndex = pos; m = reVar.exec(src);
      if (m) { out.push('variable', m[0]); pos += m[0].length; cmdStart = false; continue; }
    }
    reOps.lastIndex = pos; m = reOps.exec(src);
    if (m) { out.push('operator', m[0]); pos += m[0].length; if (/[;&|(]/.test(m[0])) cmdStart = true; continue; }
    reWord.lastIndex = pos; m = reWord.exec(src);
    if (m) {
      const w = m[0];
      let type = null;
      if (SHELL_KW.has(w)) { type = 'keyword'; cmdStart = !/^(?:in|function)$/.test(w); }
      else if (cmdStart) {
        const eq = w.indexOf('=');
        if (eq > 0 && /^[A-Za-z_][\w]*$/.test(w.slice(0, eq))) { out.push('variable', w.slice(0, eq)); out.push('operator', '='); out.push(null, w.slice(eq + 1)); pos += w.length; continue; }
        type = 'function'; cmdStart = false;
      } else if (/^--?[A-Za-z]/.test(w)) type = 'attr';
      else if (/^\d+(?:\.\d+)?$/.test(w)) type = 'number';
      out.push(type, w); pos += w.length; continue;
    }
    out.push(null, ch); pos++;
  }
}

const PS_KW = words('begin break catch class continue data define do dynamicparam else elseif end enum exit filter finally for foreach from function hidden if in inlinescript param process return static switch throw trap try until using var while workflow');
const powershell = makeLexer({
  caseInsensitive: true,
  keywords: [...PS_KW].join(' '),
  funcKeywords: 'function filter',
  typeKeywords: 'class enum',
  ident: /[\p{L}_][\p{L}\p{N}_]*(?:-[\p{L}][\p{L}\p{N}]*)?/u,
  classifyIdent: (w) => (/^[A-Za-z]+-[A-Za-z][A-Za-z0-9]*$/.test(w) ? 'function' : undefined),
  operator: /[+\-*/%=!<>|&,]+/,
  rules: [
    [/<#[\s\S]*?(?:#>|$)/, 'comment', { first: '<' }],
    R.hashComment,
    [/@"[\s\S]*?(?:\n"@|$)|@'[\s\S]*?(?:\n'@|$)/, 'string', { first: '@' }],
    [/"(?:[^"`]|`[\s\S]|"")*"?/, 'string', { first: '"' }],
    [/'(?:[^']|'')*'?/, 'string', { first: "'" }],
    [/\$(?:true|false|null)\b/i, 'keyword', { first: '$' }],
    [/\$\{[^}\n]*\}?|\$(?:[\p{L}_][\p{L}\p{N}_]*:)?[\p{L}_][\p{L}\p{N}_]*|\$[$?^_]/u, 'variable', { first: '$' }],
    [/-(?:eq|ne|gt|ge|lt|le|like|notlike|match|notmatch|contains|notcontains|in|notin|replace|split|join|and|or|not|xor|band|bor|is|isnot|as|f)\b/i, 'operator', { first: '-' }],
    [/-[\p{L}][\p{L}\p{N}]*/u, 'attr', { first: '-', when: (p) => !p || p.type !== 'ident' && p.type !== 'number' && p.type !== 'variable' }],
    [/\[[\p{L}_][\p{L}\p{N}_.]*(?:\[\])?\]/u, 'type', { first: '[' }],
  ],
});

// ---------------------------------------------------------------------------------------------
// Markup, stylesheet and config-file lexers (hand written, state-machine style)

const reMarkupNext = /<!--|<!\[CDATA\[|<!|<\?|<\/?[A-Za-z]|&(?:#\d{1,7}|#[xX][\da-fA-F]{1,6}|[A-Za-z][A-Za-z0-9]{1,31});/g;
function markupLexer(src, out) {
  const n = src.length;
  let pos = 0;
  while (pos < n) {
    reMarkupNext.lastIndex = pos;
    const m = reMarkupNext.exec(src);
    if (!m) { out.push(null, src.slice(pos)); break; }
    if (m.index > pos) out.push(null, src.slice(pos, m.index));
    pos = m.index;
    const tok = m[0];
    if (tok === '<!--') { const e = src.indexOf('-->', pos + 4); const end = e < 0 ? n : e + 3; out.push('comment', src.slice(pos, end)); pos = end; continue; }
    if (tok === '<![CDATA[') { const e = src.indexOf(']]>', pos); const end = e < 0 ? n : e + 3; out.push('string', src.slice(pos, end)); pos = end; continue; }
    if (tok === '<!' || tok === '<?') { const close = tok === '<?' ? '?>' : '>'; const e = src.indexOf(close, pos + 2); const end = e < 0 ? n : e + close.length; out.push('meta', src.slice(pos, end)); pos = end; continue; }
    if (tok[0] === '&') { out.push('meta', tok); pos += tok.length; continue; }
    pos = lexTag(src, pos, out);
  }
}

const reTagName = /[A-Za-z][\w:.-]*/y;
const reAttrName = /[^\s"'>/=<]+/y;
const reAttrValue = /"[^"]*"?|'[^']*'?|[^\s>"'<]+/y;
function lexTag(src, pos, out) {
  const n = src.length;
  const closing = src[pos + 1] === '/';
  out.push('punctuation', closing ? '</' : '<');
  pos += closing ? 2 : 1;
  reTagName.lastIndex = pos;
  const nm = reTagName.exec(src);
  const name = nm ? nm[0].toLowerCase() : '';
  if (nm) { out.push('tag', nm[0]); pos += nm[0].length; }
  let selfClosing = false;
  while (pos < n) {
    const ch = src[pos];
    if (ch === '>') { out.push('punctuation', '>'); pos++; break; }
    if (ch === '/' && src[pos + 1] === '>') { out.push('punctuation', '/>'); pos += 2; selfClosing = true; break; }
    if (ch === '<') return pos; // malformed: let the outer loop resume
    if (/\s/.test(ch)) { reWs.lastIndex = pos; reWs.exec(src); out.push(null, src.slice(pos, reWs.lastIndex)); pos = reWs.lastIndex; continue; }
    if (ch === '=') {
      out.push('operator', '='); pos++;
      while (pos < n && (src[pos] === ' ' || src[pos] === '\t')) { out.push(null, src[pos]); pos++; }
      reAttrValue.lastIndex = pos;
      const v = reAttrValue.exec(src);
      if (v) { out.push('string', v[0]); pos += v[0].length; }
      continue;
    }
    reAttrName.lastIndex = pos;
    const a = reAttrName.exec(src);
    if (a) { out.push('attr', a[0]); pos += a[0].length; continue; }
    out.push(null, ch); pos++;
  }
  // Embedded script/style bodies are highlighted with their own lexers.
  if (!closing && !selfClosing && (name === 'script' || name === 'style')) {
    const re = name === 'script' ? /<\/script/gi : /<\/style/gi;
    re.lastIndex = pos;
    const e = re.exec(src);
    const end = e ? e.index : n;
    if (end > pos) (name === 'script' ? javascript : cssLexer)(src.slice(pos, end), out);
    pos = end;
  }
  return pos;
}

function cssLexer(src, out) {
  const n = src.length;
  let pos = 0, depth = 0, ctx = 'selector';
  // Decide whether a statement inside a block is a nested selector (SCSS/LESS) or a declaration.
  const decide = (i) => {
    if (depth === 0) return 'selector';
    const end = Math.min(n, i + 400);
    for (; i < end; i++) {
      const c = src[i];
      if (c === '{') return 'selector';
      if (c === ';' || c === '}') return 'property';
    }
    return 'property';
  };
  const reAt = /@[\w-]+/y, reHex = /#[0-9a-fA-F]{3,8}(?![\w-])/y, reNum = /-?(?:\d+\.?\d*|\.\d+)(?:%|[a-zA-Z]+)?/y;
  const reWord = /-?-?[\p{L}_][\p{L}\p{N}_-]*/uy, reSel = /[.#][\p{L}_-][\p{L}\p{N}_-]*/uy, rePseudo = /::?[\p{L}-][\p{L}\p{N}_-]*/uy;
  const reVar = /[$@]-?[\p{L}_][\p{L}\p{N}_-]*/uy;
  while (pos < n) {
    const ch = src[pos];
    let m;
    if (/\s/.test(ch)) { reWs.lastIndex = pos; reWs.exec(src); out.push(null, src.slice(pos, reWs.lastIndex)); pos = reWs.lastIndex; continue; }
    if (ch === '/' && src[pos + 1] === '*') { const e = src.indexOf('*/', pos + 2); const end = e < 0 ? n : e + 2; out.push('comment', src.slice(pos, end)); pos = end; continue; }
    if (ch === '/' && src[pos + 1] === '/' && ctx !== 'value') { const e = src.indexOf('\n', pos); const end = e < 0 ? n : e; out.push('comment', src.slice(pos, end)); pos = end; continue; }
    if (ch === '"' || ch === "'") { let i = pos + 1; while (i < n && src[i] !== ch && src[i] !== '\n') i += src[i] === '\\' ? 2 : 1; const end = Math.min(n, i + 1); out.push('string', src.slice(pos, end)); pos = end; continue; }
    if (ch === '{') { out.push('punctuation', ch); pos++; depth++; ctx = decide(pos); continue; }
    if (ch === '}') { out.push('punctuation', ch); pos++; depth = Math.max(0, depth - 1); ctx = decide(pos); continue; }
    if (ch === ';') { out.push('punctuation', ch); pos++; ctx = decide(pos); continue; }
    if (ch === '@') { reAt.lastIndex = pos; m = reAt.exec(src); if (m && ctx !== 'value') { out.push('keyword', m[0]); pos += m[0].length; ctx = 'value'; continue; } }
    if (ch === '@' || ch === '$') { reVar.lastIndex = pos; m = reVar.exec(src); if (m) { out.push('variable', m[0]); pos += m[0].length; continue; } }
    if (ch === ':' && ctx === 'property') { out.push('punctuation', ':'); pos++; ctx = 'value'; continue; }
    if (ctx === 'value') {
      if (ch === '!') { m = /![ \t]*important/iy; m.lastIndex = pos; const r = m.exec(src); if (r) { out.push('keyword', r[0]); pos += r[0].length; continue; } }
      if (ch === '#') { reHex.lastIndex = pos; m = reHex.exec(src); if (m) { out.push('number', m[0]); pos += m[0].length; continue; } }
      reNum.lastIndex = pos; m = (isDigit(ch) || ch === '.' || ch === '-') ? reNum.exec(src) : null;
      if (m && /\d/.test(m[0])) { out.push('number', m[0]); pos += m[0].length; continue; }
      reWord.lastIndex = pos; m = reWord.exec(src);
      if (m) {
        const fn = src[pos + m[0].length] === '(';
        out.push(fn ? 'function' : m[0].startsWith('--') ? 'variable' : null, m[0]); pos += m[0].length; continue;
      }
    } else if (ctx === 'property') {
      reWord.lastIndex = pos; m = reWord.exec(src);
      if (m) { out.push(m[0].startsWith('--') ? 'variable' : 'attr', m[0]); pos += m[0].length; continue; }
    } else {
      reSel.lastIndex = pos; m = (ch === '.' || ch === '#') ? reSel.exec(src) : null;
      if (m) { out.push('type', m[0]); pos += m[0].length; continue; }
      rePseudo.lastIndex = pos; m = ch === ':' ? rePseudo.exec(src) : null;
      if (m) { out.push('meta', m[0]); pos += m[0].length; continue; }
      reWord.lastIndex = pos; m = reWord.exec(src);
      if (m) { out.push('tag', m[0]); pos += m[0].length; continue; }
    }
    out.push(/[(),>+~*=[\]]/.test(ch) ? 'punctuation' : null, ch); pos++;
  }
}

/** Splits src into lines keeping the terminators, and feeds each to fn(line, out). */
function byLine(src, out, fn) {
  let pos = 0;
  const n = src.length;
  while (pos < n) {
    const e = src.indexOf('\n', pos);
    const end = e < 0 ? n : e;
    fn(src.slice(pos, end), out);
    if (e >= 0) out.push(null, '\n');
    pos = end + 1;
  }
}

const reYamlNum = /^[-+]?(?:\d[\d_]*(?:\.\d*)?(?:[eE][-+]?\d+)?|0x[0-9a-fA-F]+|0o[0-7]+|\.inf|\.nan)$/i;
const YAML_LIT = words('true false yes no on off null ~ True False Yes No On Off Null TRUE FALSE NULL YES NO');
function yamlScalar(s, out) {
  const t = s.trim();
  if (!t) { out.push(null, s); return; }
  const lead = s.slice(0, s.length - s.trimStart().length), trail = s.slice(lead.length + t.length);
  out.push(null, lead);
  let type = 'string';
  if (YAML_LIT.has(t)) type = 'keyword';
  else if (reYamlNum.test(t)) type = 'number';
  else if (/^[&*][^\s]+$/.test(t)) type = 'variable';
  else if (/^!{1,2}[^\s]*$/.test(t)) type = 'meta';
  else if (/^[|>][-+0-9]*$/.test(t)) type = 'operator';
  else if (/^[[\]{},]$/.test(t)) type = 'punctuation';
  out.push(type, t);
  out.push(null, trail);
}
/** Finds a "#" comment start that is at line start or preceded by whitespace, outside quotes. */
function findHashComment(line, from) {
  let q = '';
  for (let i = from; i < line.length; i++) {
    const c = line[i];
    if (q) { if (c === q) q = ''; continue; }
    if (c === '"' || c === "'") { if (i === from || /[\s:,[{-]/.test(line[i - 1])) q = c; continue; }
    if (c === '#' && (i === 0 || line[i - 1] === ' ' || line[i - 1] === '\t')) return i;
  }
  return -1;
}
function yamlLexer(src, out) {
  byLine(src, out, (line, o) => {
    if (/^(?:---|\.\.\.)(?:\s|$)/.test(line)) { o.push('meta', line.slice(0, 3)); line = line.slice(3); if (!line) return; }
    if (/^\s*#/.test(line)) { o.push('comment', line); return; }
    if (/^%/.test(line)) { o.push('meta', line); return; }
    const ci = findHashComment(line, 0);
    const comment = ci >= 0 ? line.slice(ci) : '';
    let body = ci >= 0 ? line.slice(0, ci) : line;
    // indentation and sequence dashes
    let i = 0;
    while (i < body.length) {
      if (body[i] === ' ' || body[i] === '\t') { i++; continue; }
      if (body[i] === '-' && (i + 1 === body.length || body[i + 1] === ' ')) { o.push(null, body.slice(0, i)); o.push('punctuation', '-'); body = body.slice(i + 1); i = 0; continue; }
      break;
    }
    o.push(null, body.slice(0, i));
    body = body.slice(i);
    // key: value
    let k = -1;
    if (body[0] === '"' || body[0] === "'") {
      const e = body.indexOf(body[0], 1);
      const after = e > 0 ? /^[ \t]*:(?=[ \t]|$)/.exec(body.slice(e + 1)) : null;
      if (after) k = e + after[0].length; // index of the ':'
    } else {
      for (let j = 0; j < body.length; j++) {
        if (body[j] === ':' && (j + 1 === body.length || body[j + 1] === ' ' || body[j + 1] === '\t')) { k = j; break; }
      }
    }
    if (k > 0 && !/^[[{]/.test(body)) {
      o.push('attr', body.slice(0, k).trimEnd());
      o.push(null, body.slice(body.slice(0, k).trimEnd().length, k));
      o.push('punctuation', ':');
      body = body.slice(k + 1);
    }
    if (/^\s*["']/.test(body)) { const lead = body.length - body.trimStart().length; o.push(null, body.slice(0, lead)); o.push('string', body.slice(lead)); }
    else yamlScalar(body, o);
    if (comment) o.push('comment', comment);
  });
}

const tomlValue = makeLexer({
  literals: 'true false inf nan',
  rules: [
    R.hashComment,
    [/"""[\s\S]*?(?:"""|$)|'''[\s\S]*?(?:'''|$)/, 'string', { first: '"\'' }],
    R.dq, [/'[^'\n]*'?/, 'string', { first: "'" }],
    [/\d{4}-\d{2}-\d{2}(?:[T ]\d{2}:\d{2}(?::\d{2}(?:\.\d+)?)?(?:Z|[+-]\d{2}:\d{2})?)?/, 'number', { first: '0123456789' }],
  ],
});
function tomlLexer(src, out) {
  const lines = src.split('\n');
  let inMulti = '';
  lines.forEach((line, idx) => {
    if (idx > 0) out.push(null, '\n');
    if (inMulti) {
      const e = line.indexOf(inMulti);
      if (e < 0) { out.push('string', line); return; }
      out.push('string', line.slice(0, e + 3)); line = line.slice(e + 3); inMulti = '';
      tomlValue(line, out); return;
    }
    const t = line.trimStart();
    if (/^\[\[?[^\]]*\]\]?/.test(t)) {
      const lead = line.length - t.length;
      const m = /^\[\[?[^\]\n]*\]?\]?/.exec(t)[0];
      out.push(null, line.slice(0, lead)); out.push('type', m); tomlValue(t.slice(m.length), out); return;
    }
    const eq = line.indexOf('=');
    if (eq > 0 && !/^\s*#/.test(line) && !/["']/.test(line.slice(0, eq).replace(/^\s*"[^"]*"|^\s*'[^']*'/, ''))) {
      const key = line.slice(0, eq);
      const lead = key.length - key.trimStart().length;
      out.push(null, key.slice(0, lead)); out.push('attr', key.trim()); out.push(null, key.slice(lead + key.trim().length));
      out.push('operator', '=');
      const rest = line.slice(eq + 1);
      const tq = /("""|''')/.exec(rest);
      if (tq && rest.indexOf(tq[1], tq.index + 3) < 0) { inMulti = tq[1]; tomlValue(rest.slice(0, tq.index), out); out.push('string', rest.slice(tq.index)); return; }
      tomlValue(rest, out); return;
    }
    tomlValue(line, out);
  });
}

function iniLexer(src, out) {
  byLine(src, out, (line, o) => {
    const t = line.trimStart();
    const lead = line.slice(0, line.length - t.length);
    if (!t) { o.push(null, line); return; }
    if (t[0] === ';' || t[0] === '#') { o.push(null, lead); o.push('comment', t); return; }
    if (t[0] === '[') { o.push(null, lead); o.push('type', t); return; }
    const m = /^([^=:]+?)([ \t]*)([=:])(.*)$/.exec(t);
    if (!m) { o.push(null, line); return; }
    o.push(null, lead); o.push('attr', m[1]); o.push(null, m[2]); o.push('operator', m[3]);
    const v = m[4];
    const vt = v.trim();
    const vLead = v.slice(0, v.length - v.trimStart().length);
    o.push(null, vLead);
    if (/^(?:true|false|yes|no|on|off)$/i.test(vt)) o.push('keyword', vt);
    else if (/^-?\d+(?:\.\d+)?$/.test(vt)) o.push('number', vt);
    else o.push('string', vt);
    o.push(null, v.slice(vLead.length + vt.length));
  });
}

function diffLexer(src, out) {
  byLine(src, out, (line, o) => {
    if (/^(?:\+\+\+|---)(?: |$)/.test(line) || line.startsWith('@@')) o.push('meta', line);
    else if (line[0] === '+' || line[0] === '>') o.push('inserted', line);
    else if (line[0] === '-' || line[0] === '<') o.push('deleted', line);
    else if (/^(?:diff |index |new file|deleted file|similarity|rename |Binary files|\\ No newline)/.test(line)) o.push('comment', line);
    else o.push(null, line);
  });
}

/** JS RegExp \s (WhiteSpace + LineTerminator) by UTF-16 code unit. */
function isSpaceCode(c) {
  return c === 32 || (c >= 9 && c <= 13) || c === 0xa0 || c === 0x1680 || (c >= 0x2000 && c <= 0x200a)
    || c === 0x2028 || c === 0x2029 || c === 0x202f || c === 0x205f || c === 0x3000 || c === 0xfeff;
}

/**
 * seek(from) → first index ≥ from whose code unit satisfies test, or s.length. The last answer
 * is cached, so a run of non-decreasing queries scans every character at most once.
 */
function seeker(s, test) {
  let from = -1, at = -1;
  return (f) => {
    if (f >= from && f <= at) return at; // nothing matched in [from, at), hence none in [f, at)
    let k = f;
    while (k < s.length && !test(s.charCodeAt(k))) k++;
    from = f;
    at = k;
    return k;
  };
}

/**
 * Inline Markdown spans of one line, scanned left to right in linear time. It matches what the
 * single regex
 *   /(`+)[^`]*?\1|\*\*[^*\n]+\*\*|__[^_\n]+__|\*[^*\s][^*\n]*\*|_[^_\s][^_\n]*_|
 *    !?\[[^\]\n]*\]\([^)\s]*\)|<https?:[^>\s]*>|https?:\/\/[^\s<>)]+/g
 * matched (leftmost match, first alternative wins), but that regex rescanned the rest of the
 * line from every "[" (or "[a](", "<http:") that never closed: quadratic on one long line, so a
 * crafted shared .md file froze the viewer's tab for minutes. Each look-ahead below ("next ]",
 * "next ) or space", …) is cached and only moves forward.
 */
function markdownInline(s, out) {
  const n = s.length;
  const nextTick = seeker(s, (c) => c === 96); // `
  const nextStar = seeker(s, (c) => c === 42 || c === 10); // * or \n
  const nextUnder = seeker(s, (c) => c === 95 || c === 10); // _ or \n
  const nextBracket = seeker(s, (c) => c === 93 || c === 10); // ] or \n
  const nextParenOrSpace = seeker(s, (c) => c === 41 || isSpaceCode(c)); // ) or \s
  const nextGtOrSpace = seeker(s, (c) => c === 62 || isSpaceCode(c)); // > or \s
  let pos = 0; // start of pending plain text
  const emit = (start, parts) => {
    if (start > pos) out.push(null, s.slice(pos, start));
    for (const [type, a, b] of parts) out.push(type, s.slice(a, b));
    pos = parts[parts.length - 1][2];
  };
  // Delimited emphasis: **x** / __x__ (strong) or *x* / _x_ (em) starting at i; end index or -1.
  const delimited = (i, ch, next) => {
    if (s[i + 1] === ch) {
      const q = next(i + 2);
      return q > i + 2 && q < n && s[q] === ch && s[q + 1] === ch ? q + 2 : -1;
    }
    if (i + 1 >= n || isSpaceCode(s.charCodeAt(i + 1))) return -1;
    const q = next(i + 2);
    return q < n && s[q] === ch ? q + 1 : -1;
  };
  let i = 0;
  while (i < n) {
    const c = s[i];
    let end = -1;
    if (c === '`') {
      let r = 1;
      while (s[i + r] === '`') r++;
      const q = nextTick(i + r);
      let k = 0;
      while (k < r && s[q + k] === '`') k++;
      if (q < n && k === r) end = q + r; // closing run at least as long as the opening one
      else if (r >= 2) end = i + 2 * (r >> 1); // the opening run closes itself
      if (end > 0) { emit(i, [['string', i, end]]); i = end; continue; }
    } else if (c === '*' || c === '_') {
      end = delimited(i, c, c === '*' ? nextStar : nextUnder);
      if (end > 0) { emit(i, [[s[i + 1] === c ? 'keyword' : 'variable', i, end]]); i = end; continue; }
    } else if (c === '[' || (c === '!' && s[i + 1] === '[')) {
      const b = c === '[' ? i : i + 1;
      const q = nextBracket(b + 1);
      if (q < n && s[q] === ']' && s[q + 1] === '(') {
        const r = nextParenOrSpace(q + 2);
        if (r < n && s[r] === ')') { emit(i, [['attr', i, q + 1], ['string', q + 1, r + 1]]); i = r + 1; continue; }
      }
    } else if (c === '<' && s.startsWith('http', i + 1)) {
      const j = s[i + 5] === 's' ? i + 6 : i + 5;
      if (s[j] === ':') {
        const r = nextGtOrSpace(j + 1);
        if (r < n && s[r] === '>') { emit(i, [['string', i, r + 1]]); i = r + 1; continue; }
      }
    } else if (c === 'h' && s.startsWith('http', i)) {
      const j = s[i + 4] === 's' ? i + 5 : i + 4;
      if (s.startsWith('://', j)) {
        let e = j + 3;
        for (; e < n; e++) {
          const cc = s.charCodeAt(e);
          if (cc === 60 || cc === 62 || cc === 41 || isSpaceCode(cc)) break; // < > ) \s
        }
        if (e > j + 3) { emit(i, [['string', i, e]]); i = e; continue; }
      }
    }
    i++;
  }
  if (pos < n) out.push(null, s.slice(pos));
}
function markdownLexer(src, out) {
  let fence = '';
  byLine(src, out, (line, o) => {
    const fm = /^ {0,3}(`{3,}|~{3,})/.exec(line);
    if (fence) {
      if (fm && fm[1][0] === fence[0] && fm[1].length >= fence.length && !line.slice(fm[0].length).trim()) { o.push('meta', line); fence = ''; }
      else o.push('string', line);
      return;
    }
    if (fm) { fence = fm[1]; o.push('meta', line); return; }
    let m;
    if ((m = /^ {0,3}#{1,6}(?:[ \t]|$)/.exec(line))) { o.push('keyword', line); return; }
    if (/^ {0,3}(?:(?:\*[ \t]*){3,}|(?:-[ \t]*){3,}|(?:_[ \t]*){3,})$/.test(line)) { o.push('meta', line); return; }
    if ((m = /^ {0,3}(?:>[ \t]?)+/.exec(line))) { o.push('comment', m[0]); line = line.slice(m[0].length); }
    if ((m = /^[ \t]*(?:[*+-]|\d{1,9}[.)])(?:[ \t]+\[[ xX]\])?(?=[ \t]|$)/.exec(line))) { o.push('operator', m[0]); line = line.slice(m[0].length); }
    markdownInline(line, o);
  });
}

const DOCKER_INSTR = /^([ \t]*)(FROM|RUN|CMD|LABEL|EXPOSE|ENV|ADD|COPY|ENTRYPOINT|VOLUME|USER|WORKDIR|ARG|ONBUILD|STOPSIGNAL|HEALTHCHECK|SHELL|MAINTAINER)\b/i;
function dockerfileLexer(src, out) {
  let cont = false;
  byLine(src, out, (line, o) => {
    if (!cont && /^\s*#/.test(line)) { o.push('comment', line); return; }
    const m = cont ? null : DOCKER_INSTR.exec(line);
    let rest = line, isCommand = cont;
    if (m) {
      o.push(null, m[1]); o.push('keyword', m[2]); rest = line.slice(m[0].length);
      const instr = m[2].toUpperCase();
      if (instr === 'FROM') {
        // FROM [--platform=x] image [AS name]
        for (const part of rest.split(/(\s+)/)) {
          if (!part) continue;
          if (/^\s+$/.test(part)) o.push(null, part);
          else if (/^as$/i.test(part)) o.push('keyword', part);
          else if (part.startsWith('--')) o.push('attr', part);
          else o.push('string', part);
        }
        cont = /\\\s*$/.test(line);
        return;
      }
      isCommand = /^(?:RUN|CMD|ENTRYPOINT|SHELL|HEALTHCHECK|ONBUILD)$/.test(instr);
    }
    shellLexer(rest, o, isCommand);
    cont = /\\\s*$/.test(line);
  });
}

// ---------------------------------------------------------------------------------------------
// Registry

const LANGS = {
  javascript: { label: 'JavaScript', lex: javascript, aliases: 'js jsx mjs cjs node' },
  typescript: { label: 'TypeScript', lex: javascript, aliases: 'ts tsx mts cts' },
  json: { label: 'JSON', lex: json, aliases: 'jsonc json5 geojson webmanifest' },
  python: { label: 'Python', lex: python, aliases: 'py py3 python3 gyp pyw' },
  go: { label: 'Go', lex: go, aliases: 'golang' },
  rust: { label: 'Rust', lex: rust, aliases: 'rs' },
  java: { label: 'Java', lex: java, aliases: 'jsp' },
  kotlin: { label: 'Kotlin', lex: kotlin, aliases: 'kt kts' },
  c: { label: 'C', lex: c, aliases: 'h' },
  cpp: { label: 'C++', lex: cpp, aliases: 'c++ cc cxx hpp hh hxx h++ ino' },
  csharp: { label: 'C#', lex: csharp, aliases: 'cs c# dotnet' },
  php: { label: 'PHP', lex: php, aliases: 'php3 php4 php5 php7 php8 phtml' },
  ruby: { label: 'Ruby', lex: ruby, aliases: 'rb gemspec podspec rake' },
  swift: { label: 'Swift', lex: swift, aliases: '' },
  sql: { label: 'SQL', lex: sql, aliases: 'mysql postgres postgresql pgsql psql sqlite plsql tsql mssql' },
  bash: { label: 'Shell', lex: shellLexer, aliases: 'sh shell zsh ksh console shellscript fish' },
  powershell: { label: 'PowerShell', lex: powershell, aliases: 'ps ps1 psm1 psd1 pwsh posh' },
  html: { label: 'HTML', lex: markupLexer, aliases: 'htm xhtml vue svelte' },
  xml: { label: 'XML', lex: markupLexer, aliases: 'svg xsd xsl xslt plist rss atom wsdl csproj' },
  css: { label: 'CSS', lex: cssLexer, aliases: 'scss sass less' },
  yaml: { label: 'YAML', lex: yamlLexer, aliases: 'yml' },
  toml: { label: 'TOML', lex: tomlLexer, aliases: '' },
  markdown: { label: 'Markdown', lex: markdownLexer, aliases: 'md mkd mkdn mdown gfm' },
  dockerfile: { label: 'Dockerfile', lex: dockerfileLexer, aliases: 'docker containerfile' },
  diff: { label: 'Diff', lex: diffLexer, aliases: 'patch udiff' },
  ini: { label: 'INI', lex: iniLexer, aliases: 'cfg conf properties editorconfig gitconfig desktop service env dotenv' },
};

const ALIASES = new Map();
for (const [id, def] of Object.entries(LANGS)) {
  ALIASES.set(id, id);
  for (const a of words(def.aliases)) ALIASES.set(a, id);
}

/**
 * Resolves a language name or alias (case-insensitive; info strings like "js {.x}" use the
 * first word) to a canonical id, or null when unsupported.
 * @param {string} lang
 * @returns {string|null}
 */
export function resolveLanguage(lang) {
  if (!lang || typeof lang !== 'string') return null;
  let key = lang.trim().split(/[\s{,]/, 1)[0].toLowerCase();
  if (key.startsWith('language-')) key = key.slice(9);
  return ALIASES.get(key) || null;
}

/**
 * Human-readable label for a language ("cpp" → "C++"). Unknown names are returned trimmed.
 * @param {string} lang
 */
export function languageLabel(lang) {
  const id = resolveLanguage(lang);
  if (id) return LANGS[id].label;
  return (lang || '').trim().slice(0, 32);
}

/** @returns {{id: string, label: string}[]} supported languages, sorted by label. */
export function listLanguages() {
  return Object.entries(LANGS)
    .map(([id, d]) => ({ id, label: d.label }))
    .sort((a, b) => a.label.localeCompare(b.label));
}

/**
 * Tokenizes source code.
 * @param {string} code
 * @param {string} lang language id or alias
 * @param {{maxChars?: number, budgetMs?: number}} [limits] override MAX_HIGHLIGHT_CHARS /
 *        HIGHLIGHT_BUDGET_MS (tests use this to exercise the lexers themselves)
 * @returns {{type: string|null, value: string}[]} tokens; type null means plain text.
 *          Concatenating all values reproduces the input exactly.
 */
export function tokenize(code, lang, { maxChars = MAX_HIGHLIGHT_CHARS, budgetMs = HIGHLIGHT_BUDGET_MS } = {}) {
  const src = String(code ?? '');
  const id = resolveLanguage(lang);
  if (!id || !src || src.length > maxChars) return src ? [{ type: null, value: src }] : [];
  const out = new TokenList(Number.isFinite(budgetMs) ? clock() + budgetMs : Infinity);
  try {
    LANGS[id].lex(src, out);
  } catch {
    // A lexer bug (or a blown time budget) must never break rendering of a note: fall back to
    // plain text.
    return [{ type: null, value: src }];
  }
  return out.tokens;
}

/**
 * Highlights code into a DocumentFragment of text nodes and <span class="hl-…"> elements.
 * @param {string} code
 * @param {string} lang
 * @param {Document} [doc]
 * @returns {DocumentFragment}
 */
export function highlight(code, lang, doc = globalThis.document) {
  const frag = doc.createDocumentFragment();
  for (const t of tokenize(code, lang)) {
    if (t.type) {
      const span = doc.createElement('span');
      span.setAttribute('class', 'hl-' + t.type);
      span.appendChild(doc.createTextNode(t.value));
      frag.appendChild(span);
    } else {
      frag.appendChild(doc.createTextNode(t.value));
    }
  }
  return frag;
}

const escapeHtml = (s) => s.replace(/[&<>"']/g, (ch) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[ch]);

/**
 * Highlights code into an escaped HTML string (for exports and tests).
 * @param {string} code
 * @param {string} lang
 * @returns {string}
 */
export function highlightToHTML(code, lang) {
  let s = '';
  for (const t of tokenize(code, lang)) s += t.type ? `<span class="hl-${t.type}">${escapeHtml(t.value)}</span>` : escapeHtml(t.value);
  return s;
}

/**
 * Best-effort language guess for unlabeled code. Returns null when unsure.
 * @param {string} code
 * @returns {string|null}
 */
export function detectLanguage(code) {
  const s = String(code ?? '').slice(0, 4000);
  if (!s.trim()) return null;
  const tests = [
    ['php', /<\?php/, 10],
    ['diff', /^(?:diff --git|@@ -\d+(?:,\d+)? \+\d+|--- a\/)/m, 8],
    ['bash', /^#!.*\b(?:ba|z)?sh\b/, 10],
    ['python', /^#!.*\bpython/, 10],
    ['html', /^\s*<!doctype html|<html[\s>]|<\/(?:div|span|body|head|p)>/i, 7],
    ['xml', /^\s*<\?xml\b/, 9],
    ['go', /^package \w+\s*$|^func (?:\(\w+ \*?\w+\) )?\w+\(|:= /m, 4],
    ['rust', /\bfn \w+\s*(?:<[^>]*>)?\(|\blet mut\b|\bimpl\b.*\{|println!\(/, 4],
    ['python', /^\s*(?:def \w+\(.*\)\s*(?:->\s*[\w\[\], .]+)?:|class \w+(?:\(.*\))?:|from [\w.]+ import |import \w+(?:\.\w+)*\s*$|if __name__ == )/m, 4],
    ['cpp', /#include\s*<(?:iostream|vector|string|memory|map)>|\bstd::|\btemplate\s*</, 5],
    ['c', /#include\s*[<"][\w/]+\.h[>"]|\bprintf\s*\(|\bint main\s*\(/, 3],
    ['csharp', /\busing System\b|\bnamespace [\w.]+|\bpublic (?:sealed |static |partial )*class \w+|\bConsole\.Write/, 4],
    ['java', /\bpublic static void main\b|\bSystem\.out\.print|\bimport java\./, 5],
    ['kotlin', /\bfun \w+\(|\bval \w+\s*[:=]/, 3],
    ['swift', /\bimport (?:UIKit|SwiftUI|Foundation)\b|\bfunc \w+\(.*\)\s*->|\bguard let\b/, 5],
    ['ruby', /^\s*(?:def \w+[?!]?(?:\(.*\))?\s*$|end\s*$|require ['"]|puts )/m, 3],
    ['sql', /\b(?:SELECT\s+[\s\S]+?\s+FROM|INSERT\s+INTO|CREATE\s+TABLE|UPDATE\s+\w+\s+SET|DELETE\s+FROM)\b/i, 5],
    ['powershell', /\$\w+\s*=|\b(?:Get|Set|New|Remove|Write|Invoke)-\w+|\bparam\s*\(/, 3],
    ['dockerfile', /^\s*FROM\s+[\w./:-]+(?:\s+AS\s+\w+)?\s*$/im, 6],
    ['typescript', /:\s*(?:string|number|boolean|void)\b|\binterface \w+\s*\{|\bimport type\b/, 3],
    ['javascript', /\b(?:const|let|var)\s+\w+\s*=|=>|\bfunction\s*\w*\s*\(|console\.log|\brequire\(|\bexport (?:default|const|function)/, 3],
    ['css', /^\s*[.#]?[\w-]+(?:[\s,>:.#[\]="\w-]*)\{\s*$|^\s*[\w-]+\s*:\s*[^;]+;\s*$/m, 2],
    ['json', /^\s*[{[]\s*(?:"[^"]*"\s*:|\[|\{|\d|"|true|false|null)/, 3],
    ['yaml', /^(?:---\s*$|[\w-]+:\s+\S|\s*- [\w-]+:\s)/m, 2],
    ['toml', /^\s*\[[\w.-]+\]\s*$[\s\S]*^\s*[\w-]+\s*=\s*/m, 3],
    ['ini', /^\s*\[[^\]]+\]\s*$[\s\S]*^\s*[\w.-]+\s*=/m, 2],
    ['bash', /^\s*(?:\$ |sudo |apt(?:-get)? |cd |ls |echo |export |curl |git |npm |docker )/m, 3],
    ['markdown', /^#{1,6} \S|^\s*[-*] \[[ x]\] |\[[^\]]+\]\([^)]+\)/m, 2],
  ];
  const scores = new Map();
  for (const [id, re, w] of tests) if (re.test(s)) scores.set(id, (scores.get(id) || 0) + w);
  if (scores.has('json')) { try { JSON.parse(code); scores.set('json', 20); } catch { scores.delete('json'); } }
  if (scores.has('typescript') && scores.has('javascript')) scores.set('typescript', scores.get('typescript') + scores.get('javascript'));
  let best = null, bestScore = 2;
  for (const [id, sc] of scores) if (sc > bestScore) { best = id; bestScore = sc; }
  return best;
}
