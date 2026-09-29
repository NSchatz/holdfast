package config

import (
	"path"
	"testing"
)

// The startup walk's prune decision (S0168), at the one place it is decided: the exclude
// half of a root's path filters, asked of a DIRECTORY. The walk's own cases grade what the
// walk does with the answer; these grade the answer.

// TestS0168AC1_ADirectoryIsExcludedExactlyWhenAnExcludePatternReachesIt is S0168 [AC-1] at
// the decision: a directory strictly beneath a root is excluded when an exclude pattern in
// force for that root matches it or a directory containing it within the root, and in no
// other case - not for the root itself, not for a whole-path near miss, not for a directory
// an include list leaves out.
//
// The second half is the soundness the prune rests on, asked the other way round: every
// directory the decision excludes is one beneath which Offers refuses a file, so a walk that
// does not list it drops nothing the filters would have offered.
func TestS0168AC1_ADirectoryIsExcludedExactlyWhenAnExcludePatternReachesIt(t *testing.T) {
	root := Root{Path: "/srv/media", Clean: "/srv/media", Filters: PathFilters{
		Exclude: []string{"movies/4k", "**/Extras", "**/.Trash-*/**", "/srv/media/import", "movies/tv"},
		Include: []string{"movies/**", "import/**", "a/**", "x/**"},
	}}
	for _, tc := range []struct {
		dir  string
		want bool
	}{
		{"/srv/media/movies/4k", true},          // a relative pattern, the directory itself
		{"/srv/media/movies/4k/remux", true},    // and beneath it, through the ancestor
		{"/srv/media/a/b/Extras", true},         // a name at any depth
		{"/srv/media/a/b/Extras/deep", true},    // and beneath it
		{"/srv/media/.Trash-0", true},           // a trailing /** reaches the bare directory
		{"/srv/media/x/.Trash-1000/y", true},    // at depth, and beneath it
		{"/srv/media/import", true},             // an absolute pattern
		{"/srv/media/import/new", true},         // and beneath it
		{"/srv/media/movies/tv", true},          // the whole path
		{"/srv/media/movies/tv-archive", false}, // and never a substring of it
		{"/srv/media/movies", false},            // an ancestor of an excluded directory is not excluded
		{"/srv/media/tv", false},                // outside every include pattern: include never prunes
		{"/srv/media", false},                   // the root itself is never reached
		{"/srv/other/movies/4k", false},         // a path under another tree is nobody's business here
	} {
		if got := root.ExcludesDirectory(tc.dir); got != tc.want {
			t.Errorf("ExcludesDirectory(%s) = %v, want %v", tc.dir, got, tc.want)
		}
		if !tc.want {
			continue
		}
		for _, file := range []string{path.Join(tc.dir, "f.mkv"), path.Join(tc.dir, "deeper", "g.mkv")} {
			if root.Offers(file) {
				t.Errorf("%s is excluded as a directory, yet Offers(%s) accepts a file beneath it: "+
					"pruning it would drop a file the filters offer", tc.dir, file)
			}
		}
	}
	// Anti-vacuity for the soundness half: a file under a directory the decision leaves
	// listed is still one Offers accepts where the lists say so.
	if !root.Offers("/srv/media/movies/tv-archive/b.mkv") {
		t.Fatal("the fixture's filters offer nothing at all, so the soundness half above proves nothing")
	}

	// A pattern naming the ROOT keeps every file out and still never reaches the root itself.
	named := Root{Path: "/srv/media", Clean: "/srv/media", Filters: PathFilters{Exclude: []string{"/srv/media"}}}
	if named.ExcludesDirectory("/srv/media") || !named.ExcludesDirectory("/srv/media/tv") {
		t.Fatalf("a pattern naming the root: root excluded = %v, child excluded = %v; want false and true",
			named.ExcludesDirectory("/srv/media"), named.ExcludesDirectory("/srv/media/tv"))
	}
	// And a root with no exclude list prunes nothing, whatever its include list says.
	includeOnly := Root{Path: "/srv/media", Clean: "/srv/media", Filters: PathFilters{Include: []string{"movies/**"}}}
	if includeOnly.ExcludesDirectory("/srv/media/tv") {
		t.Fatal("an include list with no exclude list pruned a directory")
	}
}

// TestS0168AC1_TheRootAScanAssignsAPathToIsTheRootWhoseFiltersDecide is S0168 [AC-1]'s
// "the root that path is assigned to": the FIRST configured root containing the directory,
// which is the rule the engine applies to every file it enumerates. Nested roots are refused
// by Validate; the decision is asked about them anyway, because agreeing with the scan must
// not depend on a refusal that ran somewhere else.
func TestS0168AC1_TheRootAScanAssignsAPathToIsTheRootWhoseFiltersDecide(t *testing.T) {
	outer := Root{Path: "/srv/media", Clean: "/srv/media", Filters: PathFilters{Exclude: []string{"sub"}}}
	inner := Root{Path: "/srv/media/sub", Clean: "/srv/media/sub"}
	tv := Root{Path: "/srv/tv", Clean: "/srv/tv"}

	outerFirst := (&Config{Roots: []Root{outer, inner, tv}}).DirectoryExcluded()
	for dir, want := range map[string]bool{
		"/srv/media/sub":   true, // assigned to the outer root, whose pattern names it
		"/srv/media/sub/x": true, // and beneath it
		"/srv/media/other": false,
		"/srv/tv/sub":      false, // another root's own patterns decide there, and it has none
		"/srv/elsewhere/x": false, // under no root at all
	} {
		if got := outerFirst(dir); got != want {
			t.Errorf("outer root first: excluded(%s) = %v, want %v", dir, got, want)
		}
	}

	innerFirst := (&Config{Roots: []Root{inner, outer, tv}}).DirectoryExcluded()
	for dir, want := range map[string]bool{
		"/srv/media/sub":   false, // assigned to the inner root, which is never reached itself
		"/srv/media/sub/x": false, // and whose own filters exclude nothing
		"/srv/media/other": false,
	} {
		if got := innerFirst(dir); got != want {
			t.Errorf("inner root first: excluded(%s) = %v, want %v", dir, got, want)
		}
	}

	// A configuration assembled by hand resolves one root per library root with the
	// top-level lists, exactly as RootProfiles does for every other reader.
	byHand := (&Config{LibraryRoots: []string{"/srv/media"}, ExcludePaths: []string{"**/Extras"}}).DirectoryExcluded()
	if !byHand("/srv/media/film/Extras") || byHand("/srv/media/film") {
		t.Fatalf("the top-level exclude list does not decide for a hand-assembled configuration: "+
			"Extras excluded = %v, film excluded = %v", byHand("/srv/media/film/Extras"), byHand("/srv/media/film"))
	}
}

// TestS0168AC12_AMalformedPatternNeverPrunes is S0168 [AC-12] at the decision: a pattern
// that is not well formed never excludes a directory, even where the matcher would answer
// yes. It is not hypothetical: the pinned doublestar (v4.10.0) matches the malformed
// `{.Trash-0,[}` against `.Trash-0` with no error, so a decision that trusted the matcher's
// answer would prune on a pattern Validate refuses. The case says so in its log if a later
// matcher stops answering yes, because the guard is then no longer what it exercises.
func TestS0168AC12_AMalformedPatternNeverPrunes(t *testing.T) {
	const malformed = "{.Trash-0,[}"
	if validPattern(malformed) {
		t.Fatalf("%q is well formed in this build, so this case proves nothing", malformed)
	}
	if !matchPattern(malformed, ".Trash-0") {
		t.Logf("the matcher no longer answers yes for %q; the guard below is still what decides", malformed)
	}

	root := Root{Path: "/srv/media", Clean: "/srv/media", Filters: PathFilters{
		Exclude: []string{malformed, "**/Extras"},
	}}
	if root.ExcludesDirectory("/srv/media/.Trash-0") {
		t.Fatal("a malformed pattern pruned a directory: a pattern that cannot be read must list it")
	}
	// The well-formed pattern beside it still decides, so the answer above is the guard and
	// not a decision that prunes nothing at all.
	if !root.ExcludesDirectory("/srv/media/film/Extras") {
		t.Fatal("a well-formed pattern beside a malformed one no longer prunes")
	}

	// And the configuration carrying it is one Validate refuses, which is what makes meeting
	// it at the walk the "without passing validation" case.
	cfg := Config{LibraryRoots: []string{"/srv/media"}, ExcludePaths: []string{malformed}}
	if err := cfg.validateFilters(); err == nil {
		t.Fatalf("a configuration carrying %q passed the filter validation", malformed)
	}
}
