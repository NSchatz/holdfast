package config

// The per-root filesystem watch: an OPT-IN accelerator for DISCOVERY, written inside the
// `library_roots` entry it is about and nowhere else.
//
// Two keys, `watch` and `watch_settle_sec`, and they live where `rules` lives: INSIDE one
// entry. There is deliberately no top-level spelling and no HOLDFAST_* override, which is
// the difference between these and the path filters beside them. A top-level `watch: true`
// would be inherited by every entry that stayed silent, and an entry that says nothing
// about the watch must start no watch at all - a library whose files are still arriving
// over a network mount is the case, and an operator who turned the accelerator on for one
// root must not find it running over the other four. Written at the top level, in the file
// or in the environment, either key is a refusal naming where it goes.
//
// NEITHER KEY IS A PROFILE KNOB, and that is load-bearing rather than tidy. profileKnobs is
// one list with two readers - the acceptance check that refuses any other key inside an
// entry, and Profile.Digest, which is what a terminal row records its configuration by - so
// a watch key added to it would re-attribute every row in the ledger the day an operator
// turned a discovery accelerator on. `path` is the precedent and the filter keys are the
// second: a key an entry may CARRY is not the same set as a knob a profile DIGESTS. A watch
// decides HOW a file is discovered, never what is done to it once it is.

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The two keys, spelled once so the acceptance check, the parser, the refusals and the
// shipped example cannot disagree about them.
const (
	watchKey       = "watch"
	watchSettleKey = "watch_settle_sec"
)

// DefaultWatchSettleSec is how long a watched file's size must hold still before the watch
// offers it for processing. A download client writing a 40 GB remux is the case the delay
// exists for: a probe taken mid-write reads a duration and a packet count that are not the
// file's, and every gate downstream is weighed against those numbers.
const DefaultWatchSettleSec = 60

// watchKeys is THE enumeration of the watch keys a library_roots entry may carry, in the
// order they are validated and reported. It is CLOSED, and it is deliberately not part of
// profileKnobs for the reason at the top of this file.
var watchKeys = []string{watchKey, watchSettleKey}

// WatchKeys returns the closed set of watch keys an entry may carry. It returns a copy: the
// set is closed, and a caller must not be able to open it.
func WatchKeys() []string { return append([]string(nil), watchKeys...) }

// isWatchKey reports whether key names one of the watch keys.
func isWatchKey(key string) bool {
	for _, k := range watchKeys {
		if k == key {
			return true
		}
	}
	return false
}

// Watch is one library root's RESOLVED watch state: whether this root is watched at all,
// and how long a file's size must hold still before the watch offers it.
//
// The zero value is the state every configuration written before this feature resolves to,
// and the state every entry that stays silent resolves to: no watch.
type Watch struct {
	// Enabled reports whether this root opted in.
	Enabled bool
	// SettleSec is the settle period in seconds. It is DefaultWatchSettleSec unless the
	// entry named another, and it is meaningful only where Enabled is true.
	SettleSec int
}

// Settle is the settle period as a duration.
func (w Watch) Settle() time.Duration { return time.Duration(w.SettleSec) * time.Second }

// parseWatch resolves the watch keys one entry carried. where is the entry as the refusals
// name it, already carrying the index and the path.
//
// A settle period written beside a watch that is not on is a REFUSAL rather than a value
// nothing reads: a key an operator may write and nothing consults is how somebody comes to
// believe a root is watched, on the one setting that stands between a probe and a file that
// is still being written.
func parseWatch(where string, raw map[string]any) (Watch, error) {
	w := Watch{SettleSec: DefaultWatchSettleSec}
	if v, ok := raw[watchKey]; ok {
		on, err := watchBool(where, watchKey, v)
		if err != nil {
			return Watch{}, err
		}
		w.Enabled = on
	}
	if v, ok := raw[watchSettleKey]; ok {
		if !w.Enabled {
			return Watch{}, fmt.Errorf("%s sets %q without %s: the settle period only decides when a WATCHED "+
				"root's files are offered, so this value would be read by nothing. Write `%s: true` beside it, "+
				"or remove it", where, watchSettleKey, watchKey+": true", watchKey)
		}
		secs, err := watchSeconds(where, watchSettleKey, v)
		if err != nil {
			return Watch{}, err
		}
		w.SettleSec = secs
	}
	if !w.Enabled {
		return Watch{}, nil
	}
	return w, nil
}

// watchBool reads the opt-in. It accepts a YAML boolean and the two words a boolean is
// spelled with, and refuses everything else BY NAME rather than reading it weakly: `watch:
// maybe` resolving to false is a root an operator believes is watched and is not.
func watchBool(where, key string, raw any) (bool, error) {
	switch v := raw.(type) {
	case bool:
		return v, nil
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "true":
			return true, nil
		case "false":
			return false, nil
		}
	}
	return false, fmt.Errorf("%s has a non-boolean %q (%#v): the watch is either on for this root (true) "+
		"or absent, which is off", where, key, raw)
}

// watchSeconds reads the settle period. It is a WHOLE NUMBER OF SECONDS: a fraction, a
// word, a boolean or a list is a refusal naming the key and the value, on the same
// discipline history_retention_rows and bitrate_kbps keep.
func watchSeconds(where, key string, raw any) (int, error) {
	if !isWholeNumber(raw) {
		return 0, fmt.Errorf("%s has a %q that is not a whole number of seconds (%#v): the settle period is "+
			"how long a file's size must hold still before the watch offers it", where, key, raw)
	}
	secs, err := wholeSeconds(raw)
	if err != nil {
		return 0, fmt.Errorf("%s has a %q that is not a whole number of seconds (%#v): %w", where, key, raw, err)
	}
	if secs < 0 {
		return 0, fmt.Errorf("%s has a negative %q (%d): a settle period is a wait, and 0 offers a file the "+
			"moment it is seen - which on a file a download client is still writing is the probe this delay "+
			"exists to prevent", where, key, secs)
	}
	return secs, nil
}

// wholeSeconds converts a value isWholeNumber has already accepted into an int.
func wholeSeconds(raw any) (int, error) {
	switch v := raw.(type) {
	case int:
		return v, nil
	case int32:
		return int(v), nil
	case int64:
		return int(v), nil
	case uint:
		return int(v), nil
	case uint32:
		return int(v), nil
	case uint64:
		return int(v), nil
	case float32:
		return int(v), nil
	case float64:
		return int(v), nil
	case string:
		return strconv.Atoi(strings.TrimSpace(v))
	}
	return 0, fmt.Errorf("%#v is not a number", raw)
}

// misplacedWatchError is the refusal for a watch key written where no watch state lives:
// the file's top level, or the environment.
//
// It is a refusal and not a warning because the alternative is silence. Neither key is a
// top-level key, so a build that merely ignored one would start, scan, and watch nothing at
// all while the operator's file plainly says a watch is on.
func misplacedWatchError(key, where string) error {
	return fmt.Errorf("%q is written INSIDE a library_roots entry, not at the top level of %s: the watch is "+
		"per library root, and a root that says nothing about it starts no watch - so there is nothing at the "+
		"top level for one to inherit from. Write it as:\n"+
		"  library_roots:\n"+
		"    - path: /media/tv\n"+
		"      %s: true\n"+
		"      %s: %d",
		key, where, watchKey, watchSettleKey, DefaultWatchSettleSec)
}
