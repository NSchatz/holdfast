package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"strings"
)

// The design record's grammar, which is small on purpose: a check that has to guess what
// a document meant is a check whose verdict is an opinion.
//
//	## Identity
//	### <one of the five identity values>
//	token: `--some-token`
//	value: `the value that token is declared with`
//	why:   one sentence, which may wrap
//
//	## Blocklist exceptions
//	### <a blocklist entry id>
//	why:   one sentence, which may wrap
//
// Anything outside a `###` block is prose for a human and is not read. Inside one, a line
// that opens with a short `key:` IS a key: an unrecognised one is a parse failure rather
// than prose, because a mistyped `tokn:` that read as prose would silently delete the one
// assertion holding the record to the surface.

const (
	identityHeading  = "## Identity"
	exceptionHeading = "## Blocklist exceptions"
)

// identityNames are C1's five, in the order the check reports them.
var identityNames = []string{
	"display face",
	"text face",
	"accent",
	"radius signature",
	"shadow signature",
}

// identityValue is one declared value of the five.
type identityValue struct {
	name  string
	line  int
	token string
	value string
	why   string
}

// exception is one blocklist entry the record buys with a reason.
type exception struct {
	id   string
	line int
	why  string
}

type record struct {
	path       string
	values     map[string]identityValue
	exceptions map[string]exception
	order      []string // exception ids, in the order the record names them
}

var (
	reKeyLine = regexp.MustCompile(`^([a-z][a-z-]{0,19}):(\s+(.*))?$`)
	reBackt   = regexp.MustCompile("^`(.*)`$")
)

// loadRecord reads the record and says which of absent, unreadable and unparseable it
// was. Each is a different fault with a different fix, and none of them is "empty": a
// record that could not be read must never be read as one that excepts nothing and
// declares nothing, because that record would pass every check below over no evidence.
func loadRecord(path string) (*record, error) {
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("the design record %s IS ABSENT. interface-craft C1 asks this repository to declare its display face, text face, accent, radius signature and shadow signature with one sentence each, and nothing here can be decided without it", path)
	case errors.Is(err, fs.ErrPermission):
		return nil, fmt.Errorf("the design record %s CANNOT BE READ (%v). A record this check cannot read is not a record it may treat as empty", path, err)
	case err != nil:
		return nil, fmt.Errorf("the design record %s CANNOT BE READ (%v)", path, err)
	}
	return parseRecord(path, string(raw))
}

func parseRecord(path, text string) (*record, error) {
	r := &record{path: path, values: map[string]identityValue{}, exceptions: map[string]exception{}}
	section := ""
	block := ""
	blockLine := 0
	keys := map[string]string{}
	keyOrder := []string{}
	lastKey := ""

	flush := func() error {
		if block == "" {
			return nil
		}
		defer func() { block, keys, keyOrder, lastKey = "", map[string]string{}, nil, "" }()
		switch section {
		case identityHeading:
			if _, dup := r.values[block]; dup {
				return fmt.Errorf("the design record %s DOES NOT PARSE: it declares the identity value %q twice, so which of them the check should hold the surface to is undecidable", path, block)
			}
			for _, k := range keyOrder {
				if k != "token" && k != "value" && k != "why" {
					return fmt.Errorf("the design record %s DOES NOT PARSE: the identity value %q at line %d carries the key %q, which is not one of token, value, why", path, block, blockLine, k)
				}
			}
			r.values[block] = identityValue{name: block, line: blockLine, token: unbacktick(keys["token"]), value: unbacktick(keys["value"]), why: keys["why"]}
		case exceptionHeading:
			if _, dup := r.exceptions[block]; dup {
				return fmt.Errorf("the design record %s DOES NOT PARSE: it names the blocklist exception %q twice", path, block)
			}
			for _, k := range keyOrder {
				if k != "why" {
					return fmt.Errorf("the design record %s DOES NOT PARSE: the exception %q at line %d carries the key %q, which is not `why`", path, block, blockLine, k)
				}
			}
			r.exceptions[block] = exception{id: block, line: blockLine, why: keys["why"]}
			r.order = append(r.order, block)
		}
		return nil
	}

	for n, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimRight(line, " \t\r")
		switch {
		case strings.HasPrefix(trimmed, "## "):
			if err := flush(); err != nil {
				return nil, err
			}
			section = strings.TrimSpace(trimmed)
			continue
		case strings.HasPrefix(trimmed, "### "):
			if err := flush(); err != nil {
				return nil, err
			}
			block, blockLine = strings.TrimSpace(strings.TrimPrefix(trimmed, "###")), n+1
			continue
		}
		if block == "" || section == "" {
			continue // prose for a human, outside every block
		}
		if strings.TrimSpace(trimmed) == "" {
			lastKey = ""
			continue
		}
		if m := reKeyLine.FindStringSubmatch(strings.TrimSpace(trimmed)); m != nil {
			key := m[1]
			if _, dup := keys[key]; dup {
				return nil, fmt.Errorf("the design record %s DOES NOT PARSE: %q at line %d carries the key %q twice", path, block, blockLine, key)
			}
			keys[key] = strings.TrimSpace(m[3])
			keyOrder = append(keyOrder, key)
			lastKey = key
			continue
		}
		if lastKey == "" {
			continue // prose inside a block that belongs to no key
		}
		keys[lastKey] = strings.TrimSpace(keys[lastKey] + " " + strings.TrimSpace(trimmed))
	}
	if err := flush(); err != nil {
		return nil, err
	}

	if !strings.Contains(text, identityHeading) {
		return nil, fmt.Errorf("the design record %s DOES NOT PARSE: it carries no %q section, so it declares none of the five identity values interface-craft C1 requires", path, identityHeading)
	}
	if !strings.Contains(text, exceptionHeading) {
		return nil, fmt.Errorf("the design record %s DOES NOT PARSE: it carries no %q section. A record with no exceptions section is not a record with no exceptions - it is one this check cannot tell apart from a truncated file, and C2's exception route would be silently unavailable", path, exceptionHeading)
	}
	return r, nil
}

func unbacktick(s string) string {
	s = strings.TrimSpace(s)
	if m := reBackt.FindStringSubmatch(s); m != nil {
		return strings.TrimSpace(m[1])
	}
	return s
}

// isReason decides whether a `why:` is a reason sentence at all. "One sentence" is read
// as AT LEAST one: refusing a two-sentence reason would grade prose length rather than
// whether a reason was given, and the failure this clause exists to stop is the empty
// reason, not the long one.
func isReason(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) < 20 {
		return false
	}
	if !strings.HasSuffix(s, ".") && !strings.HasSuffix(s, "!") && !strings.HasSuffix(s, "?") {
		return false
	}
	return len(strings.Fields(s)) >= 4
}
