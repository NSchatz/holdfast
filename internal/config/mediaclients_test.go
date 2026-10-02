package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/secret"
)

// loadValidated loads cfg and runs Validate, returning whichever of the two refused.
func loadValidated(t *testing.T, cfg string) (*Config, error) {
	t.Helper()
	c, err := load(t, cfg)
	if err != nil {
		return nil, err
	}
	return c, c.Validate()
}

const mediaBase = "library_roots:\n  - /mnt/media\n"

// The three credential keys, and the address each one enables a target with.
var mediaCredentials = []struct{ key, urlKey string }{
	{"plex_token", "plex_url"},
	{"sonarr_api_key", "sonarr_url"},
	{"radarr_api_key", "radarr_url"},
}

// TestSecretKeys_PlexSonarrRadarrAreSecretBearingAndRefuseALiteral is S0179 AC-12 and the
// goal's credential line: each of the three media-server credentials is in the closed list
// of secret-bearing keys, a literal value is refused naming the key - in the YAML file and
// in the HOLDFAST_* variable - with no part of the value in the refusal, and a `file:`
// reference is accepted and resolves to the file's contents.
func TestSecretKeys_PlexSonarrRadarrAreSecretBearingAndRefuseALiteral(t *testing.T) {
	const literal = "LITERAL-CREDENTIAL-MUST-NEVER-BE-ECHOED"
	for _, cred := range mediaCredentials {
		t.Run(cred.key, func(t *testing.T) {
			listed := false
			for _, k := range SecretBearingKeys {
				listed = listed || k == cred.key
			}
			if !listed {
				t.Fatalf("%s is not in SecretBearingKeys: %v", cred.key, SecretBearingKeys)
			}

			refuses := func(where string, err error) {
				t.Helper()
				if err == nil {
					t.Fatalf("a literal %s %s was ACCEPTED", cred.key, where)
				}
				var lit *secret.ErrLiteral
				if !errorsAsLiteral(err, &lit) {
					t.Errorf("a literal %s %s was refused, but not as a literal credential: %v", cred.key, where, err)
				}
				if !strings.Contains(err.Error(), cred.key) {
					t.Errorf("the refusal %s does not name %s: %v", where, cred.key, err)
				}
				if strings.Contains(err.Error(), literal) {
					t.Errorf("the refusal %s echoes the literal value: %v", where, err)
				}
			}

			// In the YAML file, with the address beside it so nothing else can be the refusal.
			_, err := loadValidated(t, mediaBase+cred.urlKey+": http://media.invalid:1\n"+cred.key+": "+literal+"\n")
			refuses("in the YAML file", err)

			// In the HOLDFAST_* variable.
			t.Setenv("HOLDFAST_"+strings.ToUpper(cred.key), literal)
			_, err = loadValidated(t, mediaBase+cred.urlKey+": http://media.invalid:1\n")
			refuses("in HOLDFAST_"+strings.ToUpper(cred.key), err)
			t.Setenv("HOLDFAST_"+strings.ToUpper(cred.key), "")

			// A file: reference is accepted, and resolves to the file's contents.
			secretFile := filepath.Join(t.TempDir(), "credential")
			if err := os.WriteFile(secretFile, []byte("resolved-"+cred.key+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			c, err := loadValidated(t, mediaBase+cred.urlKey+": http://media.invalid:1\n"+cred.key+": file:"+secretFile+"\n")
			if err != nil {
				t.Fatalf("a file: reference in %s was refused: %v", cred.key, err)
			}
			refs, err := c.SecretRefs()
			if err != nil {
				t.Fatalf("SecretRefs: %v", err)
			}
			set, err := secret.Resolve(context.Background(), refs)
			if err != nil {
				t.Fatalf("resolving %s: %v", cred.key, err)
			}
			if got := set.Get(cred.key).Expose(); got != "resolved-"+cred.key {
				t.Errorf("%s resolved to %q, want the file's contents", cred.key, got)
			}
			if !c.MediaTarget(strings.SplitN(cred.key, "_", 2)[0]).Enabled {
				t.Errorf("the target is not enabled with its address and its credential reference both set")
			}
		})
	}
}

func errorsAsLiteral(err error, target **secret.ErrLiteral) bool {
	for err != nil {
		if l, ok := err.(*secret.ErrLiteral); ok {
			*target = l
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// TestMediaTargets_AC12_ACmdReferenceIsAccepted: the second reference form is accepted for
// each credential, by shape (S0179 AC-12).
func TestMediaTargets_AC12_ACmdReferenceIsAccepted(t *testing.T) {
	for _, cred := range mediaCredentials {
		c, err := loadValidated(t, mediaBase+cred.urlKey+": https://media.invalid\n"+cred.key+": cmd:/bin/echo value\n")
		if err != nil {
			t.Fatalf("%s: a cmd: reference was refused: %v", cred.key, err)
		}
		if got := c.SecretRef(cred.key).Kind(); got != "cmd" {
			t.Errorf("%s parsed as a %q reference, want cmd", cred.key, got)
		}
	}
}

// TestMediaTargets_AC13_AHalfConfiguredTargetIsRefusedNamingBothKeys: an address with no
// credential, and a credential with no address, each refuse naming the key (S0179 AC-13).
func TestMediaTargets_AC13_AHalfConfiguredTargetIsRefusedNamingBothKeys(t *testing.T) {
	for _, cred := range mediaCredentials {
		for name, body := range map[string]string{
			"address only":           cred.urlKey + ": http://media.invalid:1\n",
			"credential only":        cred.key + ": file:/run/secrets/x\n",
			"address and blank cred": cred.urlKey + ": http://media.invalid:1\n" + cred.key + ": \"  \"\n",
		} {
			t.Run(cred.key+"/"+name, func(t *testing.T) {
				_, err := loadValidated(t, mediaBase+body)
				if err == nil {
					t.Fatal("a half-configured target was accepted")
				}
				for _, want := range []string{cred.key, cred.urlKey, "half-configured"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("the refusal does not say %q: %v", want, err)
					}
				}
			})
		}
	}
}

// TestMediaTargets_AC13_AnAddressThatIsNotAnAbsoluteHTTPURLIsRefused: the address is a
// scheme, a host and an optional base path, http or https, and nothing else. The refusal
// names the key and never quotes the value, which may be where a credential was pasted.
func TestMediaTargets_AC13_AnAddressThatIsNotAnAbsoluteHTTPURLIsRefused(t *testing.T) {
	bad := map[string]string{
		"no scheme":          "media.invalid:8989",
		"a path":             "/api",
		"another scheme":     "ftp://media.invalid",
		"no host":            "http://",
		"a port and no host": "http://:8989",
		"userinfo":           "http://user:PASTED-SECRET@media.invalid",
		"a query":            "http://media.invalid/?apikey=PASTED-SECRET",
		"an empty query":     "http://media.invalid/?",
		"a fragment":         "http://media.invalid/#PASTED-SECRET",
		"unparseable":        "http://media.invalid/%zz",
	}
	for _, cred := range mediaCredentials {
		for name, addr := range bad {
			t.Run(cred.urlKey+"/"+name, func(t *testing.T) {
				_, err := loadValidated(t, mediaBase+cred.urlKey+": \""+addr+"\"\n"+cred.key+": file:/run/secrets/x\n")
				if err == nil {
					t.Fatalf("%s: %q was accepted", cred.urlKey, addr)
				}
				if !strings.Contains(err.Error(), cred.urlKey) {
					t.Errorf("the refusal does not name %s: %v", cred.urlKey, err)
				}
				if strings.Contains(err.Error(), "PASTED-SECRET") {
					t.Errorf("the refusal quotes the configured value: %v", err)
				}
			})
		}
	}
	// Anti-vacuity: the accepted shapes are accepted, with and without a base path, and a
	// trailing slash is not part of the address a client is given.
	for _, good := range []string{"http://media.invalid:8989", "https://media.invalid/sonarr/", "HTTP://10.0.0.1:32400"} {
		c, err := loadValidated(t, mediaBase+"sonarr_url: "+good+"\nsonarr_api_key: file:/run/secrets/x\n")
		if good == "HTTP://10.0.0.1:32400" {
			// url.Parse lower-cases the scheme, so this is accepted too.
			if err != nil {
				t.Errorf("%q was refused: %v", good, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%q was refused: %v", good, err)
		}
		if got := c.MediaTarget("sonarr").URL; strings.HasSuffix(got, "/") {
			t.Errorf("the target address %q keeps a trailing slash", got)
		}
	}
}

// TestMediaTargets_AC13_APathMapSideThatIsNotAbsoluteIsRefused covers every shape a path map
// may not be, each refused naming the key and the entry (S0179 AC-13).
func TestMediaTargets_AC13_APathMapSideThatIsNotAbsoluteIsRefused(t *testing.T) {
	cases := []struct{ name, yaml, want string }{
		{"a relative from", "  - {from: media, to: /data}\n", "[0].from"},
		{"a relative to", "  - {from: /mnt/media, to: data}\n", "[0].to"},
		{"an empty to", "  - {from: /mnt/media, to: \"\"}\n", "[0].to"},
		{"the second entry", "  - {from: /a, to: /b}\n  - {from: /c, to: d}\n", "[1].to"},
		{"a missing to", "  - {from: /mnt/media}\n", "[0]"},
		{"a missing from", "  - {to: /mnt/media}\n", "[0]"},
		{"a misspelled side", "  - {form: /mnt/media, to: /data}\n", "form"},
		{"a number for a side", "  - {from: 4, to: /data}\n", "[0]"},
		{"an entry that is a string", "  - /mnt/media:/data\n", "[0]"},
		{"a repeated from", "  - {from: /a, to: /b}\n  - {from: /a/, to: /c}\n", "[1].from"},
		{"a repeated to", "  - {from: /a, to: /b}\n  - {from: /c, to: /b}\n", "[1].to"},
	}
	for _, key := range []string{"radarr_path_map", "sonarr_path_map", "plex_path_map"} {
		for _, tc := range cases {
			t.Run(key+"/"+tc.name, func(t *testing.T) {
				_, err := loadValidated(t, mediaBase+key+":\n"+tc.yaml)
				if err == nil {
					t.Fatal("the path map was accepted")
				}
				if !strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), tc.want) {
					t.Errorf("the refusal must name %s and %q: %v", key, tc.want, err)
				}
			})
		}
		t.Run(key+"/not a list", func(t *testing.T) {
			_, err := loadValidated(t, mediaBase+key+": /mnt/media\n")
			if err == nil || !strings.Contains(err.Error(), key) {
				t.Errorf("a path map that is not a list must refuse naming %s: %v", key, err)
			}
		})
		t.Run(key+"/in the environment", func(t *testing.T) {
			t.Setenv("HOLDFAST_"+strings.ToUpper(key), "/a=/b")
			_, err := loadValidated(t, mediaBase)
			if err == nil || !strings.Contains(err.Error(), "HOLDFAST_"+strings.ToUpper(key)) {
				t.Errorf("a path map in the environment must refuse naming the variable: %v", err)
			}
		})
	}
}

// TestMediaTargets_OffByDefaultAndReadAsWritten: with none of the nine keys written no
// target is enabled, and with all nine written each target carries its own address and map.
func TestMediaTargets_OffByDefaultAndReadAsWritten(t *testing.T) {
	c, err := loadValidated(t, mediaBase)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range c.MediaTargets() {
		if target.Enabled || target.URL != "" || len(target.PathMap) != 0 {
			t.Errorf("the %s target is not off by default: %+v", target.Name, target)
		}
	}

	c, err = loadValidated(t, mediaBase+
		"radarr_url: http://radarr.invalid:7878\nradarr_api_key: file:/run/secrets/radarr\n"+
		"radarr_path_map:\n  - {from: /mnt/media/movies, to: /movies}\n"+
		"sonarr_url: http://sonarr.invalid:8989\nsonarr_api_key: file:/run/secrets/sonarr\n"+
		"sonarr_path_map:\n  - from: /mnt/media/tv\n    to: /tv\n"+
		"plex_url: http://plex.invalid:32400\nplex_token: file:/run/secrets/plex\n"+
		"plex_path_map:\n  - {from: /mnt/media, to: /data/media}\n  - {from: /mnt/media/tv, to: /data/shows}\n")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]struct {
		url  string
		from string
		to   string
	}{
		"radarr": {"http://radarr.invalid:7878", "/mnt/media/movies/Film/a.mkv", "/movies/Film/a.mkv"},
		"sonarr": {"http://sonarr.invalid:8989", "/mnt/media/tv/Show/S1/e.mkv", "/tv/Show/S1/e.mkv"},
		"plex":   {"http://plex.invalid:32400", "/mnt/media/tv/Show/S1/e.mkv", "/data/shows/Show/S1/e.mkv"},
	}
	names := []string{}
	for _, target := range c.MediaTargets() {
		names = append(names, target.Name)
		w := want[target.Name]
		if !target.Enabled || target.URL != w.url {
			t.Errorf("%s: enabled=%v url=%q, want enabled at %q", target.Name, target.Enabled, target.URL, w.url)
		}
		if got := target.PathMap.Map(w.from); got != w.to {
			t.Errorf("%s: Map(%q) = %q, want %q", target.Name, w.from, got, w.to)
		}
	}
	if strings.Join(names, ",") != "radarr,sonarr,plex" {
		t.Errorf("the targets are reported as %v, want radarr, sonarr, plex in that order", names)
	}
	defer func() {
		if recover() == nil {
			t.Error("MediaTarget with an unknown name did not panic")
		}
	}()
	c.MediaTarget("jellyfin")
}

// TestPathMap_MapAndReverse is the path map's rule in both directions: the longest prefix
// wins, a prefix matches on whole components only, and an unmapped path passes unchanged.
func TestPathMap_MapAndReverse(t *testing.T) {
	m := PathMap{
		{From: "/mnt/media", To: "/data"},
		{From: "/mnt/media/tv", To: "/shows"},
		{From: "/mnt/media/tv/kids/", To: "/kids/"},
	}
	forward := []struct{ in, want string }{
		{"/mnt/media/movies/Film/a.mkv", "/data/movies/Film/a.mkv"}, // the short prefix
		{"/mnt/media/tv/Show/e.mkv", "/shows/Show/e.mkv"},           // the longer one wins
		{"/mnt/media/tv/kids/Show/e.mkv", "/kids/Show/e.mkv"},       // the longest wins
		{"/mnt/media/tv", "/shows"},                                 // the prefix itself
		{"/mnt/media/tv/", "/shows"},                                // cleaned before matching
		{"/mnt/media/tv2/Show/e.mkv", "/data/tv2/Show/e.mkv"},       // `tv` does not own `tv2`
		{"/mnt/media2/x.mkv", "/mnt/media2/x.mkv"},                  // no entry: unchanged
		{"/mnt/mediax", "/mnt/mediax"},                              // a string prefix is not a prefix
		{"/elsewhere/./a/../b.mkv", "/elsewhere/b.mkv"},             // unchanged, and cleaned
	}
	for _, tc := range forward {
		if got := m.Map(tc.in); got != tc.want {
			t.Errorf("Map(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	back := []struct{ in, want string }{
		{"/data/movies/Film/a.mkv", "/mnt/media/movies/Film/a.mkv"},
		{"/shows/Show/e.mkv", "/mnt/media/tv/Show/e.mkv"},
		{"/kids/Show/e.mkv", "/mnt/media/tv/kids/Show/e.mkv"},
		{"/shows", "/mnt/media/tv"},
		{"/shows2/Show/e.mkv", "/shows2/Show/e.mkv"}, // `/shows` does not own `/shows2`
		{"/dataset/x.mkv", "/dataset/x.mkv"},         // nor `/data` `/dataset`
		{"/other/x.mkv", "/other/x.mkv"},
	}
	for _, tc := range back {
		if got := m.Reverse(tc.in); got != tc.want {
			t.Errorf("Reverse(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// Reverse undoes Map for every path under a mapped prefix.
	for _, p := range []string{"/mnt/media/a.mkv", "/mnt/media/tv/Show/e.mkv", "/mnt/media/tv/kids/k.mkv"} {
		if got := m.Reverse(m.Map(p)); got != p {
			t.Errorf("Reverse(Map(%q)) = %q", p, got)
		}
	}
	// Reverse picks the longest TO, which need not belong to the entry with the longest FROM.
	crossed := PathMap{{From: "/a/b/c", To: "/x"}, {From: "/a", To: "/x/y/z"}}
	if got := crossed.Reverse("/x/y/z/f.mkv"); got != "/a/f.mkv" {
		t.Errorf("Reverse over crossed lengths = %q, want /a/f.mkv", got)
	}
	if got := crossed.Map("/a/b/c/f.mkv"); got != "/x/f.mkv" {
		t.Errorf("Map over crossed lengths = %q, want /x/f.mkv", got)
	}
	// The empty map changes nothing in either direction.
	var none PathMap
	if none.Map("/mnt/media/a.mkv") != "/mnt/media/a.mkv" || none.Reverse("/data/a.mkv") != "/data/a.mkv" {
		t.Error("the empty map changed a path")
	}
	// A root prefix owns every absolute path, and still joins cleanly.
	root := PathMap{{From: "/", To: "/host"}}
	if got := root.Map("/media/a.mkv"); got != "/host/media/a.mkv" {
		t.Errorf("Map under a / prefix = %q", got)
	}
	if got := root.Map("/"); got != "/host" {
		t.Errorf("Map of / itself = %q", got)
	}
	if got := root.Reverse("/host/media/a.mkv"); got != "/media/a.mkv" {
		t.Errorf("Reverse onto a / prefix = %q", got)
	}
	// A hand-built map with a tie resolves to the entry written first.
	tie := PathMap{{From: "/a", To: "/first"}, {From: "/a", To: "/second"}}
	if got := tie.Map("/a/f"); got != "/first/f" {
		t.Errorf("a tie resolved to %q, want the first entry", got)
	}
}

// TestUnderPrefix is the component-boundary test the maps and the owner lookups share.
func TestUnderPrefix(t *testing.T) {
	cases := []struct {
		p, prefix, rest string
		ok              bool
	}{
		{"/movies/Film", "/movies/Film", "", true},
		{"/movies/Film/a.mkv", "/movies/Film", "a.mkv", true},
		{"/movies/Film 2/a.mkv", "/movies/Film", "", false},
		{"/movies/Film2", "/movies/Film", "", false},
		{"/movies", "/movies/Film", "", false},
		{"/a/b", "/", "a/b", true},
		{"/", "/", "", true},
		{"relative/x", "/", "", false},
	}
	for _, tc := range cases {
		rest, ok := UnderPrefix(tc.p, tc.prefix)
		if rest != tc.rest || ok != tc.ok {
			t.Errorf("UnderPrefix(%q, %q) = %q, %v; want %q, %v", tc.p, tc.prefix, rest, ok, tc.rest, tc.ok)
		}
	}
}
