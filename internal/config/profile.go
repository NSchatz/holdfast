package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/go-viper/mapstructure/v2"
	"github.com/knadh/koanf/v2"

	"github.com/NSchatz/holdfast/internal/encoder"
)

// Per-library profiles: one resolved set of engine knobs per library root.
//
// A `library_roots` entry is EITHER a plain string (the whole list was that until this
// existed, and still may be) or a mapping carrying `path` plus any subset of the closed
// knob set below. The string form means "this root, all defaults", so every configuration
// written against the flat list resolves to exactly what it always did.
//
// Resolution is THREE layers, innermost last: built-in default <- top-level (the YAML
// file, then HOLDFAST_*) <- the entry's own profile. It keeps the same zero-vs-absent
// discipline koanf's defaults layer gives at the top level: a key PRESENT in a profile
// overrides whatever its value, so `crf: 0` means 0, and an ABSENT key inherits.
//
// There is deliberately no new top-level key and no environment spelling that addresses
// one root: the profile lives inside the entry, so `knownKeys` needs no new member and
// `HOLDFAST_CRF` keeps exactly the power over the file's top level that it had.

// Layer names which of the three layers supplied a resolved knob value. It is recorded
// per knob per root because `holdfast validate` must print the configuration the
// inheritance PRODUCED rather than the file as written, and a value with no layer beside
// it leaves an operator unable to tell an inherited default from a choice they made.
type Layer string

// The three layers, in the order they override one another. Each is spelled to read as
// the tail of "... came from <layer>", because that is the only sentence it is ever put
// in and a token an operator has to translate is a token they will misread.
const (
	LayerDefault  Layer = "the built-in default"
	LayerTopLevel Layer = "the top-level configuration"
	LayerProfile  Layer = "this root's own profile"
)

// profileKnobs is THE enumeration of the knobs a library_roots entry may override, and
// it is the only one. isProfileKnob (the acceptance check that refuses anything else
// inside an entry) and resolveRoots (which seeds each root's profile from the top-level
// value of every knob named here) both read it, so the set an entry is allowed to carry
// and the set the resolver actually resolves cannot drift apart - and a knob added to
// one without the other would be a knob an operator may write that nothing reads.
//
// It is CLOSED. Everything else is either a typo or a key that describes the PROCESS
// rather than a library, and both are startup refusals: silently ignoring `workers: 4`
// inside an entry is how an operator comes to believe a root has a worker count it
// does not.
//
// `exclude_paths` and `include_paths` are deliberately absent. Neither is a top-level
// key of this build, so there is no global value for a profile to override; path
// filtering is its own feature, not a profile of one that does not exist.
var profileKnobs = []string{
	"encoder", "crf", "preset", "pixel_format", "container_ext",
	"min_bitrate_kbps", "min_savings_percent", "skip_hardlinked",
	"vmaf_enable", "min_vmaf", "vmaf_min_pool", "vmaf_min_chroma",
	"vmaf_subsample", "vmaf_model",
}

// ProfileKnobs returns the closed set of knobs a library_roots entry may override, in
// the order a resolved profile is printed and digested. It returns a copy: the set is
// closed, and a caller must not be able to open it.
func ProfileKnobs() []string { return append([]string(nil), profileKnobs...) }

// isProfileKnob reports whether key names a knob an entry may override.
func isProfileKnob(key string) bool {
	for _, k := range profileKnobs {
		if k == key {
			return true
		}
	}
	return false
}

// rootPathKey is the one key inside an entry that is not a knob: which tree the profile
// is about. Spelled once so the acceptance check, the parser and the refusal messages
// cannot disagree about it.
const rootPathKey = "path"

// Profile is one library root's RESOLVED engine knobs - the values that decide what
// happens to a file under that root. Every field is a knob in profileKnobs and carries
// the same `yaml` tag as the top-level key it inherits from, which is what lets the
// resolver decode a profile out of the same map shape koanf hands the top level.
//
// The two pointer fields keep the meaning they have on Config: nil is the built-in
// default rather than a false the operator never wrote. After Load they are never nil
// (the defaults layer supplies both), so the accessors exist for a Profile assembled by
// hand.
type Profile struct {
	Encoder           string  `yaml:"encoder"`
	CRF               int     `yaml:"crf"`
	Preset            string  `yaml:"preset"`
	PixelFormat       string  `yaml:"pixel_format"`
	ContainerExt      string  `yaml:"container_ext"`
	MinBitrateKbps    int     `yaml:"min_bitrate_kbps"`
	MinSavingsPercent int     `yaml:"min_savings_percent"`
	SkipHardlinked    *bool   `yaml:"skip_hardlinked"`
	VmafEnable        *bool   `yaml:"vmaf_enable"`
	MinVmaf           float64 `yaml:"min_vmaf"`
	VmafMinPool       float64 `yaml:"vmaf_min_pool"`
	VmafMinChroma     float64 `yaml:"vmaf_min_chroma"`
	VmafSubsample     int     `yaml:"vmaf_subsample"`
	VmafModel         string  `yaml:"vmaf_model"`
}

// VmafGate reports whether the VMAF gate is enabled for this root, defaulting to true
// when unset.
func (p Profile) VmafGate() bool { return vmafGate(p.VmafEnable) }

// HardlinkSkip reports whether hard-linked sources are skipped under this root,
// defaulting to true when unset.
func (p Profile) HardlinkSkip() bool { return hardlinkSkip(p.SkipHardlinked) }

// ContainerMatchesSource reports whether this root's ContainerExt is the "match the
// source" sentinel rather than a forced extension.
func (p Profile) ContainerMatchesSource() bool { return containerMatchesSource(p.ContainerExt) }

// PixelFormatAuto reports whether this root's PixelFormat is the "derive per source"
// sentinel rather than a forced pixel format.
func (p Profile) PixelFormatAuto() bool { return pixelFormatAuto(p.PixelFormat) }

// values renders the resolved knobs in profileKnobs order, as the canonical text the
// digest is taken over and as the values `validate` prints. Both readings come from
// here so the digest and the printed configuration can never describe different
// profiles.
//
// The two tri-state pointers are rendered RESOLVED (through the accessors above), never
// as "nil" or "set": two profiles that behave identically must digest identically, and
// an absent skip_hardlinked and an explicit `true` behave identically.
//
// A float is formatted with 'g' and -1 precision, which is the shortest text that
// round-trips back to the same float64 - so 95 and 95.0 are one value here, as they are
// to the gate.
func (p Profile) values() []string {
	f := func(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }
	return []string{
		p.Encoder,
		strconv.Itoa(p.CRF),
		p.Preset,
		p.PixelFormat,
		p.ContainerExt,
		strconv.Itoa(p.MinBitrateKbps),
		strconv.Itoa(p.MinSavingsPercent),
		strconv.FormatBool(p.HardlinkSkip()),
		strconv.FormatBool(p.VmafGate()),
		f(p.MinVmaf),
		f(p.VmafMinPool),
		f(p.VmafMinChroma),
		strconv.Itoa(p.VmafSubsample),
		p.VmafModel,
	}
}

// digestLength is how many hex characters of the SHA-256 a profile digest carries. It is
// a LEDGER field, printed beside a path on every terminal row and repeated in every
// exported line, so the full 64 characters would cost far more than they buy: 16 hex
// characters is 64 bits, and the set being distinguished is the handful of profiles one
// operator wrote in one YAML file.
const digestLength = 16

// Digest is a stable identifier for this profile's RESOLVED overridable values.
//
// It is on the ledger row beside the deciding root because the root path alone stops
// being interpretable the moment the profile is edited: a terminal row would go on
// naming /mnt/tv while /mnt/tv now means something else, and the record of what decided
// that file would be gone. Two rows decided under identical resolved values carry the
// same digest whatever the file said, and a row decided under any different value
// carries a different one.
//
// The knob NAME is hashed beside its value, and the values are separated rather than
// concatenated, so no two different profiles can produce one text: without that,
// preset "slow" + model "auto" and preset "slowauto" + model "" would be the same bytes.
func (p Profile) Digest() string {
	var b strings.Builder
	for i, knob := range profileKnobs {
		b.WriteString(knob)
		b.WriteByte('=')
		b.WriteString(p.values()[i])
		b.WriteByte('\n')
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])[:digestLength]
}

// validate runs every per-value refusal that belongs to a knob a profile may carry. It
// is the SAME function the top level is checked with, which is what makes "a profile
// value the top level would refuse is refused" true by construction rather than by two
// copies of the same arithmetic agreeing.
//
// The messages name the key and the offending value and nothing else; the caller adds
// the root when it is checking a root's profile, so the top level's refusal reads
// exactly as it always has.
func (p Profile) validate() error {
	if p.Encoder != "" {
		if _, ok := encoder.Lookup(p.Encoder); !ok {
			return fmt.Errorf("encoder %q is not supported (known: %v)", p.Encoder, encoder.Known())
		}
	}
	if p.CRF < 0 || p.CRF > 51 {
		return fmt.Errorf("crf %d out of range (0-51)", p.CRF)
	}
	if p.MinSavingsPercent < 0 || p.MinSavingsPercent >= 100 {
		return fmt.Errorf("min_savings_percent %d out of range (0-99)", p.MinSavingsPercent)
	}
	if strings.ContainsAny(p.ContainerExt, "./\\") {
		return fmt.Errorf("container_ext %q must be a bare extension (no dot or slash)", p.ContainerExt)
	}
	if p.MinVmaf < 0 || p.MinVmaf > 100 {
		return fmt.Errorf("min_vmaf %g out of range (0-100)", p.MinVmaf)
	}
	if p.VmafMinPool < 0 || p.VmafMinPool > 100 {
		return fmt.Errorf("vmaf_min_pool %g out of range (0-100)", p.VmafMinPool)
	}
	// The chroma floor bounds a PSNR in dB. 0 is the "disabled" sentinel (warned about,
	// not refused); a negative floor could never reject and would be a silent no-op, and
	// a value above 100 dB could never be cleared and would reject every encode there is.
	// Both are configuration the operator did not mean, so both are refused BY NAME
	// rather than clamped into something plausible.
	if p.VmafMinChroma < 0 || p.VmafMinChroma > 100 {
		return fmt.Errorf("vmaf_min_chroma %g out of range (0-100 dB; 0 disables the chroma floor)", p.VmafMinChroma)
	}
	if p.VmafSubsample < 0 {
		// 0 means "use the default" (Load's koanf layer sets 1; the VMAF scorer also
		// floors <1 to 1) - consistent with the other zero-defaulted knobs. Only a
		// negative interval is invalid.
		return fmt.Errorf("vmaf_subsample %d must be >= 0", p.VmafSubsample)
	}
	// Fail-safe: an explicitly-enabled VMAF gate with no effective threshold (every
	// floor 0) is enabled-but-never-rejecting - a silent no-op on a delete-capable tool.
	// Refuse it. (Checked only when vmaf_enable is EXPLICIT: a nil pointer is the
	// default-on state, and Load always resolves it to true with min_vmaf=95, so a real
	// config never trips this by omission.) The chroma floor counts here: a gate that
	// rejects on chroma alone is a strange configuration but it is not a no-op, and
	// refusing it would be refusing a gate that does gate.
	if p.VmafEnable != nil && *p.VmafEnable && p.MinVmaf == 0 && p.VmafMinPool == 0 && p.VmafMinChroma == 0 {
		return errVmafGateNeverRejects
	}
	return nil
}

// warnings reports the configurations of THIS profile that are valid but weaken a
// safety gate. root is the cleaned root the profile decides, named in every warning so
// an operator running one process over three libraries can tell WHICH of them lost its
// floor; "" names none, which is what a configuration with no roots at all gets.
//
// See Config.Warnings for why a warning is always a weakened gate and never a default.
func (p Profile) warnings(root string) []string {
	where := ""
	if root != "" {
		where = "library root " + root + ": "
	}
	// The gate off entirely is the operator's call - but it is also the WEAKEST
	// configuration this tool has, strictly weaker than "gate on, floor off" (which
	// warns below). Staying silent about the dangerous one while nagging about the safer
	// one would be exactly backwards.
	if !p.VmafGate() {
		return []string{where + "vmaf_enable is false — there is NO perceptual gate. The structural checks " +
			"(codec, duration/packet parity, size, stream counts, decode-integrity) all pass on an " +
			"encode that decodes perfectly and looks terrible, and the source is then deleted. This is " +
			"the weakest setting available; prefer lowering min_vmaf/vmaf_min_pool over disabling the gate."}
	}
	var w []string
	if p.VmafMinPool <= 0 {
		w = append(w, where+"vmaf_min_pool is 0 — the worst-frame floor is DISABLED, leaving the pooled "+
			"harmonic mean as the only VMAF gate. A mean hides local damage: ~1% of frames can collapse "+
			"to VMAF ~35 while the mean still clears min_vmaf, and the source is then deleted. "+
			"If honest encodes are being rejected, LOWER the floor (e.g. 45) rather than setting it to 0 — "+
			"a lower floor still bounds local damage; 0 bounds nothing.")
	}
	if p.VmafMinChroma <= 0 {
		w = append(w, where+"vmaf_min_chroma is 0 - the chroma floor is DISABLED, so CHROMA DAMAGE IS "+
			"UNGUARDED. The VMAF model is luma-only and every structural check passes an output whose "+
			"colour planes have been flattened, shifted or desaturated: it decodes perfectly, carries "+
			"the right duration, packets and streams, and scores ~99 on VMAF. The source is then deleted. "+
			"If honest encodes are being rejected, LOWER the floor (e.g. 25) rather than setting it to 0 - "+
			"a lower floor still bounds chroma damage; 0 bounds nothing.")
	}
	if p.VmafSubsample > 1 {
		w = append(w, fmt.Sprintf("%svmaf_subsample is %d — VMAF measures only every %dth frame, so the "+
			"vmaf_min_pool and vmaf_min_chroma worst-frame floors are a SAMPLE, not a guarantee: a "+
			"damaged frame that is never sampled is never seen. Use vmaf_subsample: 1 on content you "+
			"cannot re-acquire.",
			where, p.VmafSubsample, p.VmafSubsample))
	}
	return w
}

// Root is ONE library root together with the profile that decides every file under it.
//
// Path is the root AS CONFIGURED, because that is the spelling every other surface is
// keyed on - the startup check's declarations, the scan's walk, the allow_non_local
// entries an operator writes. Clean is filepath.Clean of it, which is what the nesting
// refusal compares and what a terminal row records: a ledger that recorded the raw
// spelling would attribute two rows to two roots that are the same tree.
type Root struct {
	Path    string
	Clean   string
	Profile Profile

	// Layers says which of the three layers supplied each knob's resolved value, keyed
	// by the knob's config key. It is what `holdfast validate` prints beside each value,
	// so the printed configuration states not only what the inheritance produced but
	// where each piece of it came from.
	Layers map[string]Layer
}

// Contains reports whether p is this root or a path beneath it.
func (r Root) Contains(p string) bool { return underRoot(r.Clean, filepath.Clean(p)) }

// LayerOf reports which layer supplied knob's resolved value, defaulting to the
// top-level layer for a Root that carries no record of them (one derived from a Config
// assembled by hand rather than loaded from a file).
func (r Root) LayerOf(knob string) Layer {
	if l, ok := r.Layers[knob]; ok {
		return l
	}
	return LayerTopLevel
}

// EffectiveKnob is one resolved knob of a root's profile: the config key, the value the
// three layers produced, and which layer supplied it.
type EffectiveKnob struct {
	Knob  string
	Value string
	Layer Layer
}

// Effective is this root's profile as the inheritance PRODUCED it, in profileKnobs
// order, each value beside the layer it came from.
//
// It is what `holdfast validate` prints, and it reads the same values() the digest is
// taken over - so the configuration an operator is shown and the profile a row is
// attributed to can never describe different things.
func (r Root) Effective() []EffectiveKnob {
	vals := r.Profile.values()
	out := make([]EffectiveKnob, 0, len(profileKnobs))
	for i, knob := range profileKnobs {
		out = append(out, EffectiveKnob{Knob: knob, Value: vals[i], Layer: r.LayerOf(knob)})
	}
	return out
}

// underRoot reports whether child is root, or lies beneath it, comparing on PATH
// BOUNDARIES: /a/b is under /a and /ab is not. Both arguments must already be cleaned.
func underRoot(root, child string) bool {
	if child == root {
		return true
	}
	sep := string(filepath.Separator)
	if root == sep {
		return strings.HasPrefix(child, sep)
	}
	return strings.HasPrefix(child, root+sep)
}

// rootEntry is one library_roots entry as WRITTEN: the tree it names, and whatever
// subset of the overridable knobs it carried. override is nil for the string form and
// for a mapping that carried nothing but `path`, which is why those two resolve
// identically rather than merely similarly.
type rootEntry struct {
	path     string
	override map[string]any
}

// parseRootEntries turns the raw library_roots value into entries, refusing anything
// that is neither a tree nor a tree with a profile.
//
// Every refusal names the entry's INDEX, because that is the only thing an operator can
// use to find a malformed entry: an entry with no usable `path` has no other name, and
// one whose path is a number cannot be quoted back as a path. Where a path IS readable
// it is named too.
//
// A raw value that is not a list at all is treated as a single entry, which is what
// HOLDFAST_LIBRARY_ROOTS=/mnt/media has always meant.
func parseRootEntries(raw any, file string) ([]rootEntry, error) {
	if raw == nil {
		return nil, nil
	}
	items, ok := raw.([]any)
	if !ok {
		if ss, isStrings := raw.([]string); isStrings {
			items = make([]any, 0, len(ss))
			for _, s := range ss {
				items = append(items, s)
			}
		} else {
			items = []any{raw}
		}
	}
	entries := make([]rootEntry, 0, len(items))
	for i, item := range items {
		e, err := parseRootEntry(i, item, file)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// parseRootEntry parses ONE entry. See parseRootEntries for the refusal discipline.
func parseRootEntry(i int, item any, file string) (rootEntry, error) {
	switch v := item.(type) {
	case string:
		return checkedEntry(i, v, nil, file)
	case map[string]any:
		return parseRootMapping(i, v, file)
	case map[any]any:
		// Defensive: this build's YAML parser produces string-keyed maps, but a
		// non-string key must be a refusal naming the key rather than a panic or a
		// silently dropped knob.
		m := make(map[string]any, len(v))
		for key, val := range v {
			s, ok := key.(string)
			if !ok {
				return rootEntry{}, fmt.Errorf("library_roots[%d] in %s has a non-string key %#v: "+
					"an entry is either a path or a mapping of %s plus profile knobs", i, file, key, rootPathKey)
			}
			m[s] = val
		}
		return parseRootMapping(i, m, file)
	default:
		return rootEntry{}, fmt.Errorf("library_roots[%d] in %s is %#v: an entry must be either a path "+
			"(a string) or a mapping carrying %q plus any of the profile knobs (%s)",
			i, file, item, rootPathKey, strings.Join(profileKnobs, ", "))
	}
}

// parseRootMapping parses the mapping spelling of an entry: `path` plus any subset of
// the closed knob set. Everything else is a refusal, and the refusal for a DAEMON-level
// key says so in as many words - `workers` inside an entry is not a typo, it is a
// misunderstanding about what a profile is, and telling the operator it might be a typo
// would send them looking for a spelling mistake that is not there.
func parseRootMapping(i int, m map[string]any, file string) (rootEntry, error) {
	where := fmt.Sprintf("library_roots[%d] in %s", i, file)
	rawPath, ok := m[rootPathKey]
	if !ok {
		return rootEntry{}, fmt.Errorf("%s is a mapping with no %q: an entry that carries profile knobs "+
			"must say which tree they are about", where, rootPathKey)
	}
	p, ok := rawPath.(string)
	if !ok {
		return rootEntry{}, fmt.Errorf("%s has a non-string %q (%#v): a root is a path", where, rootPathKey, rawPath)
	}
	// Past here the entry HAS a usable path, so every refusal names it as well as the
	// index: an operator scanning a long library_roots list finds the root far faster by
	// its own name than by counting entries.
	if strings.TrimSpace(p) != "" {
		where += " (" + p + ")"
	}

	override := make(map[string]any, len(m)-1)
	for key, val := range m {
		if key == rootPathKey {
			continue
		}
		if !isProfileKnob(key) {
			if knownKeys[key] {
				return rootEntry{}, fmt.Errorf("%s sets %q, which is a DAEMON-level key: it describes the "+
					"process, not a library, so it may only be set at the top level. A library_roots entry "+
					"accepts %q plus: %s", where, key, rootPathKey, strings.Join(profileKnobs, ", "))
			}
			return rootEntry{}, fmt.Errorf("%s has unknown key %q (typo?): a library_roots entry accepts "+
				"%q plus: %s", where, key, rootPathKey, strings.Join(profileKnobs, ", "))
		}
		if val == nil {
			// Neither an override nor an inherit. A profile key is "present overrides,
			// absent inherits", and a key written with no value at all is neither - so
			// it is refused rather than guessed at, on a tool whose knobs decide whether
			// an original is deleted.
			return rootEntry{}, fmt.Errorf("%s sets %q with no value: a profile knob either carries a value "+
				"(which overrides the top level, even when it is zero) or is absent (which inherits it)",
				where, key)
		}
		override[key] = val
	}
	if len(override) == 0 {
		override = nil
	}
	return checkedEntry(i, p, override, file)
}

// checkedEntry applies the two checks that hold for BOTH spellings of an entry, so a
// mapping carrying only `path` really is the bare string form of the same path and not
// merely something like it.
func checkedEntry(i int, p string, override map[string]any, file string) (rootEntry, error) {
	if strings.TrimSpace(p) == "" && override == nil {
		// A bare empty entry keeps the refusal it has always had, in Validate, which
		// names it the same way for a Config assembled by hand.
		return rootEntry{path: p}, nil
	}
	if strings.TrimSpace(p) == "" {
		return rootEntry{}, fmt.Errorf("library_roots[%d] in %s has an empty %q: a profile has to say which "+
			"tree it is about", i, file, rootPathKey)
	}
	if !filepath.IsAbs(p) {
		return rootEntry{}, fmt.Errorf("library_roots[%d] in %s is %q, which is not an absolute path: this "+
			"tool only ever mutates the filesystem under a library root, so a root is never resolved "+
			"against a working directory", i, file, p)
	}
	return rootEntry{path: p, override: override}, nil
}

// resolveRoots produces one resolved profile per entry: the top-level value of every
// knob, overridden by whatever that entry carried, decoded through the same weakly-typed
// decoder the top level uses so an environment string and a YAML integer land the same
// way in both.
//
// explicitTop is the set of top-level keys the FILE or the ENVIRONMENT actually carried,
// which is the only way to tell a value that was chosen at the top level from one that
// is simply the built-in default - the resolved value is identical either way.
func resolveRoots(k *koanf.Koanf, entries []rootEntry, explicitTop map[string]bool, file string) ([]Root, error) {
	roots := make([]Root, 0, len(entries))
	for _, e := range entries {
		m := make(map[string]any, len(profileKnobs))
		layers := make(map[string]Layer, len(profileKnobs))
		for _, knob := range profileKnobs {
			m[knob] = k.Get(knob)
			if explicitTop[knob] {
				layers[knob] = LayerTopLevel
			} else {
				layers[knob] = LayerDefault
			}
		}
		for knob, v := range e.override {
			m[knob] = v
			layers[knob] = LayerProfile
		}
		var p Profile
		if err := decodeProfile(m, &p); err != nil {
			return nil, fmt.Errorf("resolving the profile of library root %q in %s: %w", e.path, file, err)
		}
		roots = append(roots, Root{
			Path:    e.path,
			Clean:   filepath.Clean(e.path),
			Profile: p,
			Layers:  layers,
		})
	}
	return roots, nil
}

// decodeProfile turns a resolved knob map into a Profile. WeaklyTypedInput matches the
// top-level decoder exactly: an environment override arrives as a string and a YAML
// integer as an int, and a profile must read both the way the top level does.
func decodeProfile(m map[string]any, p *Profile) error {
	dec, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		WeaklyTypedInput: true,
		Result:           p,
		TagName:          "yaml",
	})
	if err != nil {
		return err
	}
	return dec.Decode(m)
}
