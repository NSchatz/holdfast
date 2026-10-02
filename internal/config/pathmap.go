package config

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// PathMapEntry is one prefix pair of a path map: From is a directory as holdfast sees it,
// To is the same directory as the target service sees it. Both are absolute, slash-separated
// paths.
type PathMapEntry struct {
	From string `yaml:"from"`
	To   string `yaml:"to"`
}

// PathMap translates a path between holdfast's view of the library and a target service's
// view of it (`radarr_path_map`, `sonarr_path_map`, `plex_path_map`). holdfast and the
// service usually run in different containers, so one directory has two names.
//
// It is the ONE place both directions live, and both follow one rule:
//
//   - Map takes a path as holdfast sees it and answers the path the service sees: the
//     entry whose From is the LONGEST prefix of the path wins.
//   - Reverse takes a path as the service sees it and answers the path holdfast sees: the
//     entry whose To is the longest prefix of the path wins.
//   - A prefix matches on WHOLE path components only: `/movies/Film` is a prefix of
//     `/movies/Film/a.mkv` and of itself, and never of `/movies/Film 2/a.mkv`.
//   - A path no entry matches passes through UNCHANGED, cleaned. An empty map therefore
//     changes nothing, which is right wherever both sides mount the library at one path.
//
// A map Validate accepted has no two entries with the same From and no two with the same To,
// so neither direction can meet a tie. A map assembled by hand that carries one resolves it
// to the entry written first.
type PathMap []PathMapEntry

// Map translates a path from holdfast's view to the target service's view.
func (m PathMap) Map(holdfastPath string) string {
	out, _ := m.translate(holdfastPath, func(e PathMapEntry) (string, string) { return e.From, e.To })
	return out
}

// Reverse translates a path from the target service's view back to holdfast's view. It is
// the inverse of Map for every path under a mapped prefix.
func (m PathMap) Reverse(targetPath string) string {
	out, _ := m.ReverseMatched(targetPath)
	return out
}

// ReverseMatched is Reverse, and also reports whether an entry's To covered the path. A
// caller that must not take an uncovered path for one of holdfast's own asks it; Reverse
// itself passes such a path through unchanged.
func (m PathMap) ReverseMatched(targetPath string) (holdfastPath string, matched bool) {
	return m.translate(targetPath, func(e PathMapEntry) (string, string) { return e.To, e.From })
}

// translate rewrites the longest matching prefix of p, and reports whether one matched.
// sides picks which half of an entry is matched and which replaces it.
func (m PathMap) translate(p string, sides func(PathMapEntry) (match, replace string)) (string, bool) {
	p = path.Clean(p)
	best, bestLen := -1, -1
	for i, e := range m {
		match, _ := sides(e)
		match = path.Clean(match)
		if _, ok := UnderPrefix(p, match); ok && len(match) > bestLen {
			best, bestLen = i, len(match)
		}
	}
	if best < 0 {
		return p, false
	}
	match, replace := sides(m[best])
	rest, _ := UnderPrefix(p, path.Clean(match))
	return path.Join(path.Clean(replace), rest), true
}

// UnderPrefix reports whether the cleaned path p is prefix itself or lies beneath it, on a
// whole-component boundary, and returns what follows the prefix ("" for the prefix itself).
// It is the one component-boundary test the path maps and the media-server clients' owner
// lookups share, so `/movies/Film` owns `/movies/Film/x` everywhere and `/movies/Film 2`
// nowhere.
func UnderPrefix(p, prefix string) (rest string, ok bool) {
	if p == prefix {
		return "", true
	}
	if prefix == "/" {
		if strings.HasPrefix(p, "/") {
			return p[1:], true
		}
		return "", false
	}
	if strings.HasPrefix(p, prefix+"/") {
		return p[len(prefix)+1:], true
	}
	return "", false
}

// parsePathMap reads one path-map key as the file carried it: a list of mappings, each with
// exactly the keys `from` and `to`, each a string. Anything else refuses naming the key and
// the entry, because a pair silently dropped is a rescan sent to a directory the target
// does not have. raw nil (the key absent, or written with no value) is the empty map.
func parsePathMap(raw any, key, where string) (PathMap, error) {
	if raw == nil {
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("%s in %s must be a list of {from: <path>, to: <path>} pairs: %#v is not one",
			key, where, raw)
	}
	out := make(PathMap, 0, len(list))
	for i, item := range list {
		entry, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s[%d] in %s must be a mapping with the keys from and to: %#v is not one",
				key, i, where, item)
		}
		names := make([]string, 0, len(entry))
		for name := range entry {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if name != "from" && name != "to" {
				return nil, fmt.Errorf("%s[%d] in %s has unknown key %q (typo?): an entry carries from and to",
					key, i, where, name)
			}
		}
		from, fromOK := entry["from"].(string)
		to, toOK := entry["to"].(string)
		if !fromOK || !toOK {
			return nil, fmt.Errorf("%s[%d] in %s must carry both from and to, each a path written as a string",
				key, i, where)
		}
		out = append(out, PathMapEntry{From: from, To: to})
	}
	return out, nil
}

// validate refuses a map with a side that is not an absolute path, and one in which two
// entries share a From or share a To: either direction would then have two answers for one
// path, and a guess here is a rescan of the wrong directory or a webhook resolved to the
// wrong file.
func (m PathMap) validate(key string) error {
	froms, tos := map[string]int{}, map[string]int{}
	for i, e := range m {
		for _, side := range []struct{ name, value string }{{"from", e.From}, {"to", e.To}} {
			if !strings.HasPrefix(side.value, "/") {
				return fmt.Errorf("%s[%d].%s %q must be an absolute path (it starts with /)",
					key, i, side.name, side.value)
			}
		}
		from, to := path.Clean(e.From), path.Clean(e.To)
		if j, dup := froms[from]; dup {
			return fmt.Errorf("%s[%d].from %q repeats %s[%d].from: one path would map two ways", key, i, e.From, key, j)
		}
		if j, dup := tos[to]; dup {
			return fmt.Errorf("%s[%d].to %q repeats %s[%d].to: one path would map back two ways", key, i, e.To, key, j)
		}
		froms[from], tos[to] = i, i
	}
	return nil
}
