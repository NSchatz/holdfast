package main

import (
	"regexp"
	"sort"
	"strings"
)

// What a source is READ as. Everything below works on one string per file with every
// comment blanked out in place, so a byte offset still names the line it was written on
// and a failure can point at it.

// decl is one CSS declaration, with the block preludes enclosing it. The preludes are
// what tell `--accent` declared on `:root` apart from the same token declared again
// inside `@media (prefers-color-scheme: dark)`.
type decl struct {
	file    string
	line    int
	prop    string
	value   string
	context []string
}

// underAtRule reports whether an at-rule opened any enclosing block, which is this
// check's definition of "not the default declaration".
func (d decl) underAtRule() bool {
	for _, c := range d.context {
		if strings.HasPrefix(c, "@") {
			return true
		}
	}
	return false
}

// where names the declaration for a reader who has to go and fix it.
func (d decl) where() string { return positionOf(d.file, d.line) }

// source is one identity source, read.
type source struct {
	path  string
	size  int
	text  string // comments blanked, offsets preserved
	decls []decl
	idx   *lineIndex
	segs  []segment
}

// segments lazily splits the whole source into alphanumeric runs, which is what the
// twelve string entries of the blocklist are matched against.
func (s *source) segments() []segment {
	if s.segs == nil {
		s.segs = segmentsOf(s.text)
		if s.segs == nil {
			s.segs = []segment{}
		}
	}
	return s.segs
}

// at names the line an offset falls on and hands back that line's text, so a hit can
// quote what it saw rather than assert it.
func (s *source) at(off int) (int, string) {
	line := s.idx.line(off)
	from := s.idx.starts[line-1]
	to := len(s.text)
	if line < len(s.idx.starts) {
		to = s.idx.starts[line] - 1
	}
	text := strings.TrimSpace(s.text[from:to])
	if len(text) > 160 {
		text = text[:160] + "..."
	}
	return line, text
}

// lineIndex turns a byte offset into the 1-based line it falls on.
type lineIndex struct{ starts []int }

func newLineIndex(text string) *lineIndex {
	idx := &lineIndex{starts: []int{0}}
	for i := 0; i < len(text); i++ {
		if text[i] == '\n' {
			idx.starts = append(idx.starts, i+1)
		}
	}
	return idx
}

func (l *lineIndex) line(off int) int {
	i := sort.SearchInts(l.starts, off+1)
	if i < 1 {
		return 1
	}
	return i
}

// blankComments replaces every CSS comment, and in an HTML document every markup
// comment, with spaces - keeping the newlines, so every offset still names its own line.
//
// A comment paints nothing and ships no face, so the scan below must not read one as a
// hit. The other direction is the risk this trade takes: a `/*` inside a string literal
// would blank text that is really there. That is a MISSED hit rather than an invented
// one, and it is what the self-test's fifteen defeats are for - each mutates a real
// declaration in a real file, so a blanking bug shows up as a defeat that went green.
func blankComments(text string, html bool) string {
	b := []byte(text)
	blank := func(from, to int) {
		for i := from; i < to && i < len(b); i++ {
			if b[i] != '\n' {
				b[i] = ' '
			}
		}
	}
	for i := 0; i < len(b); i++ {
		switch {
		case b[i] == '/' && i+1 < len(b) && b[i+1] == '*':
			end := strings.Index(text[i+2:], "*/")
			if end < 0 {
				blank(i, len(b)) // unterminated: CSS reads it to the end, so we do too
				return string(b)
			}
			blank(i, i+2+end+2)
			i = i + 2 + end + 1
		case html && strings.HasPrefix(text[i:], "<!--"):
			end := strings.Index(text[i+4:], "-->")
			if end < 0 {
				blank(i, len(b))
				return string(b)
			}
			blank(i, i+4+end+3)
			i = i + 4 + end + 2
		}
	}
	return string(b)
}

var (
	reProp        = regexp.MustCompile(`^-{0,2}[A-Za-z_][A-Za-z0-9_-]*$`)
	reStyleOpen   = regexp.MustCompile(`(?i)<style\b[^>]*>`)
	reStyleAttr   = regexp.MustCompile(`(?i)style\s*=\s*"([^"]*)"`)
	reStyleAttr2  = regexp.MustCompile(`(?i)style\s*=\s*'([^']*)'`)
	reCloseStyle  = regexp.MustCompile(`(?i)</style\s*>`)
	reWhitespaceR = regexp.MustCompile(`\s+`)
)

// normalizeSpace collapses runs of whitespace to one space. Values are compared after it,
// so a font stack wrapped across two lines still equals the one written on one.
func normalizeSpace(s string) string {
	return strings.TrimSpace(reWhitespaceR.ReplaceAllString(s, " "))
}

// parseStylesheet reads declarations out of text[from:to], tracking the block preludes.
func parseStylesheet(file, text string, from, to int, idx *lineIndex) []decl {
	var out []decl
	var stack []string
	seg := from
	i := from
	for i < to {
		switch text[i] {
		case '"', '\'':
			i = skipQuoted(text, i, to)
			continue
		case '(':
			i = skipParens(text, i, to)
			continue
		case '{':
			stack = append(stack, normalizeSpace(text[seg:i]))
			i++
			seg = i
			continue
		case '}':
			if d, ok := declarationIn(file, text, seg, i, stack, idx); ok {
				out = append(out, d)
			}
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			i++
			seg = i
			continue
		case ';':
			if d, ok := declarationIn(file, text, seg, i, stack, idx); ok {
				out = append(out, d)
			}
			i++
			seg = i
			continue
		}
		i++
	}
	return out
}

func declarationIn(file, text string, from, to int, stack []string, idx *lineIndex) (decl, bool) {
	if len(stack) == 0 || from >= to {
		return decl{}, false // an at-rule statement at the top level is not a declaration
	}
	raw := text[from:to]
	colon := strings.Index(raw, ":")
	if colon < 0 {
		return decl{}, false
	}
	prop := strings.TrimSpace(raw[:colon])
	if !reProp.MatchString(prop) {
		return decl{}, false // a selector, not a property
	}
	lead := len(raw) - len(strings.TrimLeft(raw, " \t\r\n"))
	return decl{
		file:    file,
		line:    idx.line(from + lead),
		prop:    prop,
		value:   normalizeSpace(raw[colon+1:]),
		context: append([]string(nil), stack...),
	}, true
}

func skipQuoted(text string, i, to int) int {
	q := text[i]
	i++
	for i < to {
		if text[i] == '\\' {
			i += 2
			continue
		}
		if text[i] == q {
			return i + 1
		}
		i++
	}
	return to
}

func skipParens(text string, i, to int) int {
	depth := 0
	for i < to {
		switch text[i] {
		case '"', '\'':
			i = skipQuoted(text, i, to)
			continue
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i + 1
			}
		}
		i++
	}
	return to
}

// readSource reads one identity source and pulls its declarations out of it. A `.css`
// file is a stylesheet whole; an HTML document carries its CSS in `<style>` blocks and in
// `style=` attributes, and this repository's page carries all of it in one inline block.
func readSource(path string, raw []byte) source {
	html := !strings.HasSuffix(path, ".css")
	text := blankComments(string(raw), html)
	s := source{path: path, size: len(raw), text: text, idx: newLineIndex(text)}
	if !html {
		s.decls = parseStylesheet(path, text, 0, len(text), s.idx)
		return s
	}
	for _, open := range reStyleOpen.FindAllStringIndex(text, -1) {
		from := open[1]
		to := len(text)
		if close := reCloseStyle.FindStringIndex(text[from:]); close != nil {
			to = from + close[0]
		}
		s.decls = append(s.decls, parseStylesheet(path, text, from, to, s.idx)...)
	}
	for _, re := range []*regexp.Regexp{reStyleAttr, reStyleAttr2} {
		for _, m := range re.FindAllStringSubmatchIndex(text, -1) {
			s.decls = append(s.decls, parseInline(path, text, m[2], m[3], s.idx)...)
		}
	}
	return s
}

// parseInline reads a `style="..."` attribute, which is a declaration list with no block
// around it.
func parseInline(file, text string, from, to int, idx *lineIndex) []decl {
	var out []decl
	seg := from
	for i := from; i <= to; i++ {
		if i < to && text[i] != ';' {
			continue
		}
		if d, ok := declarationIn(file, text, seg, i, []string{"style="}, idx); ok {
			out = append(out, d)
		}
		seg = i + 1
	}
	return out
}
