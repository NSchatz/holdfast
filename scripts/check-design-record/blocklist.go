package main

import (
	"fmt"
	"regexp"
	"strings"
)

// THE C2 BLOCKLIST, and the only place it is written.
//
// interface-craft C2, verbatim: the declared faces are not Inter, Roboto, Helvetica,
// Arial, Space Grotesk or a bare system stack alone; the accent is not Tailwind
// `indigo-500`/`blue-600` or an untouched shadcn `zinc`/`slate` ramp; no surface ships a
// purple-to-blue gradient, a gradient `bg-clip-text` heading, glassmorphism, or a uniform
// `rounded-2xl` + `shadow-lg` pairing.
//
// Fifteen entries, because each one has to be defeatable ON ITS OWN: `make
// check-design-record-selftest` mutates a copy of the tree once per entry and requires
// the check to red and to say what it saw. An entry that bundled two identifiers would be
// proved by whichever of them the self-test happened to write.
//
// Twelve are matched as SEGMENT SEQUENCES in the source, which is how a name survives the
// spellings it really appears in: `helvetica` is found inside the family `"Helvetica
// Neue"`, and `zinc` inside the class `bg-zinc-500`, without `inter` being found inside
// the word `pointer`. Three are descriptions rather than strings - a bare system stack
// alone, a purple-to-blue gradient, and glassmorphism - and each has a detector that
// decides it from the declarations.
//
// NOTHING HERE IS AN IDENTITY SOURCE. This file declares the blocklist and the design
// record names its exceptions, and neither is scanned: the sources are the four listed in
// main.go and no other file is opened for scanning at all, so a hit can only come from
// the surface.

// hit is one blocklist entry found in one identity source.
type hit struct {
	entry  string
	file   string
	line   int
	detail string
}

// entry is one blocklist entry, its id (which is what the design record excepts) and its
// detector.
type entry struct {
	id     string
	what   string
	detect func(s *source, cat map[string]string) []hit
}

// blocklist is the whole list, in C2's own order.
var blocklist = []entry{
	face("inter", "Inter", "Inter"),
	face("roboto", "Roboto", "Roboto"),
	face("helvetica", "Helvetica", "Helvetica"),
	face("arial", "Arial", "Arial"),
	face("space-grotesk", "Space Grotesk", "Space Grotesk"),
	{id: "bare-system-stack", what: "a bare system stack alone", detect: detectBareSystemStack},
	ident("indigo-500", "the Tailwind accent indigo-500"),
	ident("blue-600", "the Tailwind accent blue-600"),
	ident("zinc", "an untouched shadcn zinc ramp"),
	ident("slate", "an untouched shadcn slate ramp"),
	{id: "purple-to-blue-gradient", what: "a purple-to-blue gradient", detect: detectPurpleToBlue},
	ident("bg-clip-text", "a gradient bg-clip-text heading"),
	{id: "glassmorphism", what: "glassmorphism (a backdrop blur behind a translucent surface)", detect: detectGlassmorphism},
	ident("rounded-2xl", "the uniform rounded-2xl half of the rounded-2xl + shadow-lg pairing"),
	ident("shadow-lg", "the shadow-lg half of the rounded-2xl + shadow-lg pairing"),
}

func entryIDs() []string {
	out := make([]string, 0, len(blocklist))
	for _, e := range blocklist {
		out = append(out, e.id)
	}
	return out
}

// face builds the detector for a blocklisted typeface. The name is matched as a sequence
// of words anywhere in the source with the comments taken out, so it is found in a
// `font-family`, in a `--font-*` token, in a `style=` attribute and in the href of a font
// service - all four of which really do ship a face.
func face(id, name, what string) entry {
	want := lowerSegments(name)
	return entry{
		id:   id,
		what: "the typeface " + what,
		detect: func(s *source, _ map[string]string) []hit {
			return matches(s, id, want, anySeparator, "names the face "+name)
		},
	}
}

// ident builds the detector for a framework identifier. Segments join with a hyphen or an
// underscore and with nothing else, which is what makes `indigo-500` a hit inside
// `bg-indigo-500` and not inside the sentence "indigo 500".
func ident(id, what string) entry {
	want := lowerSegments(id)
	return entry{
		id:   id,
		what: what,
		detect: func(s *source, _ map[string]string) []hit {
			return matches(s, id, want, hyphenSeparator, "carries the identifier "+id)
		},
	}
}

// --- the segment matcher -------------------------------------------------------------

// segment is one run of letters and digits, lowercased, with where it was found.
type segment struct {
	text string
	from int
	to   int
}

func segmentsOf(text string) []segment {
	var out []segment
	start := -1
	for i := 0; i <= len(text); i++ {
		alnum := i < len(text) && isAlnum(text[i])
		switch {
		case alnum && start < 0:
			start = i
		case !alnum && start >= 0:
			out = append(out, segment{text: strings.ToLower(text[start:i]), from: start, to: i})
			start = -1
		}
	}
	return out
}

func isAlnum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

func lowerSegments(s string) []string {
	var out []string
	for _, seg := range segmentsOf(s) {
		out = append(out, seg.text)
	}
	return out
}

// anySeparator allows the gaps a face name really appears with: a space, a quote, a plus
// in a query string.
func anySeparator(sep string) bool { return len(sep) <= 3 }

// hyphenSeparator is the framework-identifier joiner and nothing else.
func hyphenSeparator(sep string) bool { return sep == "-" || sep == "_" }

func matches(s *source, id string, want []string, joins func(string) bool, saying string) []hit {
	if len(want) == 0 {
		return nil
	}
	segs := s.segments()
	var out []hit
	for i := 0; i+len(want) <= len(segs); i++ {
		ok := true
		for j := range want {
			if segs[i+j].text != want[j] {
				ok = false
				break
			}
			if j > 0 && !joins(s.text[segs[i+j-1].to:segs[i+j].from]) {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		line, text := s.at(segs[i].from)
		out = append(out, hit{entry: id, file: s.path, line: line, detail: saying + ": " + text})
	}
	return out
}

// --- the three that are descriptions rather than strings -------------------------------

// genericFamilies are the CSS generic families. One of them present is how this check
// decides a declaration is a font stack at all, rather than guessing from the property
// name - `--font-ui` and `font-family` and a `style=` attribute all reach the same test.
var genericFamilies = map[string]bool{
	"serif": true, "sans-serif": true, "monospace": true, "cursive": true,
	"fantasy": true, "system-ui": true, "ui-serif": true, "ui-sans-serif": true,
	"ui-monospace": true, "ui-rounded": true, "math": true, "emoji": true, "fangsong": true,
}

// systemFamilies are the faces a host already has: the platform UI faces, the metric
// substitutes a distribution ships, and the generics. A stack built only from these NAMES
// NO CHOSEN FACE, which is what C2's "a bare system stack alone" is about.
var systemFamilies = map[string]bool{
	"-apple-system": true, "blinkmacsystemfont": true, "segoe ui": true,
	"segoe ui emoji": true, "segoe ui symbol": true, "roboto": true,
	"helvetica neue": true, "helvetica": true, "arial": true, "noto sans": true,
	"noto color emoji": true, "liberation sans": true, "liberation mono": true,
	"liberation serif": true, "dejavu sans": true, "dejavu sans mono": true,
	"dejavu serif": true, "cantarell": true, "ubuntu": true, "oxygen": true,
	"oxygen-sans": true, "fira sans": true, "droid sans": true, "apple color emoji": true,
	"menlo": true, "monaco": true, "consolas": true, "courier new": true, "courier": true,
	"lucida console": true, "sf mono": true, "sf pro text": true, "andale mono": true,
	"times new roman": true, "georgia": true, "tahoma": true, "verdana": true,
	"segoe ui mono": true, "cascadia mono": true, "noto sans mono": true,
}

func detectBareSystemStack(s *source, _ map[string]string) []hit {
	var out []hit
	for _, d := range s.decls {
		families := splitTopLevel(d.value, ',')
		if len(families) == 0 {
			continue
		}
		generic := false
		chosen := ""
		for _, f := range families {
			name := normalizeFamily(f)
			if genericFamilies[name] {
				generic = true
			}
			if !genericFamilies[name] && !systemFamilies[name] {
				chosen = f
			}
		}
		if !generic || chosen != "" {
			continue
		}
		out = append(out, hit{entry: "bare-system-stack", file: d.file, line: d.line,
			detail: fmt.Sprintf("`%s` is a stack of %d families and every one of them is a face the host already has, so it names no chosen face: %s", d.prop, len(families), d.value)})
	}
	return out
}

func normalizeFamily(f string) string {
	f = strings.TrimSpace(f)
	f = strings.Trim(f, `"'`)
	return strings.ToLower(normalizeSpace(f))
}

// reGradient matches every gradient function, EVERY time it appears in a value: a
// declaration that draws two gradients has to be decided twice, and one written as
// `repeating-linear-gradient` must be found once rather than once per spelling that is a
// substring of it.
var reGradient = regexp.MustCompile(`(?:repeating-)?(?:linear|radial|conic)-gradient\(`)

func detectPurpleToBlue(s *source, cat map[string]string) []hit {
	var out []hit
	for _, d := range s.decls {
		low := strings.ToLower(d.value)
		for _, m := range reGradient.FindAllStringIndex(low, -1) {
			args := low[m[1]-1:]
			args = args[:skipParens(args, 0, len(args))]
			var purple, blue []string
			for _, tok := range colourTokens(args) {
				h, sat, l, ok := colourOf(resolveVar(tok, cat, 0))
				if !ok {
					continue
				}
				switch classify(h, sat, l) {
				case huePurple:
					purple = append(purple, tok)
				case hueBlue:
					blue = append(blue, tok)
				}
			}
			if len(purple) == 0 || len(blue) == 0 {
				continue
			}
			out = append(out, hit{entry: "purple-to-blue-gradient", file: d.file, line: d.line,
				detail: fmt.Sprintf("`%s` draws a gradient with a purple stop (%s) and a blue stop (%s): %s",
					d.prop, strings.Join(purple, " "), strings.Join(blue, " "), d.value)})
		}
	}
	return out
}

// colourTokens splits a gradient's arguments into the things that might be colours.
func colourTokens(args string) []string {
	var out []string
	for _, part := range splitTopLevel(strings.Trim(strings.TrimSpace(args), "()"), ',') {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		// A stop is "<colour> <position>", and a functional colour carries its own
		// spaces, so split on a space only outside parentheses.
		for _, word := range splitTopLevel(part, ' ') {
			if word = strings.TrimSpace(word); word != "" {
				out = append(out, word)
			}
		}
	}
	return out
}

// resolveVar follows `var(--x)` into the declared tokens, because a gradient's stops are
// normally token references rather than literals in a repository that has a token file.
func resolveVar(tok string, cat map[string]string, depth int) string {
	if depth > 4 || !strings.HasPrefix(strings.ToLower(tok), "var(") || !strings.HasSuffix(tok, ")") {
		return tok
	}
	inner := tok[len("var(") : len(tok)-1]
	name := strings.TrimSpace(splitTopLevel(inner, ',')[0])
	v, ok := cat[name]
	if !ok {
		return tok
	}
	return resolveVar(strings.TrimSpace(v), cat, depth+1)
}

func detectGlassmorphism(s *source, _ map[string]string) []hit {
	var out []hit
	for _, d := range s.decls {
		prop := strings.ToLower(d.prop)
		if prop != "backdrop-filter" && prop != "-webkit-backdrop-filter" {
			continue
		}
		if !strings.Contains(strings.ToLower(d.value), "blur(") {
			continue
		}
		out = append(out, hit{entry: "glassmorphism", file: d.file, line: d.line,
			detail: fmt.Sprintf("`%s: %s` blurs whatever is behind the surface, which is what glassmorphism IS", d.prop, d.value)})
	}
	// The same effect, spelled as the framework class that produces it.
	out = append(out, matches(s, "glassmorphism", []string{"backdrop", "blur"}, hyphenSeparator,
		"carries the identifier backdrop-blur")...)
	return out
}

// splitTopLevel splits on a separator that is not inside parentheses or a string.
func splitTopLevel(s string, sep byte) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); {
		switch s[i] {
		case '"', '\'':
			i = skipQuoted(s, i, len(s))
			continue
		case '(':
			i = skipParens(s, i, len(s))
			continue
		case sep:
			out = append(out, s[start:i])
			i++
			start = i
			continue
		}
		i++
	}
	if start <= len(s) {
		out = append(out, s[start:])
	}
	return out
}
