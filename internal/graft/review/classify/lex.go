package classify

import (
	"bytes"
	"path"
	"strings"
	"unicode/utf8"
)

// language identifies the lexer used for a file. A wrong COMMENT_ONLY lets a
// logic change pass review with fewer approvals, so only languages whose
// lexical structure is simple enough to be certain about have a lexer: Go,
// Java, Rust and Python. Everything else (C, C++, JavaScript, TypeScript,
// shell, Ruby, ...) is unknown and classified as code.
type language int

const (
	langUnknown language = iota
	langGo
	langJava
	langRust
	langPython
	numLanguages
)

const (
	// nlToken is a significant line break. Automatic semicolon insertion (Go)
	// and statement ends (Python) make line breaks part of the program, so they
	// are kept in the token stream; Java and Rust do not record them. Consecutive breaks collapse into one, and
	// leading or trailing breaks are dropped, so blank lines and a missing final
	// newline do not matter.
	nlToken = "\n"
	// indPrefix marks the indentation of a Python code line. Indentation is
	// significant in Python, so it is kept verbatim.
	indPrefix = "\x01"
)

// maxLexSize bounds the source size that is lexed. Larger files are classified
// as code, which keeps the token slice and the nesting work bounded.
const maxLexSize = 1 << 20

// utf8BOM is the UTF-8 encoding of U+FEFF.
var utf8BOM = []byte("\xef\xbb\xbf")

// hasLoneCR reports whether src contains a carriage return that is not part of
// a "\r\n" sequence.
func hasLoneCR(src []byte) bool {
	for i, c := range src {
		if c == '\r' && (i+1 >= len(src) || src[i+1] != '\n') {
			return true
		}
	}
	return false
}

// hashComment skips a Python "#" comment that starts at s.pos. Comments that
// carry meaning for the interpreter (shebang lines and source encoding
// declarations) are not comments to the program, so they make the lexer give
// up.
func (s *scanner) hashComment() {
	end := s.pos
	for end < len(s.src) && s.src[end] != '\n' {
		end++
	}
	if text := s.src[s.pos:end]; bytes.HasPrefix(text, []byte("#!")) || bytes.Contains(text, []byte("coding")) {
		s.fail("meaningful # comment (shebang or encoding declaration), not lexed")
		return
	}
	s.pos = end
}

// languageOf maps a file path to the lexer for its extension. Only the
// languages for which COMMENT_ONLY can be decided with certainty have one;
// every other extension is unknown.
func languageOf(filePath string) language {
	switch strings.ToLower(path.Ext(path.Base(filePath))) {
	case ".go":
		return langGo
	case ".java":
		return langJava
	case ".rs":
		return langRust
	case ".py":
		return langPython
	}
	return langUnknown
}

// tokenize returns the significant token stream of src: comments and
// whitespace are dropped, everything else is kept verbatim. ok is false when
// the lexer is not certain of the token boundaries; reason then says why, and
// the caller must treat the file as code.
func tokenize(lang language, src []byte) (toks []string, reason string, ok bool) {
	if bytes.IndexByte(src, 0) >= 0 {
		return nil, "binary content", false
	}
	if len(src) > maxLexSize {
		return nil, "too large to lex", false
	}
	if !utf8.Valid(src) {
		return nil, "invalid UTF-8", false
	}
	// The languages end a line comment at a lone carriage return, and Java
	// unicode escapes become line breaks or quotes before lexing. The lexer only
	// knows "\n", so such sources are not lexed.
	if hasLoneCR(src) {
		return nil, "lone carriage return", false
	}
	if lang == langJava && bytes.Contains(src, []byte(`\u`)) {
		return nil, "unicode escape in Java source", false
	}
	// A shebang line is skipped by Rust (and by Java source launchers) up to
	// the line break, so a "/*" on it does not open a comment there. "#![" is a
	// Rust inner attribute, not a shebang.
	if head := bytes.TrimPrefix(src, utf8BOM); lang != langPython &&
		bytes.HasPrefix(head, []byte("#!")) && !bytes.HasPrefix(head, []byte("#![")) {
		return nil, "shebang line, not lexed", false
	}
	s := &scanner{src: src, lang: lang}
	switch lang {
	case langGo, langJava, langRust:
		s.lexC()
	case langPython:
		s.lexPython()
	default:
		return nil, "unsupported language", false
	}
	if s.why != "" {
		return nil, s.why, false
	}
	return s.finish(), "", true
}

// scanner is the state shared by all lexers. Any construct whose boundaries
// cannot be determined with certainty calls fail, which ends the scan with an
// uncertain result.
type scanner struct {
	src  []byte
	lang language
	pos  int
	toks []string
	// bol is set at the beginning of a physical line (Python indentation).
	bol bool
	// why is non-empty once the lexer gave up.
	why string
}

func (s *scanner) fail(reason string) {
	if s.why == "" {
		s.why = reason
	}
	s.pos = len(s.src)
}

// at returns the byte at i, or 0 outside the source.
func (s *scanner) at(i int) byte {
	if i >= 0 && i < len(s.src) {
		return s.src[i]
	}
	return 0
}

func (s *scanner) emit(tok string) {
	s.toks = append(s.toks, tok)
}

// newline records a significant line break, collapsing repeated breaks.
func (s *scanner) newline() {
	if n := len(s.toks); n > 0 && s.toks[n-1] != nlToken {
		s.toks = append(s.toks, nlToken)
	}
}

func (s *scanner) finish() []string {
	if n := len(s.toks); n > 0 && s.toks[n-1] == nlToken {
		s.toks = s.toks[:n-1]
	}
	return s.toks
}

// skipLine advances to the end of the current line without consuming the
// line break.
func (s *scanner) skipLine() {
	for s.pos < len(s.src) && s.src[s.pos] != '\n' {
		s.pos++
	}
}

// quoted emits the string or character literal that starts at s.pos.
func (s *scanner) quoted(multiline bool) {
	end, ok := quotedEnd(s.src, s.pos, multiline)
	if !ok {
		s.fail("unterminated string or character literal")
		return
	}
	s.emit(string(s.src[s.pos:end]))
	s.pos = end
}

// plainWord emits an identifier or keyword.
func (s *scanner) plainWord() {
	start := s.pos
	for s.pos < len(s.src) && isIdentChar(s.src[s.pos]) {
		s.pos++
	}
	s.emit(string(s.src[start:s.pos]))
}

// number emits a numeric literal. Signs are part of the literal only directly
// after an exponent mark, so "1e+5" stays one token while "1 + 5" does not.
// The literal may swallow more than the language would (for example "0xE+1"),
// which only makes token streams differ more often.
func (s *scanner) number() {
	start := s.pos
	s.pos++
	for s.pos < len(s.src) {
		c := s.src[s.pos]
		if (c == '+' || c == '-') && strings.IndexByte("eEpP", s.src[s.pos-1]) >= 0 {
			s.pos++
		} else if isIdentChar(c) || c == '.' {
			s.pos++
		} else {
			break
		}
	}
	s.emit(string(s.src[start:s.pos]))
}

// operator emits the longest operator from ops that starts at s.pos, or a
// single byte. ops must be ordered longest first.
func (s *scanner) operator(ops []string) {
	rest := s.src[s.pos:]
	for _, op := range ops {
		if len(rest) >= len(op) && string(rest[:len(op)]) == op {
			s.emit(op)
			s.pos += len(op)
			return
		}
	}
	s.emit(string(rest[:1]))
	s.pos++
}

// isSpace reports whether c is a blank in every supported language. A carriage
// return only reaches the lexers as part of "\r\n". Form feeds and vertical
// tabs are not blanks in every language, so they are kept as tokens: adding or
// removing one is a code change.
func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r'
}

func isDigit(c byte) bool {
	return c >= '0' && c <= '9'
}

func isLetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// isIdentStart reports whether c may start an identifier. Non-ASCII bytes are
// glued to identifiers, so any change to them changes a token.
func isIdentStart(c byte) bool {
	return isLetter(c) || c == '_' || c == '$' || c >= 0x80
}

func isIdentChar(c byte) bool {
	return isIdentStart(c) || isDigit(c)
}

// quotedEnd returns the index just after the quoted literal that starts at
// start, where src[start] is the quote. A backslash escapes the next byte.
// Unless multiline is set, a line break ends the search unsuccessfully.
func quotedEnd(src []byte, start int, multiline bool) (int, bool) {
	q := src[start]
	for i := start + 1; i < len(src); {
		c := src[i]
		switch {
		case c == '\\':
			i += 2
		case c == q:
			return i + 1, true
		case c == '\n' && !multiline:
			return 0, false
		default:
			i++
		}
	}
	return 0, false
}

// tripleEnd returns the index just after the triple-quoted literal that starts
// at start (a Python triple-quoted string or a Java text block). A backslash
// escapes the next byte.
func tripleEnd(src []byte, start int, q byte) (int, bool) {
	for i := start + 3; i < len(src); {
		switch {
		case src[i] == '\\':
			i += 2
		case src[i] == q && i+2 < len(src) && src[i+1] == q && src[i+2] == q:
			return i + 3, true
		default:
			i++
		}
	}
	return 0, false
}

func equalTokens(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
