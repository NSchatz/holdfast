//go:build regress0046

package main

// F14c and F14r, kept beside regress_0046_F14_test.go.
//
// F14c is the CLASS F14 and F14b are two instances of. Every control the
// observation environment has lives INSIDE the shell it is observing. The
// recorder is a shell function, the guard is a DEBUG trap, the empty PATH is an
// exported variable, and the descent into a repository script is a mirror the
// working directory points at. None of them is out of the observed step's reach,
// so a `run:` script written to defeat the harness takes them away with one line
// each - and the step's `docker push` is then neither recorded, nor refused, nor
// decided.
//
// The conductor's hard constraint is that "whatever observes execution must be
// safe to run against a workflow written to defeat it", and
// `release-shape-selftest` grades 71 mutated workflows through exactly this
// environment. These lines are recorded so a fix addresses the property rather
// than the `export PATH=` spelling alone.
//
// F14r is what makes F14 and F15 REGRESSIONS rather than newly-found holes: the
// lexical reader this pass moved off the act decision is still in the tree,
// still graded against bash by command_test.go, and it decides both commands
// correctly. The observation route decides neither.

import "testing"

func TestRegress0046L6_F14c_TheObservedScriptCanDismantleTheObserver(t *testing.T) {
	withObserver(t)
	push := "docker push ghcr.io/nschatz/holdfast:dev"
	var silent []string
	for _, disable := range []string{
		"export PATH=/usr/bin:/bin",
		"trap - DEBUG",
		"unset -f command_not_found_handle",
		"command_not_found_handle() { :; }",
		"__rs_invoke() { :; }",
	} {
		run := disable + "\n" + push + "\n"
		step := Step{Name: "probe", Run: run}
		acts, err := step.Acts(nil)
		t.Logf("%-38s -> observed %q acts=%v err=%v", disable, step.script(), acts, err)
		if len(acts) == 0 && err == nil {
			silent = append(silent, disable)
		}
	}
	if len(silent) > 0 {
		t.Fatalf("%d line(s) put the step's `docker push` beyond every part of the observation - not recorded, not refused, not decided: %q", len(silent), silent)
	}
}

// F14r. The same two scripts, decided by the LEXICAL reader that used to be the
// act route. Both are acts there. So each of F14 and F15 is a spelling the
// previous ordinal's code caught and this one does not.
func TestRegress0046L6_F14r_TheLexicalReaderStillDecidesWhatTheObservationMisses(t *testing.T) {
	withObserver(t)
	for _, tc := range []struct {
		what string
		run  string
	}{
		{"F14", "export PATH=/usr/bin:/bin\ndocker push ghcr.io/nschatz/holdfast:dev\n"},
		{"F15", "exec docker push ghcr.io/nschatz/holdfast:dev\n"},
	} {
		lexical := ""
		for _, c := range ShellCommands(tc.run) {
			kind, why, err := c.Act()
			if kind != "" {
				lexical = string(kind) + " (" + why + ")"
			} else if err != nil {
				lexical = "refusal: " + err.Error()
			}
		}
		acts, err := (Step{Name: "probe", Run: tc.run}).Acts(nil)
		t.Logf("%s: the reader says %q; the observation says acts=%v err=%v", tc.what, lexical, acts, err)
		if lexical != "" && len(acts) == 0 && err == nil {
			t.Errorf("%s: the lexical reader in this same package decides this command (%s) and the observed route - the one A6 is now graded through - reports nothing at all", tc.what, lexical)
		}
	}
}
