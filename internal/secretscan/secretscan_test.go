package secretscan

import (
	"fmt"
	"strings"
	"testing"
)

// The fixtures, COMPOSED so this file never contains the string its own scanner searches
// for. A literal here would make `make secret-scan` red by construction, and the only ways
// out of that are an allowlist that grows until the scanner stops scanning, or deleting the
// proof. scripts/check-pins-selftest.sh settled that question first.
func fixtures() map[string]string {
	return map[string]string{
		"AWS access key id":           "AKIA" + strings.Repeat("Q", 16),
		"GitHub token":                "ghp_" + strings.Repeat("x", 36),
		"GitHub fine-grained token":   "github_pat_" + strings.Repeat("y", 30),
		"Slack token":                 "xoxb-" + strings.Repeat("1", 24),
		"Slack webhook":               "hooks.slack.com/services/T" + strings.Repeat("Z", 20),
		"Google API key":              "AIza" + strings.Repeat("a", 35),
		"Stripe secret key":           "sk_live_" + strings.Repeat("3", 24),
		"Anthropic API key":           "sk-ant-" + strings.Repeat("A", 40),
		"OpenAI API key":              "sk-" + strings.Repeat("B", 48),
		"PEM private key block":       "-----BEGIN RSA PRIVATE" + " KEY-----",
		"PyPI upload token":           "pypi-" + "AgEIcHlwaS5vcmc" + strings.Repeat("c", 10),
		"npm registry auth directive": "_authToken" + "=deadbeefdeadbeef",
	}
}

// mem is a Source backed by a map, so the ruleset is graded without a repository. The git
// -backed Source is graded by scripts/secret-scan-selftest.sh against real clones.
type mem struct {
	files    map[string]string
	required bool
}

func (m mem) Paths() ([]string, error) {
	out := make([]string, 0, len(m.files))
	for p := range m.files {
		out = append(out, p)
	}
	return out, nil
}
func (m mem) Read(p string) ([]byte, error) {
	b, ok := m.files[p]
	if !ok {
		return nil, fmt.Errorf("no such file %q", p)
	}
	return []byte(b), nil
}
func (m mem) Describe() string    { return "an in-memory fixture tree" }
func (m mem) RequiresFiles() bool { return m.required }

// AC-9: every covered family is caught, and the finding names the path, the LINE NUMBER
// and the family.
func TestSecretScan_AC9_EveryCoveredFamilyIsCaughtWithPathLineAndFamily(t *testing.T) {
	for family, payload := range fixtures() {
		t.Run(family, func(t *testing.T) {
			body := "// a leading line\n// a second leading line\nconst k = \"" + payload + "\"\n"
			got := ScanContent("internal/x/leak.go", []byte(body), Families())
			if len(got) != 1 {
				t.Fatalf("got %d findings, want 1: %v", len(got), got)
			}
			f := got[0]
			if f.Family != family {
				t.Errorf("Family = %q, want %q", f.Family, family)
			}
			if f.Line != 3 {
				t.Errorf("Line = %d, want 3", f.Line)
			}
			if f.Path != "internal/x/leak.go" {
				t.Errorf("Path = %q", f.Path)
			}
			if f.Kind != KindContent {
				t.Errorf("Kind = %q, want %q", f.Kind, KindContent)
			}
		})
	}
}

// AC-9: a finding prints at most a truncated PREFIX and never the whole match. A report is
// pasted into issues and printed into CI logs that outlive the rotation.
func TestSecretScan_AC9_AFindingTruncatesAndNeverPrintsTheWholeMatch(t *testing.T) {
	for family, payload := range fixtures() {
		got := ScanContent("x.go", []byte("k = "+payload), Families())
		if len(got) == 0 {
			t.Fatalf("%s: no finding", family)
		}
		f := got[0]
		if len(f.Prefix) > PrefixLen {
			t.Errorf("%s: prefix is %d bytes, over the %d-byte cap", family, len(f.Prefix), PrefixLen)
		}
		if len(f.Prefix) >= f.MatchLen {
			t.Errorf("%s: prefix is not shorter than the match (%d vs %d)", family, len(f.Prefix), f.MatchLen)
		}
		rendered := f.String()
		if strings.Contains(rendered, payload) {
			t.Errorf("%s: the rendered finding contains the whole credential: %s", family, rendered)
		}
		if !strings.Contains(rendered, f.Prefix) {
			t.Errorf("%s: the rendered finding omits its own prefix: %s", family, rendered)
		}
	}
}

// AC-9: an ordinary tree produces nothing. Without this, every case above could be a
// scanner that flags everything, which would prove nothing at all.
func TestSecretScan_AC9_OrdinarySourceProducesNoFinding(t *testing.T) {
	ordinary := []string{
		"package engine // nothing to see",
		"const defaultServerAddr = \"127.0.0.1:8080\"",
		"server_auth_token: file:/run/secrets/holdfast-control-token",
		"notify_url: cmd:/usr/local/bin/fetch-notify-url",
		"h1:3Hc5ZXzMRfS0iFrPdQ0SZQmPAUEsV4tIEsaXuFDGSBw=", // a go.sum hash
		"sk-short",                    // too short to be an issued key
		"ignore-scripts=true",         // the npm decision, which carries no credential
		"AKIA is a prefix, not a key", // a prefix with no payload
		"-----BEGIN CERTIFICATE-----", // a certificate is public
	}
	for _, line := range ordinary {
		if got := ScanContent("x", []byte(line), Families()); len(got) != 0 {
			t.Errorf("ordinary line %q produced %v", line, got)
		}
	}
}

// AC-10: a forbidden filename is a finding whatever the file contains, and the documented
// example names are the only exceptions - exact NAMES, not a suffix rule, or every
// forbidden name would be one suffix away from allowed.
func TestSecretScan_AC10_ForbiddenNamesAreRefusedWhateverTheyContain(t *testing.T) {
	refused := []string{
		"testdata/.env", "testdata/.env.production", "deploy/.env.local",
		".env", "testdata/id_rsa", "testdata/id_ed25519",
		"testdata/.npmrc", "testdata/.pypirc",
		"testdata/.env.example.real", "testdata/.env.examples",
	}
	for _, p := range refused {
		for _, content := range []string{"", "# nothing secret at all\n", "TOKEN=\n"} {
			f, bad := ForbiddenName(p, nil)
			if !bad {
				t.Errorf("%q was allowed", p)
				continue
			}
			if f.Kind != KindName || f.Path != p || f.Line != 0 {
				t.Errorf("%q: finding = %+v", p, f)
			}
			if strings.Contains(f.String(), content) && content != "" {
				t.Errorf("%q: the finding quoted the file's content", p)
			}
		}
	}
	for _, p := range []string{
		"testdata/.env.example", "testdata/.env.sample", "testdata/.env.template",
		".env.example", "internal/x/config.go", "testdata/id_rsa.pub.go",
	} {
		if _, bad := ForbiddenName(p, nil); bad {
			t.Errorf("%q was refused but is allowed", p)
		}
	}
}

// AC-10/AC-11: the bounded exemption register forgives a NAME and never content, which is
// the whole reason it is safe to have one.
func TestSecretScan_AC10_TheExemptionRegisterForgivesANameAndNeverContent(t *testing.T) {
	const path = "internal/webui/e2e/.npmrc"
	exempt := []NameExemption{{Path: path, Reason: "because the test says so"}}

	if _, bad := ForbiddenName(path, exempt); bad {
		t.Error("the exempted path's NAME was still refused")
	}
	if _, bad := ForbiddenName(path, nil); !bad {
		t.Error("without the register the path is not refused, so the register grants nothing")
	}
	if _, bad := ForbiddenName("other/.npmrc", exempt); !bad {
		t.Error("the register exempted a path it does not name")
	}

	// The content rule still binds it in full.
	body := "ignore-scripts=true\n//registry.npmjs.org/:" + fixtures()["npm registry auth directive"] + "\n"
	src := mem{files: map[string]string{path: body}}
	got, err := ScanWith(src, Families(), exempt)
	if err != nil {
		t.Fatalf("ScanWith: %v", err)
	}
	if len(got) != 1 || got[0].Kind != KindContent || got[0].Line != 2 {
		t.Fatalf("the exempted path was not content-scanned: %v", got)
	}
}

// AC-11: the register refuses to go stale or to grant anything it was not given. An
// allowlist that may grow is an allowlist that grows until the scanner stops scanning.
func TestSecretScan_AC11_TheRegisterRefusesToOutliveItsReason(t *testing.T) {
	present := []string{"internal/webui/e2e/.npmrc", "go.mod"}
	cases := map[string]struct {
		list []NameExemption
		want string
	}{
		"a path that is not in the tree": {
			[]NameExemption{{Path: "gone/.npmrc", Reason: "r"}}, "grants nothing"},
		"a name that was never forbidden": {
			[]NameExemption{{Path: "go.mod", Reason: "r"}}, "not forbidden"},
		"no reason": {
			[]NameExemption{{Path: "internal/webui/e2e/.npmrc", Reason: "  "}}, "no reason"},
		"a duplicate": {
			[]NameExemption{
				{Path: "internal/webui/e2e/.npmrc", Reason: "r"},
				{Path: "internal/webui/e2e/.npmrc", Reason: "r"},
			}, "listed twice"},
		"over the cap": {
			func() []NameExemption {
				out := make([]NameExemption, MaxNameExemptions+1)
				for i := range out {
					out[i] = NameExemption{Path: fmt.Sprintf("d%d/.npmrc", i), Reason: "r"}
				}
				return out
			}(), "more than the"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			problems := ValidateNameExemptions(present, tc.list)
			if len(problems) == 0 {
				t.Fatalf("ValidateNameExemptions accepted %+v", tc.list)
			}
			if !strings.Contains(strings.Join(problems, " "), tc.want) {
				t.Errorf("problems %v do not mention %q", problems, tc.want)
			}
		})
	}
	// The COMMITTED register must itself be valid against the tree it ships with, or the
	// gate is red for everyone.
	if problems := ValidateNameExemptions(present, NameExemptions); len(problems) != 0 {
		t.Errorf("the committed register is not valid: %v", problems)
	}
}

// AC-12: an EMPTY enumeration is a scan that could not run, never a clean tree. Comparing
// nothing against a ruleset passes every file it never saw.
func TestSecretScan_AC12_AnEmptyEnumerationIsNotACleanTree(t *testing.T) {
	_, err := ScanWith(mem{files: map[string]string{}, required: true}, Families(), nil)
	if err == nil {
		t.Fatal("an empty enumeration over the whole tree reported clean")
	}
	if !strings.Contains(err.Error(), "never") {
		t.Errorf("the refusal does not say why: %v", err)
	}
	// A staged set may legitimately be empty: an empty commit is not a broken scan.
	if _, err := ScanWith(mem{files: map[string]string{}}, Families(), nil); err != nil {
		t.Errorf("an empty STAGED set must not be a failure: %v", err)
	}
}

// AC-12: a file the scan could not read is a scan that could not run. A check that could
// not run has not passed.
func TestSecretScan_AC12_AFileThatCannotBeReadIsNotCleared(t *testing.T) {
	src := missingRead{mem{files: map[string]string{"a": "ok"}, required: true}}
	if _, err := ScanWith(src, Families(), nil); err == nil {
		t.Fatal("an unreadable file was reported clean")
	}
}

type missingRead struct{ mem }

func (missingRead) Read(string) ([]byte, error) { return nil, fmt.Errorf("permission denied") }

// A line carrying two families yields one finding, because the outcome - the tree is
// refused, naming that line - does not depend on enumerating the rest.
func TestSecretScan_ALineWithTwoFamiliesYieldsOneFinding(t *testing.T) {
	f := fixtures()
	line := f["AWS access key id"] + " " + f["GitHub token"]
	got := ScanContent("x", []byte(line), Families())
	if len(got) != 1 {
		t.Fatalf("got %d findings, want 1: %v", len(got), got)
	}
}

// An Anthropic key is reported as one, not as the looser OpenAI pattern that also claims
// its prefix: the family a report names is the one an operator has to go and rotate.
func TestSecretScan_TheMoreSpecificFamilyClaimsAnOverlappingPrefix(t *testing.T) {
	got := ScanContent("x", []byte("k = "+fixtures()["Anthropic API key"]), Families())
	if len(got) != 1 || got[0].Family != "Anthropic API key" {
		t.Fatalf("got %v, want one Anthropic API key finding", got)
	}
}
