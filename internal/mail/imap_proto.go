package mail

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Low-level IMAP4rev1 response reading (RFC 3501 §7, §9). A server response is one "logical
// line": text that may embed literals ("{n}\r\n" followed by n raw bytes) before the line
// continues. readResponse collects the text segments and literals under strict bounds; the
// lexer then tokenises them.

// Protocol bounds.
const (
	imapMaxLine     = 1 << 20 // one text segment (a long "* SEARCH" line fits ~150k UIDs)
	imapMaxSmallLit = 1 << 20 // literal budget per response outside message fetches
	imapMaxParts    = 4096    // literals per response
	imapMaxDepth    = 16      // parenthesised list nesting
	imapMaxListLen  = 100000  // elements in one list
)

var (
	errIMAPLineTooLong = errors.New("imap: response line too long")
	errIMAPLiteral     = errors.New("imap: response literal exceeds the size limit")
)

// errMalformed reports a protocol violation by the server.
func errMalformed(format string, a ...any) error {
	return fmt.Errorf("imap: malformed server response: %s", fmt.Sprintf(format, a...))
}

// response is one logical server response: texts[i] is followed by lits[i].
type response struct {
	texts [][]byte
	lits  [][]byte
}

// readLine reads one CRLF-terminated line (a bare LF is tolerated) of at most imapMaxLine
// bytes, without the terminator.
func readLine(r *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		frag, err := r.ReadSlice('\n')
		if len(line)+len(frag) > imapMaxLine+2 {
			return nil, errIMAPLineTooLong
		}
		line = append(line, frag...)
		if err == nil {
			break
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			return nil, io.ErrUnexpectedEOF
		}
		return nil, err
	}
	line = bytes.TrimSuffix(line, []byte("\n"))
	line = bytes.TrimSuffix(line, []byte("\r"))
	if len(line) > imapMaxLine {
		return nil, errIMAPLineTooLong
	}
	if bytes.IndexByte(line, 0) >= 0 {
		return nil, errMalformed("NUL byte in response line")
	}
	return line, nil
}

// literalSuffix reports a trailing "{n}" (or "{n+}") literal marker and returns the text
// before it.
func literalSuffix(line []byte) (prefix []byte, n int64, ok bool, err error) {
	if len(line) == 0 || line[len(line)-1] != '}' {
		return line, 0, false, nil
	}
	open := bytes.LastIndexByte(line, '{')
	if open < 0 {
		return line, 0, false, nil
	}
	digits := bytes.TrimSuffix(line[open+1:len(line)-1], []byte("+"))
	if len(digits) == 0 || len(digits) > 12 {
		return line, 0, false, nil
	}
	for _, d := range digits {
		if d < '0' || d > '9' {
			return line, 0, false, nil
		}
	}
	n, perr := strconv.ParseInt(string(digits), 10, 64)
	if perr != nil {
		return nil, 0, false, errMalformed("bad literal length")
	}
	return line[:open], n, true, nil
}

// readResponse reads one logical response. Literals may total at most litBudget bytes; a
// larger announced literal fails before any of it is read (the connection is then unusable).
func readResponse(r *bufio.Reader, litBudget int64) (*response, error) {
	resp := &response{}
	var used int64
	for {
		line, err := readLine(r)
		if err != nil {
			return nil, err
		}
		prefix, n, ok, err := literalSuffix(line)
		if err != nil {
			return nil, err
		}
		if !ok {
			resp.texts = append(resp.texts, line)
			return resp, nil
		}
		if len(resp.lits) >= imapMaxParts {
			return nil, errMalformed("too many literals in one response")
		}
		if n > litBudget-used {
			return nil, errIMAPLiteral
		}
		used += n
		lit := make([]byte, n)
		if _, err := io.ReadFull(r, lit); err != nil {
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return nil, err
		}
		resp.texts = append(resp.texts, prefix)
		resp.lits = append(resp.lits, lit)
	}
}

type tokKind int

const (
	tokEOL tokKind = iota
	tokAtom
	tokString
	tokLParen
	tokRParen
)

type token struct {
	kind tokKind
	atom string // tokAtom
	str  []byte // tokString (quoted or literal)
}

// lexer tokenises a response. Atoms may contain a bracketed section ("BODY[]",
// "BODY[HEADER.FIELDS (FROM)]<0>") which is kept whole.
type lexer struct {
	r   *response
	seg int
	pos int
}

func newLexer(r *response) *lexer { return &lexer{r: r} }

func (l *lexer) cur() []byte { return l.r.texts[l.seg] }

func (l *lexer) skipSpaces() {
	t := l.cur()
	for l.pos < len(t) && t[l.pos] == ' ' {
		l.pos++
	}
}

func (l *lexer) next() (token, error) {
	l.skipSpaces()
	t := l.cur()
	if l.pos >= len(t) {
		if l.seg < len(l.r.lits) {
			lit := l.r.lits[l.seg]
			l.seg++
			l.pos = 0
			return token{kind: tokString, str: lit}, nil
		}
		return token{kind: tokEOL}, nil
	}
	switch c := t[l.pos]; c {
	case '(':
		l.pos++
		return token{kind: tokLParen}, nil
	case ')':
		l.pos++
		return token{kind: tokRParen}, nil
	case '"':
		return l.quoted()
	default:
		return l.atom()
	}
}

func (l *lexer) quoted() (token, error) {
	t := l.cur()
	l.pos++ // opening quote
	var out []byte
	for l.pos < len(t) {
		c := t[l.pos]
		l.pos++
		switch c {
		case '"':
			return token{kind: tokString, str: out}, nil
		case '\\':
			if l.pos >= len(t) {
				return token{}, errMalformed("unterminated quoted string")
			}
			out = append(out, t[l.pos])
			l.pos++
		default:
			out = append(out, c)
		}
	}
	return token{}, errMalformed("unterminated quoted string")
}

func (l *lexer) atom() (token, error) {
	t := l.cur()
	start := l.pos
	for l.pos < len(t) {
		c := t[l.pos]
		if c == ' ' || c == '(' || c == ')' || c == '"' || c < 0x20 || c == 0x7f {
			break
		}
		if c == '[' {
			end := bytes.IndexByte(t[l.pos:], ']')
			if end < 0 {
				return token{}, errMalformed("unterminated '['")
			}
			l.pos += end + 1
			continue
		}
		l.pos++
	}
	if l.pos == start {
		return token{}, errMalformed("unexpected character %q", t[l.pos])
	}
	return token{kind: tokAtom, atom: string(t[start:l.pos])}, nil
}

// rest returns the remaining raw text of the current segment (after one space), for
// human-readable resp-text.
func (l *lexer) rest() string {
	t := l.cur()
	if l.pos < len(t) && t[l.pos] == ' ' {
		l.pos++
	}
	s := string(t[min(l.pos, len(t)):])
	l.pos = len(t)
	return s
}

// atomValue reads the next token, which must be an atom.
func (l *lexer) atomValue() (string, error) {
	tk, err := l.next()
	if err != nil {
		return "", err
	}
	if tk.kind != tokAtom {
		return "", errMalformed("expected an atom")
	}
	return tk.atom, nil
}

// nilAtom marks NIL in parsed values.
type nilAtom struct{}

// value parses one value: an atom (string), a string ([]byte), NIL (nilAtom{}) or a
// parenthesised list ([]any).
func (l *lexer) value(depth int) (any, error) {
	tk, err := l.next()
	if err != nil {
		return nil, err
	}
	return l.valueFrom(tk, depth)
}

func (l *lexer) valueFrom(tk token, depth int) (any, error) {
	switch tk.kind {
	case tokAtom:
		if strings.EqualFold(tk.atom, "NIL") {
			return nilAtom{}, nil
		}
		return tk.atom, nil
	case tokString:
		return tk.str, nil
	case tokLParen:
		if depth >= imapMaxDepth {
			return nil, errMalformed("lists nested too deeply")
		}
		var list []any
		for {
			tk, err := l.next()
			if err != nil {
				return nil, err
			}
			if tk.kind == tokRParen {
				if list == nil {
					list = []any{}
				}
				return list, nil
			}
			if tk.kind == tokEOL {
				return nil, errMalformed("unterminated list")
			}
			if len(list) >= imapMaxListLen {
				return nil, errMalformed("list too long")
			}
			v, err := l.valueFrom(tk, depth+1)
			if err != nil {
				return nil, err
			}
			list = append(list, v)
		}
	case tokRParen:
		return nil, errMalformed("unexpected ')'")
	default:
		return nil, errMalformed("unexpected end of response")
	}
}

// parseNumber parses an IMAP number (unsigned 32-bit).
func parseNumber(s string) (uint32, error) {
	if s == "" || len(s) > 10 {
		return 0, errMalformed("bad number %q", s)
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, errMalformed("bad number %q", s)
		}
	}
	n, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0, errMalformed("bad number %q", s)
	}
	return uint32(n), nil
}

// statusTail parses "[CODE args] text" after a status word.
func statusTail(l *lexer) (code, text string) {
	s := l.rest()
	if strings.HasPrefix(s, "[") {
		if end := strings.IndexByte(s, ']'); end > 0 {
			code, s = s[1:end], strings.TrimPrefix(s[end+1:], " ")
		}
	}
	return code, s
}

// quoteArg encodes s as an IMAP quoted string, or reports that it needs a literal (8-bit
// characters, CR/LF or other controls). NUL can never be sent.
func quoteArg(s string) (quoted string, needLiteral bool, err error) {
	if strings.IndexByte(s, 0) >= 0 {
		return "", false, errors.New("imap: NUL in argument")
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c > 0x7e {
			return "", true, nil
		}
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(s) + `"`, false, nil
}
