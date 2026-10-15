package classify

import (
	"bytes"
	"unicode/utf8"
)

// The multi-character operators of each language, longest first. "a++b" and
// "a+ +b" differ, while "a = -1" and "a=-1" do not. Each list is exactly the
// operators of its language, so that the lexer splits a run of operator
// characters the way the language's own scanner does (maximal munch). An
// operator missing from a list would let a blank inside it go unnoticed, and an
// operator of another language would merge characters that the language splits:
// with Java's ">>>" in the Go list, ">>> >" and ">>>>" both lexed as ">>>" ">",
// while Go reads ">>" ">" ">" and ">>" ">>".
var (
	goOps = []string{
		"<<=", ">>=", "&^=", "...",
		"&&", "||", "<-", "++", "--", "==", "!=", "<=", ">=", ":=",
		"+=", "-=", "*=", "/=", "%=", "&=", "|=", "^=", "<<", ">>", "&^",
	}
	javaOps = []string{
		">>>=",
		"<<=", ">>=", ">>>", "...",
		"->", "::", "++", "--", "&&", "||", "==", "!=", "<=", ">=",
		"+=", "-=", "*=", "/=", "%=", "&=", "|=", "^=", "<<", ">>",
	}
	// rustOps are the glued punctuation tokens of Rust. Rust has no "++" or
	// "--", so "a - -b" and "a --b" lex alike, as they do for rustc.
	rustOps = []string{
		"<<=", ">>=", "...", "..=",
		"->", "<-", "=>", "::", "..", "&&", "||", "==", "!=", "<=", ">=",
		"+=", "-=", "*=", "/=", "%=", "&=", "|=", "^=", "<<", ">>",
	}
)

// cOps returns the operator list of a C-like language.
func cOps(lang language) []string {
	switch lang {
	case langGo:
		return goOps
	case langJava:
		return javaOps
	default:
		return rustOps
	}
}

// goSpecialComment reports whether a comment that starts at rest is a Go
// directive or a cgo/linker comment. Such comments are compiled, so they are
// not comments to the program.
func goSpecialComment(rest []byte) bool {
	var body []byte
	switch {
	case bytes.HasPrefix(rest, []byte("//")):
		body = bytes.TrimLeft(rest[2:], " \t")
	case bytes.HasPrefix(rest, []byte("/*")):
		body = rest[2:]
	default:
		return false
	}
	for _, prefix := range []string{"go:", "export", "line", "+build", "extern"} {
		if bytes.HasPrefix(body, []byte(prefix)) {
			return true
		}
	}
	return false
}

// lexC lexes Go, Java and Rust. Their comment, string and literal syntax is
// close enough to share one scanner; the language-specific rules are switched
// on by s.lang.
func (s *scanner) lexC() {
	if s.lang == langGo && (bytes.Contains(s.src, []byte(`"C"`)) || bytes.Contains(s.src, []byte("`C`")) ||
		bytes.Contains(s.src, []byte("#cgo"))) {
		// cgo compiles the comment preamble, so its contents are code. The import
		// path may be an interpreted or a raw string literal.
		s.fail("cgo import or #cgo line, not lexed")
		return
	}
	if s.lang == langRust && bytes.Contains(s.src, []byte("stringify!")) {
		// stringify! turns the spelling of its tokens into a string.
		s.fail("stringify! in Rust source, not lexed")
		return
	}
	for s.pos < len(s.src) && s.why == "" {
		c := s.src[s.pos]
		next := s.at(s.pos + 1)
		switch {
		case c == '\n':
			s.pos++
			s.lineBreak()
		case isSpace(c):
			s.pos++
		case c == '/' && next == '/':
			if s.lang == langGo && goSpecialComment(s.src[s.pos:]) {
				s.fail("Go directive comment, not lexed")
				return
			}
			if s.isLineDoc(s.src[s.pos:]) {
				// Doc comments are #[doc] attributes in Rust and Markdown
				// documentation comments in Java, so they are code.
				start := s.pos
				s.skipLine()
				s.emit(string(bytes.TrimRight(s.src[start:s.pos], "\r")))
				break
			}
			s.skipLine()
		case c == '/' && next == '*':
			if s.lang == langGo && goSpecialComment(s.src[s.pos:]) {
				s.fail("Go directive comment, not lexed")
				return
			}
			if s.isBlockDoc(s.src[s.pos:]) {
				start := s.pos
				s.blockComment(s.lang == langRust)
				if s.why == "" {
					s.emit(string(s.src[start:s.pos]))
				}
				break
			}
			s.blockComment(s.lang == langRust)
		case c == '"' && s.lang == langJava && next == '"' && s.at(s.pos+2) == '"':
			s.javaTextBlock()
		case c == '"':
			s.quoted(s.lang == langRust)
			s.rustNoSuffix()
		case c == '\'' && s.lang == langRust:
			s.rustQuote()
		case c == '\'':
			s.quoted(false)
		case c == '`' && s.lang == langGo:
			s.rawGoString()
		case isIdentStart(c):
			s.cWord()
		case isDigit(c):
			s.number()
		case c == '.' && isDigit(next) && s.lang != langRust:
			// In Go and Java ".5" is one floating-point literal, while ". 5" is a
			// dot and an integer. In Rust a dot before a digit is a tuple field
			// access ("t.0"), where blanks around the dot do not matter.
			s.number()
		default:
			s.operator(cOps(s.lang))
		}
	}
}

// isLineDoc reports whether the line comment at rest is a doc comment, which
// is code. In Rust, "///" and "//!" are doc comments but four slashes are an
// ordinary comment. In Java, "///" starts a Markdown documentation comment.
func (s *scanner) isLineDoc(rest []byte) bool {
	switch s.lang {
	case langRust:
		if bytes.HasPrefix(rest, []byte("////")) {
			return false
		}
		return bytes.HasPrefix(rest, []byte("///")) || bytes.HasPrefix(rest, []byte("//!"))
	case langJava:
		return bytes.HasPrefix(rest, []byte("///"))
	}
	return false
}

// isBlockDoc reports whether the block comment at rest is a doc comment, which
// is code. In Rust, "/**" and "/*!" start doc comments, but "/**/" and "/***"
// are ordinary comments. In Java, every "/**" comment except the empty "/**/"
// is a Javadoc comment.
func (s *scanner) isBlockDoc(rest []byte) bool {
	switch s.lang {
	case langRust:
		if bytes.HasPrefix(rest, []byte("/*!")) {
			return true
		}
		return bytes.HasPrefix(rest, []byte("/**")) && len(rest) > 3 && rest[3] != '*' && rest[3] != '/'
	case langJava:
		return bytes.HasPrefix(rest, []byte("/**")) && !bytes.HasPrefix(rest, []byte("/**/"))
	}
	return false
}

// javaTextBlock emits a Java text block that starts at s.pos. The opening
// delimiter must be followed by blanks and a line break; the block is kept
// verbatim, so any change inside it, including to blanks, is a code change.
func (s *scanner) javaTextBlock() {
	i := s.pos + 3
	for i < len(s.src) && (s.src[i] == ' ' || s.src[i] == '\t' || s.src[i] == '\f' || s.src[i] == '\r') {
		i++
	}
	if s.at(i) != '\n' {
		s.fail("malformed text block opening, not lexed")
		return
	}
	end, ok := tripleEnd(s.src, s.pos, '"')
	if !ok {
		s.fail("unterminated text block")
		return
	}
	s.emit(string(s.src[s.pos:end]))
	s.pos = end
}

// cWord emits an identifier. In Rust it also joins literal prefixes to their
// literal (r"...", r#"..."#, br"...", cr"...", b"...", c"...", b'.') and
// raw identifiers (r#name), because in macro input "b\"x\"" and "b \"x\"" are
// different token trees.
func (s *scanner) cWord() {
	start := s.pos
	for s.pos < len(s.src) && isIdentChar(s.src[s.pos]) {
		s.pos++
	}
	w := string(s.src[start:s.pos])
	if s.lang != langRust {
		s.emit(w)
		return
	}
	next := s.at(s.pos)
	switch {
	case (w == "r" || w == "br" || w == "cr") && (next == '"' || next == '#'):
		if q, ok := rawStringStart(s.src, s.pos); ok {
			hashes := q - s.pos
			closing := append([]byte{'"'}, bytes.Repeat([]byte{'#'}, hashes)...)
			idx := bytes.Index(s.src[q+1:], closing)
			if idx < 0 {
				s.fail("unterminated raw string")
				return
			}
			end := q + 1 + idx + len(closing)
			s.emit(string(s.src[start:end]))
			s.pos = end
			s.rustNoSuffix()
			return
		}
		if w == "r" && next == '#' && isIdentStart(s.at(s.pos+1)) {
			end := s.pos + 1
			for end < len(s.src) && isIdentChar(s.src[end]) {
				end++
			}
			s.emit(string(s.src[start:end]))
			s.pos = end
			return
		}
		s.fail("unclear raw literal or identifier prefix")
	case (w == "b" || w == "c") && next == '"':
		end, ok := quotedEnd(s.src, s.pos, true)
		if !ok {
			s.fail("unterminated string")
			return
		}
		s.emit(string(s.src[start:end]))
		s.pos = end
		s.rustNoSuffix()
	case w == "b" && next == '\'':
		end, ok := quotedEnd(s.src, s.pos, false)
		if !ok {
			s.fail("unterminated byte literal")
			return
		}
		s.emit(string(s.src[start:end]))
		s.pos = end
		s.rustNoSuffix()
	case next == '"' || next == '\'' || next == '#':
		// Reserved prefixes (Rust 2021) or tokens whose joint spelling a macro
		// could observe.
		s.fail("identifier directly followed by a quote or '#', not lexed")
	default:
		s.emit(w)
	}
}

// rustNoSuffix fails when a Rust literal that just ended is directly followed by
// an identifier character: a literal suffix, which this lexer does not join.
func (s *scanner) rustNoSuffix() {
	if s.lang == langRust && s.why == "" && isIdentChar(s.at(s.pos)) {
		s.fail("literal with a suffix, not lexed")
	}
}

// rawStringStart reports whether the hashes that start at i are followed by
// the quote of a raw string, and returns the index of that quote.
func rawStringStart(src []byte, i int) (int, bool) {
	q := i
	for q < len(src) && src[q] == '#' {
		q++
	}
	return q, q < len(src) && src[q] == '"'
}

// blockComment skips a block comment that starts at s.pos. Rust block comments
// nest. A line break inside a comment is a line break for the language, so it
// is recorded.
func (s *scanner) blockComment(nested bool) {
	depth, hasNL := 1, false
	i := s.pos + 2
	for i < len(s.src) && depth > 0 {
		c := s.src[i]
		switch {
		case c == '*' && s.at(i+1) == '/':
			depth--
			i += 2
		case nested && c == '/' && s.at(i+1) == '*':
			depth++
			i += 2
		default:
			if c == '\n' {
				hasNL = true
			}
			i++
		}
	}
	if depth > 0 {
		s.fail("unterminated block comment")
		return
	}
	s.pos = i
	if hasNL {
		s.lineBreak()
	}
}

// lineBreak records a line break outside literals and line comments. Go
// inserts semicolons at line breaks, so they are tokens there; in Java and Rust
// a line break is only a blank.
func (s *scanner) lineBreak() {
	if s.lang == langGo {
		s.newline()
	}
}

// rustQuote handles a single quote in Rust, which opens either a character
// literal or a lifetime.
func (s *scanner) rustQuote() {
	if s.pos+1 >= len(s.src) {
		s.fail("unterminated quote")
		return
	}
	if s.src[s.pos+1] == '\\' {
		s.quoted(false)
		s.rustNoSuffix()
		return
	}
	_, size := utf8.DecodeRune(s.src[s.pos+1:])
	after := s.pos + 1 + size
	if s.at(after) == '\'' {
		s.emit(string(s.src[s.pos : after+1]))
		s.pos = after + 1
		s.rustNoSuffix()
		return
	}
	if isIdentStart(s.src[s.pos+1]) {
		end := s.pos + 1
		for end < len(s.src) && isIdentChar(s.src[end]) {
			end++
		}
		s.emit(string(s.src[s.pos:end]))
		s.pos = end
		return
	}
	s.fail("unclear use of a single quote")
}

// rawGoString emits a Go raw string literal, which may span lines.
func (s *scanner) rawGoString() {
	idx := bytes.IndexByte(s.src[s.pos+1:], '`')
	if idx < 0 {
		s.fail("unterminated raw string")
		return
	}
	end := s.pos + 2 + idx
	s.emit(string(s.src[s.pos:end]))
	s.pos = end
}
