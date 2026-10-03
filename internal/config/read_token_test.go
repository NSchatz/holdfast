package config

import (
	"strings"
	"testing"
)

// readTokenNotices returns the statements this configuration makes ABOUT the read surface
// to the process that serves it: ReadSurfaceNotices in the serving scope, narrowed to the
// ones naming server_read_token. The filter is kept although that list holds nothing else
// today, because "exactly one" is a claim about THIS statement and must stay one if the
// list ever grows.
//
// It read Notices() until S0175 moved the two statements out of the list every command
// logs: a oneshot `run` binds no listener, so it has no read surface to describe. The three
// directions below are graded on the serving scope, which is where each is a fact; the
// scoping itself is graded by TestNotices_CarryNoReadSurfaceStatement and
// TestReadSurfaceNotices_ValidatingScopeIsHedged.
func readTokenNotices(c *Config) []string {
	var out []string
	for _, n := range c.ReadSurfaceNotices(ReadSurfaceServing) {
		if strings.Contains(n, "server_read_token") {
			out = append(out, n)
		}
	}
	return out
}

// TestNotices_WarnsOnNonLoopbackBindWithNoReadToken is the notice's whole contract, in
// the three directions it has: it fires on a non-loopback bind with no read token, it is
// silent on a loopback bind, and it is silent whatever the bind is once a read token is
// configured, because the exposure it describes no longer exists.
//
// The bind table is the point of the test rather than a detail of it. "Not loopback" is
// the kind of predicate that looks obvious and is wrong on exactly one spelling: `:8080`
// binds every interface while LOOKING like a bare port, and an ABSENT server_addr binds
// the loopback default while looking like nothing at all. Both are in the table, on
// opposite sides of it.
func TestNotices_WarnsOnNonLoopbackBindWithNoReadToken(t *testing.T) {
	// The addresses that are NOT loopback: every media path is reachable from off-host,
	// so the notice is owed.
	t.Run("non-loopback bind with no read token", func(t *testing.T) {
		for _, addr := range []string{
			":8080",                // an EMPTY host binds every interface
			"0.0.0.0:8080",         // the all-interfaces IPv4 literal
			"[::]:8080",            // the all-interfaces IPv6 literal
			"192.168.1.10:8080",    // a routable RFC1918 literal
			"203.0.113.7:8080",     // a public literal
			"holdfast.lan:8080",    // a hostname that is not localhost
			"media.example.com:80", // a public hostname
		} {
			t.Run(addr, func(t *testing.T) {
				c := &Config{LibraryRoots: []string{"/mnt/media"}, ServerAddr: addr}
				got := readTokenNotices(c)
				if len(got) != 1 {
					t.Fatalf("ReadSurfaceNotices(serving) carried %d notices naming server_read_token, want exactly 1:\n%v", len(got), got)
				}
				// It must name BOTH keys: one says where the exposure is, the other says
				// what closes it, and a notice carrying only one of them leaves an
				// operator with half an instruction.
				if !strings.Contains(got[0], "server_addr") {
					t.Errorf("the notice does not name server_addr:\n%s", got[0])
				}
				if !strings.Contains(got[0], addr) {
					t.Errorf("the notice does not name the bind address %q:\n%s", addr, got[0])
				}
				// And it must state the consequence in the operator's terms - what is
				// served, to whom - rather than merely naming a key that is unset.
				low := strings.ToLower(got[0])
				for _, phrase := range []string{"every media path", "without a credential"} {
					if !strings.Contains(low, phrase) {
						t.Errorf("the notice never says %q, so it does not state the exposure:\n%s", phrase, got[0])
					}
				}
			})
		}
	})

	// The addresses that ARE loopback: nothing off-host can reach the read API, so there
	// is no exposure to state and a notice would be noise on the shipped default.
	t.Run("loopback bind emits no such notice", func(t *testing.T) {
		for _, addr := range []string{
			"127.0.0.1:8080",  // the shipped default, written out
			"",                // an ABSENT server_addr, which EffectiveServerAddr defaults to it
			"127.0.0.53:8080", // another 127.0.0.0/8 literal
			"[::1]:8080",      // the IPv6 loopback
			"localhost:8080",  // the name
			"LOCALHOST:8080",  // the name, as an operator may well have written it
		} {
			t.Run("addr="+addr, func(t *testing.T) {
				c := &Config{LibraryRoots: []string{"/mnt/media"}, ServerAddr: addr}
				if got := readTokenNotices(c); len(got) != 0 {
					t.Fatalf("a loopback bind (%q) emitted %d read-surface notice(s):\n%v", addr, len(got), got)
				}
			})
		}
	})

	// A configured read token silences it on EVERY address, including the ones above:
	// the reads are gated, so "every media path is served without a credential" is no
	// longer true and stating it would be a false alarm.
	t.Run("a configured read token silences it whatever the bind is", func(t *testing.T) {
		for _, addr := range []string{":8080", "0.0.0.0:8080", "192.168.1.10:8080", "127.0.0.1:8080", ""} {
			c := &Config{
				LibraryRoots:    []string{"/mnt/media"},
				ServerAddr:      addr,
				ServerReadToken: "file:/run/secrets/holdfast-read-token",
			}
			for _, n := range readTokenNotices(c) {
				if strings.Contains(strings.ToLower(n), "every media path") {
					t.Errorf("addr %q with a read token set still announced the open-library exposure:\n%s", addr, n)
				}
			}
		}
	})
}

// TestNotices_StatesTheRootPageIsStillOpenWhenAReadTokenIsSet is the other half of the
// startup statement, and the one an operator cannot get anywhere else. With a read token
// set the root path is STILL SERVED with no credential, and so is /metrics: this key
// gates /api reads and nothing else. A half-gated surface that reads as gated is worse
// than an open one that says it is open, and this is where it gets said.
func TestNotices_StatesTheRootPageIsStillOpenWhenAReadTokenIsSet(t *testing.T) {
	c := &Config{
		LibraryRoots:    []string{"/mnt/media"},
		ServerAddr:      "0.0.0.0:8080",
		ServerReadToken: "file:/run/secrets/holdfast-read-token",
	}
	got := readTokenNotices(c)
	if len(got) != 1 {
		t.Fatalf("ReadSurfaceNotices(serving) carried %d notices naming server_read_token, want exactly 1:\n%v", len(got), got)
	}
	low := strings.ToLower(got[0])
	for _, phrase := range []string{
		"root path at /",       // WHICH surface is still open
		"without a credential", // that it is served without one
		"/metrics",             // and the other surface this key does not reach
		"no library datum",     // and what that costs: nothing, which is why it is safe
	} {
		if !strings.Contains(low, phrase) {
			t.Errorf("the read-token notice never says %q:\n%s", phrase, got[0])
		}
	}
}

// readSurfaceBinds is every bind the tests below put a configuration on: the non-loopback
// spellings and the loopback ones, so a scoping claim is made over both sides of the rule.
var readSurfaceBinds = []string{":8080", "0.0.0.0:8080", "[::]:8080", "192.168.1.10:8080", "holdfast.lan:8080",
	"", "127.0.0.1:8080", "[::1]:8080", "localhost:8080"}

// TestNotices_CarryNoReadSurfaceStatement (S0175 AC-1's unit half): Notices() is the list
// EVERY command states, `run` included, and a oneshot run opens no listener. So on no bind
// and with the token set or unset does it name server_read_token or say anything is served
// without a credential - while the undo-window statement, which IS true of a run, stays in
// the same list. The second half is what makes the first a scoping and not a deletion.
func TestNotices_CarryNoReadSurfaceStatement(t *testing.T) {
	for _, addr := range readSurfaceBinds {
		for _, token := range []string{"", "file:/run/secrets/holdfast-read-token"} {
			c := &Config{LibraryRoots: []string{"/mnt/media"}, ServerAddr: addr, ServerReadToken: token}
			var undo bool
			for _, n := range c.Notices() {
				low := strings.ToLower(n)
				if strings.Contains(low, "server_read_token") || strings.Contains(low, "without a credential") {
					t.Errorf("addr %q token %q: Notices() describes a read surface a oneshot run does not open:\n%s",
						addr, token, n)
				}
				undo = undo || strings.Contains(n, "undo_window_hours is 0")
			}
			if !undo {
				t.Errorf("addr %q token %q: Notices() lost the undo-window statement: %v", addr, token, c.Notices())
			}
		}
	}
}

// TestReadSurfaceNotices_ValidatingScopeIsHedged (S0175 AC-3, AC-4, AC-5 at the unit): what
// `validate` is told. It sees the file and its own environment only, so on a non-loopback
// bind with no token it says the key is not set IN THIS CONFIG, names where else it may be
// supplied, and states the exposure as a consequence - never as "is served", which is the
// assertion it cannot observe. The serving scope on the same configuration is asserted
// beside it to be the unhedged one, so the two cannot be swapped unnoticed.
func TestReadSurfaceNotices_ValidatingScopeIsHedged(t *testing.T) {
	for _, addr := range []string{":8080", "0.0.0.0:8080", "[::]:8080", "192.168.1.10:8080", "holdfast.lan:8080"} {
		t.Run(addr, func(t *testing.T) {
			c := &Config{LibraryRoots: []string{"/mnt/media"}, ServerAddr: addr}
			got := c.ReadSurfaceNotices(ReadSurfaceValidating)
			if len(got) != 1 {
				t.Fatalf("the validating scope made %d statements, want exactly 1:\n%v", len(got), got)
			}
			low := strings.ToLower(got[0])
			for _, want := range []string{"server_read_token", "not set in this config", "may be supplied by",
				"holdfast_server_read_token", "server_addr", strings.ToLower(addr), "every media path",
				"without a credential", "no library datum"} {
				if !strings.Contains(low, want) {
					t.Errorf("the validating statement never says %q:\n%s", want, got[0])
				}
			}
			if strings.Contains(low, "is served without a credential") {
				t.Errorf("the validating statement ASSERTS an exposure `validate` cannot observe:\n%s", got[0])
			}

			serving := c.ReadSurfaceNotices(ReadSurfaceServing)
			if len(serving) != 1 {
				t.Fatalf("the serving scope made %d statements, want exactly 1:\n%v", len(serving), serving)
			}
			sl := strings.ToLower(serving[0])
			for _, hedge := range []string{"not set in this config", "may be supplied by"} {
				if strings.Contains(sl, hedge) {
					t.Errorf("the SERVING statement is hedged (%q); the process that serves knows:\n%s", hedge, serving[0])
				}
			}
			if !strings.Contains(sl, "is served without a credential") {
				t.Errorf("the serving statement does not assert the exposure:\n%s", serving[0])
			}
			// The zero value is the serving scope: a caller that forgets to choose says the
			// loud statement, never the soft one.
			var zero ReadSurfaceScope
			if z := c.ReadSurfaceNotices(zero); len(z) != 1 || z[0] != serving[0] {
				t.Errorf("the zero ReadSurfaceScope is not the serving scope: %v", z)
			}
		})
	}

	// Loopback, including an absent server_addr: nothing in either scope.
	for _, addr := range []string{"", "127.0.0.1:8080", "127.0.0.53:8080", "[::1]:8080", "localhost:8080", "LOCALHOST:8080"} {
		c := &Config{LibraryRoots: []string{"/mnt/media"}, ServerAddr: addr}
		if got := c.ReadSurfaceNotices(ReadSurfaceValidating); len(got) != 0 {
			t.Errorf("a loopback bind (%q) made the validating scope say:\n%v", addr, got)
		}
	}

	// A token the command CAN see: the set-token statement, identical in both scopes, and
	// neither carries the reference.
	const ref = "file:/run/secrets/holdfast-read-token"
	for _, addr := range readSurfaceBinds {
		c := &Config{LibraryRoots: []string{"/mnt/media"}, ServerAddr: addr, ServerReadToken: ref}
		v, s := c.ReadSurfaceNotices(ReadSurfaceValidating), c.ReadSurfaceNotices(ReadSurfaceServing)
		if len(v) != 1 || len(s) != 1 || v[0] != s[0] {
			t.Fatalf("addr %q with a token set: the scopes disagree or do not make exactly one statement:\n%v\n%v", addr, v, s)
		}
		low := strings.ToLower(v[0])
		for _, banned := range []string{"not set in this config", "every media path", ref, "/run/secrets"} {
			if strings.Contains(low, banned) {
				t.Errorf("addr %q: the set-token statement carries %q:\n%s", addr, banned, v[0])
			}
		}
	}
}

// TestWarnings_AreUnchangedByTheReadSurfaceNotices: a WARNING is always a weakened safety
// gate, and neither of these is one - the shipped default is an open read API, and a
// default that warns is how an operator learns to skip warnings. The two lists stay
// separate, asserted on the configuration most likely to blur them.
func TestWarnings_AreUnchangedByTheReadSurfaceNotices(t *testing.T) {
	base := Config{LibraryRoots: []string{"/mnt/media"}}
	exposed := base
	exposed.ServerAddr = "0.0.0.0:8080"
	gated := exposed
	gated.ServerReadToken = "file:/run/secrets/holdfast-read-token"

	want := strings.Join(base.Warnings(), "\n")
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{"non-loopback bind with no read token", exposed},
		{"non-loopback bind with a read token", gated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := strings.Join(tc.cfg.Warnings(), "\n"); got != want {
				t.Errorf("Warnings() changed with the bind address:\n got: %s\nwant: %s", got, want)
			}
			for _, w := range tc.cfg.Warnings() {
				if strings.Contains(w, "server_read_token") {
					t.Errorf("the read surface produced a WARNING, which is reserved for a weakened gate:\n%s", w)
				}
			}
		})
	}
}

// TestLoad_ServerReadToken covers the three layers the key can arrive through and the
// precedence between them, plus the near-miss spellings that must not be absorbed.
func TestLoad_ServerReadToken(t *testing.T) {
	const base = "library_roots:\n  - /mnt/media\n"

	t.Run("default is empty, which leaves the read API open", func(t *testing.T) {
		c := loadYAML(t, base)
		if c.ServerReadToken != "" {
			t.Errorf("server_read_token default = %q, want empty", c.ServerReadToken)
		}
	})

	t.Run("from the YAML file", func(t *testing.T) {
		c := loadYAML(t, base+"server_read_token: file:/run/secrets/read-token\n")
		if c.ServerReadToken != "file:/run/secrets/read-token" {
			t.Errorf("server_read_token = %q, want the file value", c.ServerReadToken)
		}
	})

	t.Run("from HOLDFAST_SERVER_READ_TOKEN", func(t *testing.T) {
		t.Setenv("HOLDFAST_SERVER_READ_TOKEN", "file:/run/secrets/from-env")
		c := loadYAML(t, base)
		if c.ServerReadToken != "file:/run/secrets/from-env" {
			t.Errorf("server_read_token = %q, want the environment value", c.ServerReadToken)
		}
	})

	// Precedence: the environment is the layer an operator reaches for precisely because
	// it is the one that does not have to be written down, so it has to win over a value
	// already in the file rather than be quietly ignored beside it.
	t.Run("the environment wins over the file", func(t *testing.T) {
		t.Setenv("HOLDFAST_SERVER_READ_TOKEN", "file:/run/secrets/from-env")
		c := loadYAML(t, base+"server_read_token: file:/run/secrets/from-file\n")
		if c.ServerReadToken != "file:/run/secrets/from-env" {
			t.Errorf("server_read_token = %q, want the environment value to win", c.ServerReadToken)
		}
	})

	// A near miss must REFUSE, not default. The failure this prevents is silent and
	// total: `server_readtoken` is accepted as an unknown key, the real key stays empty,
	// and the read API an operator believes they just gated serves every media path to
	// anyone who asks.
	t.Run("a near-miss key refuses to load", func(t *testing.T) {
		for _, key := range []string{"server_readtoken", "read_token", "server_read_tokens", "serverreadtoken"} {
			t.Run(key, func(t *testing.T) {
				_, err := load(t, base+key+": file:/run/secrets/read-token\n")
				if err == nil {
					t.Fatalf("Load accepted %q and left the read API open", key)
				}
				if !strings.Contains(err.Error(), "unknown config key") || !strings.Contains(err.Error(), key) {
					t.Errorf("the refusal must be the unknown-key error naming %q; got: %v", key, err)
				}
			})
		}
	})

	// Anti-vacuity: the same file with the key spelled correctly LOADS, so the refusals
	// above are about the spelling and not about the shape of the file.
	t.Run("the correctly spelled key loads", func(t *testing.T) {
		c := loadYAML(t, base+"server_read_token: file:/run/secrets/read-token\n")
		if c.ServerReadToken == "" {
			t.Error("the correctly spelled key did not load")
		}
	})
}

// TestValidate_RefusesAWhitespaceOnlyReadToken: a value that is non-empty but all
// whitespace is the one shape of this key an operator cannot detect from the outside. A
// reference is trimmed before it is read, so it resolves to "unconfigured" and the read
// API stays OPEN while the configuration says in plain sight that it is gated - and an
// open read API answers a credential-less request exactly the way a correctly gated one
// answers a credentialled one, so nothing about the daemon's behaviour gives it away.
func TestValidate_RefusesAWhitespaceOnlyReadToken(t *testing.T) {
	for _, value := range []string{" ", "   ", "\t", "\n", " \t\n "} {
		c := Config{LibraryRoots: []string{"/mnt/media"}, ServerReadToken: value}
		err := c.Validate()
		if err == nil {
			t.Fatalf("Validate accepted a whitespace-only server_read_token (%q)", value)
		}
		if !strings.Contains(err.Error(), "server_read_token") {
			t.Errorf("the refusal must name the key; got: %v", err)
		}
	}

	// Anti-vacuity, both ways: an ABSENT key is the shipped default and must validate,
	// and a real reference must validate too, so the refusal is about whitespace rather
	// than about the key existing at all.
	for _, value := range []string{"", "file:/run/secrets/holdfast-read-token"} {
		c := Config{LibraryRoots: []string{"/mnt/media"}, ServerReadToken: value}
		if err := c.Validate(); err != nil {
			t.Errorf("Validate refused server_read_token %q: %v", value, err)
		}
	}
}

// TestValidate_RefusesALiteralReadToken: the key carries a REFERENCE, never a credential,
// for the reason every other secret-bearing key does - holdfast starts ffmpeg children,
// and a child inherits its parent's environment, so a literal in HOLDFAST_SERVER_READ_TOKEN
// is readable from every encoder invocation's /proc/<pid>/environ. The message names the
// key and how to convert it and echoes no part of the value.
func TestValidate_RefusesALiteralReadToken(t *testing.T) {
	const pasted = "not-a-reference-just-a-token"
	c := Config{LibraryRoots: []string{"/mnt/media"}, ServerReadToken: pasted}
	err := c.Validate()
	if err == nil {
		t.Fatal("Validate accepted a LITERAL server_read_token")
	}
	if !strings.Contains(err.Error(), "server_read_token") {
		t.Errorf("the refusal must name the key; got: %v", err)
	}
	if strings.Contains(err.Error(), pasted) {
		t.Errorf("the refusal ECHOED the credential: %v", err)
	}
	for _, howTo := range []string{"file:", "cmd:"} {
		if !strings.Contains(err.Error(), howTo) {
			t.Errorf("the refusal must say how to convert the key (missing %q): %v", howTo, err)
		}
	}
}

// TestSecretRefs_CarriesTheReadToken: the key is in the closed list SecretRefs reports
// and resolution reads, so the start-time resolution in cmd/holdfast and the shape check
// in Validate cannot be looking at different sets of keys.
func TestSecretRefs_CarriesTheReadToken(t *testing.T) {
	c := Config{
		LibraryRoots:    []string{"/mnt/media"},
		ServerReadToken: "file:/run/secrets/holdfast-read-token",
	}
	refs, err := c.SecretRefs()
	if err != nil {
		t.Fatalf("SecretRefs: %v", err)
	}
	var found bool
	for _, r := range refs {
		if r.Key() == "server_read_token" {
			found = true
			if !r.Configured() {
				t.Error("server_read_token parsed to an UNCONFIGURED reference despite a file: value")
			}
			if got := r.String(); got != "file:/run/secrets/holdfast-read-token" {
				t.Errorf("reference = %q, want the value as written", got)
			}
		}
	}
	if !found {
		t.Fatalf("SecretRefs did not report server_read_token; got %v", refs)
	}
	var listed bool
	for _, k := range SecretBearingKeys {
		if k == "server_read_token" {
			listed = true
		}
	}
	if !listed {
		t.Errorf("server_read_token is not in SecretBearingKeys (%v), so the AC-7 literal-refusal "+
			"suite would never enumerate it", SecretBearingKeys)
	}
}
