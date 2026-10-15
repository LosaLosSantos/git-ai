package chat

import (
	"fmt"
	"regexp"
)

// redaction replaces the matches of one pattern. repl may refer to groups.
type redaction struct {
	re   *regexp.Regexp
	repl string
}

const (
	// keyValueSeparator follows a secret key name: an optional closing quote of
	// the key, bare or escaped (`"`, `\"`, `\\\"`), then `:` or `=`.
	keyValueSeparator = `(?:\\*["'])?\s*[:=]\s*`
	// headerValue is a header value up to the end of the line or the closing
	// quote, bare or escaped. A backslash belongs to the value only when it
	// does not escape a quote.
	headerValue = `(?:[^\r\n"'\\]|\\+[^\r\n"'\\])+`
	// secretValue is a secret value in one of four shapes, each with its own
	// groups so that the replacement keeps the quotes around the placeholder:
	// an escaped-quoted value (\"...\", groups 2 and 3), a double-quoted value
	// with JSON escapes inside (groups 4 and 5), a single-quoted value (groups
	// 6 and 7) and, last, an unquoted or unterminated value (group 8 holds the
	// opening quote, if any). Quoted values are redacted whole, spaces
	// included.
	secretValue = `(?:` +
		`(\\+")[^"\r\n]*?(\\+")` +
		`|(")(?:[^"\\\r\n]|\\.)*(")` +
		`|(')[^'\r\n]*(')` +
		`|(\\*["']?)(?:[^\s,;"'}&\\]|\\+[^\s,;"'}&\\])+` +
		`)`
	// secretValueRepl replaces a key followed by secretValue (group 1 is the
	// key and the separator).
	secretValueRepl = "${1}${2}${4}${6}${8}[REDACTED]${3}${5}${7}"
)

// builtinRedactions are the secret shapes removed from every context and
// answer: private keys, bearer tokens, credentials in URLs, key=value secrets
// and well-known token formats.
//
// The order matters: whole-line rules (Authorization, Cookie) run before the
// generic key=value rule, so that the credential after the first word is
// removed too.
//
// Log lines are often JSON that embeds other JSON (request payloads, headers),
// where the quotes around a key or a value are escaped as \" (or \\\" one level
// deeper). The key and value rules therefore accept backslashes before every
// quote, and a quoted value is redacted up to its closing quote, spaces
// included.
var builtinRedactions = []redaction{
	{regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z0-9 ]*PRIVATE KEY-----`), "[REDACTED PRIVATE KEY]"},
	// A log tail can start inside a key: the base64 lines before a dangling END
	// marker, or everything after a BEGIN marker whose END was cut off.
	{regexp.MustCompile(`(?m)(?:^[A-Za-z0-9+/=]{16,}\r?\n)*[A-Za-z0-9+/=]*-----END [A-Z0-9 ]*PRIVATE KEY-----`), "[REDACTED PRIVATE KEY]"},
	{regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----[\s\S]*`), "[REDACTED PRIVATE KEY]"},
	// Authorization (Basic, Digest, Token, ...) and Cookie headers: the whole
	// value to the end of the line, not only the scheme or the first cookie.
	{regexp.MustCompile(`(?i)((?:proxy-)?authorization` + keyValueSeparator + `\\*["']?)` + headerValue), "${1}[REDACTED]"},
	{regexp.MustCompile(`(?i)((?:set-)?cookie` + keyValueSeparator + `\\*["']?)` + headerValue), "${1}[REDACTED]"},
	{regexp.MustCompile(`(?i)\b(bearer)\s+[A-Za-z0-9._~+/=-]{8,}`), "${1} [REDACTED]"},
	{regexp.MustCompile(`(?i)(\b[a-z][a-z0-9+.-]*://[^/\s:@]+:)[^@\s/]+@`), "${1}[REDACTED]@"},
	{regexp.MustCompile(`(?i)((?:password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|authorization|cookie|session)` + keyValueSeparator + `)` + secretValue), secretValueRepl},
	{regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`), "[REDACTED]"},
	{regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`), "[REDACTED]"},
	{regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}`), "[REDACTED]"},
	{regexp.MustCompile(`\bgl[a-z]{2,4}-[A-Za-z0-9_-]{20,}`), "[REDACTED]"},
	{regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}`), "[REDACTED]"},
}

// redactor removes secrets from text. It is immutable after construction.
type redactor struct {
	rules []redaction
}

// newRedactor returns the built-in redactions plus one replacement per extra
// pattern from the configuration.
func newRedactor(extra []string) (*redactor, error) {
	r := &redactor{rules: append([]redaction(nil), builtinRedactions...)}
	for _, p := range extra {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("chat config: redact pattern: %w", err)
		}
		r.rules = append(r.rules, redaction{re: re, repl: "[REDACTED]"})
	}
	return r, nil
}

// Apply returns s with every secret-shaped match replaced.
func (r *redactor) Apply(s string) string {
	for _, rule := range r.rules {
		s = rule.re.ReplaceAllString(s, rule.repl)
	}
	return s
}
