package classify

import (
	"bytes"
	"strings"
)

// pyOps are the Python operators whose spelling matters, longest first.
var pyOps = []string{
	"**=", "//=", ">>=", "<<=", "...",
	"->", "**", "//", "==", "!=", "<=", ">=", "+=", "-=", "*=", "/=", "%=",
	"&=", "|=", "^=", "@=", ":=", "<<", ">>",
}

// lexPython lexes Python. Comments are dropped; docstrings and all other
// strings are code. Indentation of every code line is kept, because it is
// part of the program. Blank and comment-only lines contribute nothing. A line
// continuation backslash is a token together with its line break.
func (s *scanner) lexPython() {
	s.bol = true
	for s.pos < len(s.src) && s.why == "" {
		if s.bol {
			s.bol = false
			j := s.pos
			for j < len(s.src) && (s.src[j] == ' ' || s.src[j] == '\t' || s.src[j] == '\f') {
				j++
			}
			if j >= len(s.src) || s.src[j] == '\n' || s.src[j] == '\r' || s.src[j] == '#' {
				s.pos = j
				continue
			}
			s.emit(indPrefix + string(s.src[s.pos:j]))
			s.pos = j
			continue
		}
		c := s.src[s.pos]
		switch {
		case c == '\n':
			s.pos++
			s.newline()
			s.bol = true
		case isSpace(c):
			s.pos++
		case c == '#':
			s.hashComment()
		case c == '\\':
			s.pyContinuation()
		case c == '"' || c == '\'':
			s.pyString("")
		case isIdentStart(c):
			s.pyWord()
		case isDigit(c):
			s.number()
		default:
			s.operator(pyOps)
		}
	}
}

// pyContinuation handles a backslash outside a string, which must end the line.
// The backslash and its line break form one token, and the next line is not a
// new logical line: neither its indentation nor its being blank is skipped, so
// a blank or comment-only line after a backslash stays visible.
func (s *scanner) pyContinuation() {
	switch {
	case s.at(s.pos+1) == '\n':
		s.pos += 2
	case s.at(s.pos+1) == '\r' && s.at(s.pos+2) == '\n':
		s.pos += 3
	default:
		s.fail("backslash not at the end of a line")
		return
	}
	s.emit("\\\n")
}

// pyWord emits a name. A name directly followed by a quote is a string prefix
// (r, b, f, rb, ...) and forms one token with the string, because "r'x'" and
// "r 'x'" are different programs.
func (s *scanner) pyWord() {
	start := s.pos
	for s.pos < len(s.src) && isIdentChar(s.src[s.pos]) {
		s.pos++
	}
	w := string(s.src[start:s.pos])
	if q := s.at(s.pos); q == '"' || q == '\'' {
		if !isPyStringPrefix(w) {
			s.fail("name directly followed by a quote, not lexed")
			return
		}
		s.pyString(w)
		return
	}
	s.emit(w)
}

// isPyStringPrefix reports whether w is a valid Python string prefix.
func isPyStringPrefix(w string) bool {
	switch strings.ToLower(w) {
	case "r", "u", "b", "f", "t", "br", "rb", "fr", "rf", "tr", "rt":
		return true
	}
	return false
}

// pyString emits a Python string literal, single or triple quoted, that starts
// at s.pos, joined with its prefix.
func (s *scanner) pyString(prefix string) {
	q := s.src[s.pos]
	var end int
	var ok bool
	if s.at(s.pos+1) == q && s.at(s.pos+2) == q {
		end, ok = tripleEnd(s.src, s.pos, q)
	} else {
		end, ok = quotedEnd(s.src, s.pos, false)
	}
	if !ok {
		s.fail("unterminated Python string")
		return
	}
	tok := s.src[s.pos:end]
	// f-strings and t-strings (Python 3.14) carry replacement fields, which are
	// code that may contain strings and comments of its own.
	if strings.ContainsAny(prefix, "fFtT") && bytes.IndexByte(tok, '{') >= 0 {
		s.fail("f-string with replacement fields, not lexed")
		return
	}
	s.emit(prefix + string(tok))
	s.pos = end
}
