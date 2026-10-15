package resolve

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// parseTOML parses the TOML subset used by graft-resolve configuration. It is
// deliberately small so that the module needs no third-party dependency:
//
//   - comments, blank lines, bare and quoted keys;
//   - [table] and [[array of tables]] headers, with dotted names that descend
//     into the last element of an array of tables;
//   - key = value with basic strings ("...", with escapes), literal strings
//     ('...'), booleans, integers, floats, and arrays (possibly multi-line,
//     with trailing commas allowed).
//
// Inline tables, multi-line strings, dotted keys in assignments and dates are
// rejected with a line number.
func parseTOML(src string) (map[string]any, error) {
	p := &tomlParser{src: src, line: 1, explicit: map[string]bool{}}
	root := map[string]any{}
	cur := root
	for {
		p.skipSpace()
		if p.eof() {
			return root, nil
		}
		if p.peek() == '[' {
			t, err := p.parseHeader(root)
			if err != nil {
				return nil, err
			}
			cur = t
			continue
		}
		key, err := p.parseKey()
		if err != nil {
			return nil, err
		}
		p.skipBlanks()
		if p.peek() != '=' {
			return nil, p.errf("expected '=' after key %q", key)
		}
		p.pos++
		p.skipBlanks()
		val, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		if _, dup := cur[key]; dup {
			return nil, p.errf("duplicate key %q", key)
		}
		cur[key] = val
		if err := p.endOfLine(); err != nil {
			return nil, err
		}
	}
}

type tomlParser struct {
	src  string
	pos  int
	line int
	// explicit records the tables defined by a [header]. A table that was only
	// created implicitly by a longer header ([a.b] creates a) may be defined
	// later, as TOML allows.
	explicit map[string]bool
}

func (p *tomlParser) eof() bool { return p.pos >= len(p.src) }

func (p *tomlParser) peek() byte {
	if p.eof() {
		return 0
	}
	return p.src[p.pos]
}

func (p *tomlParser) errf(format string, args ...any) error {
	return fmt.Errorf("config line %d: %s", p.line, fmt.Sprintf(format, args...))
}

// skipBlanks skips spaces and tabs only.
func (p *tomlParser) skipBlanks() {
	for !p.eof() && (p.peek() == ' ' || p.peek() == '\t') {
		p.pos++
	}
}

// skipSpace skips whitespace, newlines and comments (used between items).
func (p *tomlParser) skipSpace() {
	for !p.eof() {
		switch c := p.peek(); {
		case c == ' ' || c == '\t' || c == '\r':
			p.pos++
		case c == '\n':
			p.pos++
			p.line++
		case c == '#':
			for !p.eof() && p.peek() != '\n' {
				p.pos++
			}
		default:
			return
		}
	}
}

// endOfLine requires that only blanks and an optional comment follow.
func (p *tomlParser) endOfLine() error {
	p.skipBlanks()
	if p.peek() == '#' {
		for !p.eof() && p.peek() != '\n' {
			p.pos++
		}
	}
	if p.eof() {
		return nil
	}
	if p.peek() == '\r' {
		p.pos++
	}
	if p.peek() != '\n' {
		return p.errf("unexpected %q after value", p.peek())
	}
	p.pos++
	p.line++
	return nil
}

// parseHeader parses [name] or [[name]] and returns the table that following
// assignments write into.
func (p *tomlParser) parseHeader(root map[string]any) (map[string]any, error) {
	p.pos++ // '['
	array := p.peek() == '['
	if array {
		p.pos++
	}
	p.skipBlanks()
	var segs []string
	for {
		k, err := p.parseKey()
		if err != nil {
			return nil, err
		}
		segs = append(segs, k)
		p.skipBlanks()
		if p.peek() != '.' {
			break
		}
		p.pos++
		p.skipBlanks()
	}
	if p.peek() != ']' {
		return nil, p.errf("expected ']' in table header")
	}
	p.pos++
	if array {
		if p.peek() != ']' {
			return nil, p.errf("expected ']]' in table header")
		}
		p.pos++
	}
	if err := p.endOfLine(); err != nil {
		return nil, err
	}
	t, err := p.navigate(root, segs, array)
	if err != nil {
		return nil, p.errf("%v", err)
	}
	return t, nil
}

// navigate returns the table named by segs under root, creating it. The last
// segment of an array header appends a new element.
func (p *tomlParser) navigate(root map[string]any, segs []string, array bool) (map[string]any, error) {
	cur := root
	// where identifies the table path; each element of an array of tables gets
	// its own index so that [strategy.llm] may appear under every element.
	where := ""
	for _, seg := range segs[:len(segs)-1] {
		where += seg + "\x00"
		v, ok := cur[seg]
		if !ok {
			next := map[string]any{}
			cur[seg] = next
			cur = next
			continue
		}
		switch t := v.(type) {
		case map[string]any:
			cur = t
		case []map[string]any:
			if len(t) == 0 {
				return nil, fmt.Errorf("table %q has no elements", seg)
			}
			cur = t[len(t)-1]
			where += fmt.Sprintf("[%d]\x00", len(t)-1)
		default:
			return nil, fmt.Errorf("%q is not a table", seg)
		}
	}
	last := segs[len(segs)-1]
	if array {
		elem := map[string]any{}
		switch existing := cur[last].(type) {
		case nil:
			cur[last] = []map[string]any{elem}
		case []map[string]any:
			cur[last] = append(existing, elem)
		default:
			return nil, fmt.Errorf("%q is not an array of tables", last)
		}
		return elem, nil
	}
	id := where + last
	if p.explicit[id] {
		return nil, fmt.Errorf("table %q defined twice", last)
	}
	p.explicit[id] = true
	if existing, ok := cur[last]; ok {
		t, isMap := existing.(map[string]any)
		if !isMap {
			return nil, fmt.Errorf("%q is not a table", last)
		}
		return t, nil
	}
	t := map[string]any{}
	cur[last] = t
	return t, nil
}

func isBareKeyByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_'
}

func (p *tomlParser) parseKey() (string, error) {
	if p.peek() == '"' {
		return p.parseBasicString()
	}
	start := p.pos
	for !p.eof() && isBareKeyByte(p.peek()) {
		p.pos++
	}
	if p.pos == start {
		return "", p.errf("expected a key")
	}
	return p.src[start:p.pos], nil
}

func (p *tomlParser) parseValue() (any, error) {
	switch c := p.peek(); {
	case c == '"':
		return p.parseBasicString()
	case c == '\'':
		return p.parseLiteralString()
	case c == '[':
		return p.parseArray()
	case c == '{':
		return nil, p.errf("inline tables are not supported")
	case p.hasWord("true"):
		p.pos += len("true")
		return true, nil
	case p.hasWord("false"):
		p.pos += len("false")
		return false, nil
	}
	return p.parseNumber()
}

func (p *tomlParser) hasWord(w string) bool {
	return strings.HasPrefix(p.src[p.pos:], w) && (p.pos+len(w) == len(p.src) || !isBareKeyByte(p.src[p.pos+len(w)]))
}

func (p *tomlParser) parseNumber() (any, error) {
	start := p.pos
	for !p.eof() {
		c := p.peek()
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '#' || c == ',' || c == ']' {
			break
		}
		p.pos++
	}
	tok := strings.ReplaceAll(p.src[start:p.pos], "_", "")
	if tok == "" {
		return nil, p.errf("expected a value")
	}
	if n, err := strconv.ParseInt(tok, 10, 64); err == nil {
		return n, nil
	}
	if strings.ContainsAny(tok, ".eE") {
		if f, err := strconv.ParseFloat(tok, 64); err == nil {
			return f, nil
		}
	}
	return nil, p.errf("unsupported value %q", tok)
}

func (p *tomlParser) parseBasicString() (string, error) {
	p.pos++ // opening quote
	var b strings.Builder
	for {
		if p.eof() {
			return "", p.errf("unterminated string")
		}
		c := p.peek()
		switch {
		case c == '"':
			p.pos++
			return b.String(), nil
		case c == '\n':
			return "", p.errf("newline in string")
		case c == '\\':
			p.pos++
			if p.eof() {
				return "", p.errf("unterminated escape")
			}
			esc := p.peek()
			p.pos++
			switch esc {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case '"', '\\':
				b.WriteByte(esc)
			case 'u', 'U':
				n := 4
				if esc == 'U' {
					n = 8
				}
				if p.pos+n > len(p.src) {
					return "", p.errf("short unicode escape")
				}
				r, err := strconv.ParseUint(p.src[p.pos:p.pos+n], 16, 32)
				if err != nil || !utf8.ValidRune(rune(r)) {
					return "", p.errf("invalid unicode escape")
				}
				b.WriteRune(rune(r))
				p.pos += n
			default:
				return "", p.errf("unsupported escape \\%c", esc)
			}
		default:
			b.WriteByte(c)
			p.pos++
		}
	}
}

func (p *tomlParser) parseLiteralString() (string, error) {
	p.pos++ // opening quote
	start := p.pos
	for {
		if p.eof() || p.peek() == '\n' {
			return "", p.errf("unterminated literal string")
		}
		if p.peek() == '\'' {
			s := p.src[start:p.pos]
			p.pos++
			return s, nil
		}
		p.pos++
	}
}

func (p *tomlParser) parseArray() ([]any, error) {
	p.pos++ // '['
	out := []any{}
	for {
		p.skipSpace()
		if p.eof() {
			return nil, p.errf("unterminated array")
		}
		if p.peek() == ']' {
			p.pos++
			return out, nil
		}
		v, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		p.skipSpace()
		switch p.peek() {
		case ',':
			p.pos++
		case ']':
		default:
			return nil, p.errf("expected ',' or ']' in array")
		}
	}
}
