// Package gen builds the dashboard's single embedded document from the source modules
// under internal/webui/src.
//
// It is deliberately a GO package with a stdlib-only import set. The binary embeds one
// self-contained index.html with go:embed, and `make build` must keep producing that file
// with the Go toolchain alone: no JavaScript runtime, no bundler, no registry package, no
// lockfile and no network. So the "build" here is a concatenation the Go standard library
// can do, and the generated document is COMMITTED - the gate proves it is not stale
// rather than regenerating it behind the build.
//
// Failure is total, never partial. Every source is read and validated before a single
// byte is written, and the write itself goes to a temp file in the destination directory
// and is renamed into place, so a generator that fails leaves the committed document
// exactly as it found it.
package gen

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

const (
	// TemplateFile is the page shell: the markup, with one marker for the stylesheet and
	// one for the script.
	TemplateFile = "index.html.tmpl"
	// StyleFile is the whole stylesheet. The phase does not split the CSS; it moves as a
	// unit.
	StyleFile = "dashboard.css"
	// ManifestFile names the script modules, in the order they are concatenated.
	ManifestFile = "js/modules.txt"
	// ModuleDir is the only directory a manifest entry may name.
	ModuleDir = "js"

	// cssMarker and jsMarker are the two substitution points in TemplateFile. Each must
	// appear exactly once.
	cssMarker = "/*holdfast:inline-css*/"
	jsMarker  = "//holdfast:inline-js"

	doctype = "<!DOCTYPE html>\n"

	banner = `<!-- GENERATED FILE - do not edit by hand.
     Built from internal/webui/src by internal/webui/gen, with the Go toolchain alone:
     no JavaScript runtime, no bundler, no registry package, no lockfile, no network.
     Edit the modules under internal/webui/src and run ` + "`make webui-gen`" + `;
     ` + "`make check`" + ` fails when this file is stale. -->
`
)

// Document generates the embedded dashboard document from the source tree rooted at
// fsys (the contents of internal/webui/src). Every error names the offending file.
func Document(fsys fs.FS) ([]byte, error) {
	tmpl, err := readText(fsys, TemplateFile)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(tmpl, doctype) {
		return nil, fmt.Errorf("%s: the page shell must begin with %q", TemplateFile, strings.TrimSuffix(doctype, "\n"))
	}
	if n := strings.Count(tmpl, cssMarker); n != 1 {
		return nil, fmt.Errorf("%s: the stylesheet marker %q appears %d times, want exactly 1", TemplateFile, cssMarker, n)
	}
	if n := strings.Count(tmpl, jsMarker); n != 1 {
		return nil, fmt.Errorf("%s: the script marker %q appears %d times, want exactly 1", TemplateFile, jsMarker, n)
	}

	css, err := readText(fsys, StyleFile)
	if err != nil {
		return nil, err
	}
	if i := indexFold(css, "</style"); i >= 0 {
		return nil, fmt.Errorf("%s: contains %q at byte %d, which would close the inlined <style> element early", StyleFile, "</style", i)
	}

	mods, err := Modules(fsys)
	if err != nil {
		return nil, err
	}
	var js strings.Builder
	for _, m := range mods {
		body, err := readText(fsys, m)
		if err != nil {
			return nil, err
		}
		if i := indexFold(body, "</script"); i >= 0 {
			return nil, fmt.Errorf("%s: contains %q at byte %d, which would close the inlined <script> element early", m, "</script", i)
		}
		if err := scanJS(body); err != nil {
			return nil, fmt.Errorf("%s: %w", m, err)
		}
		js.WriteString("// --- internal/webui/src/" + m + " ---\n")
		js.WriteString(strings.TrimRight(body, "\n"))
		js.WriteString("\n")
	}

	out := doctype + banner + strings.TrimPrefix(tmpl, doctype)
	out = strings.Replace(out, cssMarker, strings.TrimRight(css, "\n"), 1)
	out = strings.Replace(out, jsMarker, strings.TrimRight(js.String(), "\n"), 1)
	return []byte(out), nil
}

// Modules returns the script modules named by the manifest, in order, as paths relative
// to fsys. A manifest naming something that is not a readable .js file inside ModuleDir
// is an error naming that entry.
func Modules(fsys fs.FS) ([]string, error) {
	text, err := readText(fsys, ManifestFile)
	if err != nil {
		return nil, err
	}
	var out []string
	for i, line := range strings.Split(text, "\n") {
		name := strings.TrimSpace(line)
		if name == "" || strings.HasPrefix(name, "#") {
			continue
		}
		p := path.Join(ModuleDir, name)
		if !fs.ValidPath(p) || path.Dir(p) != ModuleDir || !strings.HasSuffix(name, ".js") {
			return nil, fmt.Errorf("%s:%d: %q is not a .js module inside %s/", ManifestFile, i+1, name, ModuleDir)
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: names no script module", ManifestFile)
	}
	return out, nil
}

// readText reads one source file, reporting the file by name on any failure. fs.FS
// surfaces a permission error the same way it surfaces a missing file, which is why B4's
// "missing, unreadable, or cannot be parsed" are one code path here.
func readText(fsys fs.FS, name string) (string, error) {
	b, err := fs.ReadFile(fsys, name)
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return "", fmt.Errorf("%s: is empty", name)
	}
	return string(b), nil
}

func indexFold(s, sub string) int { return strings.Index(strings.ToLower(s), sub) }

// --- the command ---------------------------------------------------------------

// Main is the whole body of the generator command, exposed so the tests drive the real
// thing rather than a stand-in. It returns the process exit code.
func Main(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("genindex", flag.ContinueOnError)
	fl.SetOutput(stderr)
	src := fl.String("src", filepath.Join("internal", "webui", "src"), "directory holding the dashboard source modules")
	out := fl.String("out", filepath.Join("internal", "webui", "index.html"), "the generated, go:embed-ed document")
	check := fl.Bool("check", false, "do not write: fail if the committed document is not what the sources generate")
	if err := fl.Parse(args); err != nil {
		return 2
	}

	doc, err := Document(os.DirFS(*src))
	if err != nil {
		fmt.Fprintf(stderr, "genindex: cannot generate %s: %s\n", *out, err)
		fmt.Fprintf(stderr, "genindex: %s is unchanged\n", *out)
		return 1
	}

	have, readErr := os.ReadFile(*out)
	if *check {
		if readErr != nil {
			fmt.Fprintf(stderr, "genindex: stale artifact %s: %s\n", *out, readErr)
			return 3
		}
		if !bytes.Equal(have, doc) {
			fmt.Fprintf(stderr, "genindex: stale artifact %s: it is not what %s generates (%d bytes committed, %d generated).\n",
				*out, *src, len(have), len(doc))
			fmt.Fprintf(stderr, "genindex: first difference at byte %d. Regenerate with `make webui-gen` and commit %s.\n",
				firstDiff(have, doc), *out)
			return 3
		}
		fmt.Fprintf(stdout, "genindex: %s is current (%d bytes)\n", *out, len(doc))
		return 0
	}

	if readErr == nil && bytes.Equal(have, doc) {
		fmt.Fprintf(stdout, "genindex: %s already current (%d bytes)\n", *out, len(doc))
		return 0
	}
	if err := writeAtomic(*out, doc); err != nil {
		fmt.Fprintf(stderr, "genindex: cannot write %s: %s\n", *out, err)
		return 1
	}
	fmt.Fprintf(stdout, "genindex: wrote %s (%d bytes)\n", *out, len(doc))
	return 0
}

// writeAtomic writes through a temp file in the destination's own directory and renames
// it into place, so a failed write can never leave a partial page behind.
func writeAtomic(name string, data []byte) error {
	dir := filepath.Dir(name)
	f, err := os.CreateTemp(dir, ".genindex-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, name)
}

func firstDiff(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

// --- the JavaScript lexical check ----------------------------------------------

// ErrUnbalanced and friends are returned by scanJS. They are lexical, not semantic: the
// generator inlines the module into a <script> element, so a module whose strings,
// comments or brackets do not close would silently swallow every module after it. That
// is the "cannot be parsed" half of B4, and it is the half a concatenating build can
// actually check without a JavaScript toolchain.
var errUnterminated = errors.New("unterminated")

// regexKeywords are the identifiers after which a `/` starts a regular expression rather
// than a division.
var regexKeywords = map[string]bool{
	"return": true, "typeof": true, "instanceof": true, "in": true, "of": true,
	"new": true, "delete": true, "void": true, "throw": true, "case": true,
	"do": true, "else": true, "yield": true, "await": true,
}

type jsScanner struct {
	src   string
	i     int
	line  int
	stack []byte
	// prev is the previous significant token: "" at the start, "ident:<text>", or
	// "punct:<b>". It decides whether a `/` opens a regular expression.
	prev string
}

func scanJS(src string) error {
	s := &jsScanner{src: src, line: 1}
	return s.run()
}

func (s *jsScanner) run() error {
	for s.i < len(s.src) {
		c := s.src[s.i]
		switch {
		case c == '\n':
			s.line++
			s.i++
		case c == ' ' || c == '\t' || c == '\r':
			s.i++
		case c == '/' && s.peek(1) == '/':
			for s.i < len(s.src) && s.src[s.i] != '\n' {
				s.i++
			}
		case c == '/' && s.peek(1) == '*':
			start := s.line
			s.i += 2
			for {
				if s.i+1 >= len(s.src) {
					return fmt.Errorf("line %d: %w block comment", start, errUnterminated)
				}
				if s.src[s.i] == '\n' {
					s.line++
				}
				if s.src[s.i] == '*' && s.src[s.i+1] == '/' {
					s.i += 2
					break
				}
				s.i++
			}
		case c == '/':
			if s.regexAllowed() {
				if err := s.scanRegex(); err != nil {
					return err
				}
			} else {
				s.i++
				if s.peek(0) == '=' {
					s.i++
				}
				s.prev = "punct:/"
			}
		case c == '"' || c == '\'':
			if err := s.scanString(c); err != nil {
				return err
			}
		case c == '`':
			if err := s.scanTemplate(); err != nil {
				return err
			}
		case c == '(' || c == '[' || c == '{':
			s.stack = append(s.stack, c)
			s.i++
			s.prev = "punct:" + string(c)
		case c == ')' || c == ']' || c == '}':
			open := map[byte]byte{')': '(', ']': '[', '}': '{'}[c]
			if len(s.stack) == 0 {
				return fmt.Errorf("line %d: unbalanced %q with nothing open", s.line, string(c))
			}
			if got := s.stack[len(s.stack)-1]; got != open {
				return fmt.Errorf("line %d: %q closes a %q", s.line, string(c), string(got))
			}
			s.stack = s.stack[:len(s.stack)-1]
			s.i++
			s.prev = "punct:" + string(c)
		case isIdentStart(c):
			start := s.i
			for s.i < len(s.src) && isIdentPart(s.src[s.i]) {
				s.i++
			}
			s.prev = "ident:" + s.src[start:s.i]
		case c >= '0' && c <= '9':
			for s.i < len(s.src) && (isIdentPart(s.src[s.i]) || s.src[s.i] == '.') {
				s.i++
			}
			s.prev = "ident:0"
		default:
			s.i++
			s.prev = "punct:" + string(c)
		}
	}
	if len(s.stack) > 0 {
		return fmt.Errorf("%w: %d bracket(s) still open at end of file (innermost %q)",
			errUnterminated, len(s.stack), string(s.stack[len(s.stack)-1]))
	}
	return nil
}

func (s *jsScanner) peek(n int) byte {
	if s.i+n >= len(s.src) {
		return 0
	}
	return s.src[s.i+n]
}

// regexAllowed decides the one genuinely ambiguous character in JavaScript's grammar.
func (s *jsScanner) regexAllowed() bool {
	if s.prev == "" {
		return true
	}
	if name, ok := strings.CutPrefix(s.prev, "ident:"); ok {
		return regexKeywords[name]
	}
	switch s.prev {
	case "punct:)", "punct:]", "punct:}":
		return false
	}
	return true
}

func (s *jsScanner) scanRegex() error {
	start := s.line
	s.i++ // the opening slash
	inClass := false
	for {
		if s.i >= len(s.src) || s.src[s.i] == '\n' {
			return fmt.Errorf("line %d: %w regular expression literal", start, errUnterminated)
		}
		switch s.src[s.i] {
		case '\\':
			s.i++
		case '[':
			inClass = true
		case ']':
			inClass = false
		case '/':
			if !inClass {
				s.i++
				for s.i < len(s.src) && isIdentPart(s.src[s.i]) {
					s.i++
				}
				s.prev = "ident:0"
				return nil
			}
		}
		s.i++
	}
}

func (s *jsScanner) scanString(quote byte) error {
	start := s.line
	s.i++
	for {
		if s.i >= len(s.src) {
			return fmt.Errorf("line %d: %w string literal", start, errUnterminated)
		}
		switch s.src[s.i] {
		case '\\':
			if s.peek(1) == '\n' {
				s.line++
			}
			s.i++
		case '\n':
			return fmt.Errorf("line %d: %w string literal (newline inside %q)", start, errUnterminated, string(quote))
		case quote:
			s.i++
			s.prev = "ident:0"
			return nil
		}
		s.i++
	}
}

func (s *jsScanner) scanTemplate() error {
	start := s.line
	s.i++
	for {
		if s.i >= len(s.src) {
			return fmt.Errorf("line %d: %w template literal", start, errUnterminated)
		}
		switch {
		case s.src[s.i] == '\\':
			s.i++
		case s.src[s.i] == '\n':
			s.line++
		case s.src[s.i] == '$' && s.peek(1) == '{':
			s.i += 2
			depth := 1
			for depth > 0 {
				if s.i >= len(s.src) {
					return fmt.Errorf("line %d: %w template substitution", start, errUnterminated)
				}
				switch s.src[s.i] {
				case '\n':
					s.line++
				case '{':
					depth++
				case '}':
					depth--
				}
				s.i++
			}
			continue
		case s.src[s.i] == '`':
			s.i++
			s.prev = "ident:0"
			return nil
		}
		s.i++
	}
}

func isIdentStart(c byte) bool {
	return c == '_' || c == '$' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}

func isIdentPart(c byte) bool { return isIdentStart(c) || (c >= '0' && c <= '9') }
