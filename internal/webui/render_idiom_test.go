package webui

// The mechanical guard on probePage's rule, which cost this repository three CI runs
// before it was one.
//
// Every grader in this package measures a page that holds an SSE stream open. Reading the
// document out of the browser's own exit - `--dump-dom`, and `--virtual-time-budget` to
// decide when to take it - hands the browser the decision about WHEN the measurement
// exists, and with a fetch permanently pending that decision may never come: virtual time
// cannot advance while one is outstanding, and the dashboard's own EventSource opens a new
// one the instant the stream ends. Two runs were lost to exactly that. The third was eight
// months later, in the one grader that had kept a driver of its own, by which time the
// reason lived only in a comment on the file that had already been fixed - which is what a
// rule nothing enforces decays into.
//
// The rule, stated once: a grader's measurement comes back as a verdict the page POSTs
// (probePage + runProbe in rendered_test.go), or over the graders' own engine driver
// (internal/webui/e2e/driver.mjs, reached from Go through engine_test.go). Either way the
// TEST owns the deadline.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// retiredDriverFlag is every way of asking the browser to decide when a measurement is
// available. They are named here, in the file that FORBIDS them, which is why this file is
// the one the sweep below cannot ask about itself.
var retiredDriverFlags = []string{"--dump-dom", "--virtual-time-budget"}

// guardFile is this file, exempt from the sweep because its own literals ARE the rule
// rather than a use of it. The exemption is not a hiding place and the second half of the
// test is what keeps it from becoming one: this file may name the flags and may not drive
// a browser, which is asked of its syntax rather than of its text.
const guardFile = "render_idiom_test.go"

func TestRenderIdiom_NoGraderWaitsOnTheBrowserToDecideItIsDone(t *testing.T) {
	names, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(names) < 5 {
		t.Fatalf("the sweep found only %d test files in this package; it must cover every grader", len(names))
	}
	swept := 0
	for _, name := range names {
		if name == guardFile {
			continue
		}
		swept++
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		for _, flag := range driverFlagsIn(string(src)) {
			t.Errorf("%s drives the browser with %q. A grader's measurement has to come back as a verdict "+
				"the page POSTs (probePage/runProbe) or over the graders' own engine driver "+
				"(engine_test.go): a page holding an SSE stream open never lets the browser decide it is "+
				"done, and three CI runs were killed at the deadline proving it", name, flag)
		}
	}
	if swept < 5 {
		t.Fatalf("the sweep looked at only %d files besides itself", swept)
	}

	// The exemption, held: the file that names the flags must not be able to USE one.
	own, err := os.ReadFile(guardFile)
	if err != nil {
		t.Fatalf("reading %s: %v", guardFile, err)
	}
	if calls := processCalls(string(own)); len(calls) > 0 {
		t.Errorf("%s starts a process (%s). It is exempt from the sweep because it names the retired "+
			"flags; it may not also be the file that uses one", guardFile, strings.Join(calls, ", "))
	}

	// The sweep must BITE. A source passing one of these flags in a string literal has to be
	// reported however it is spelled, and prose stating the rule must not be.
	for _, bad := range []string{
		"package p\nvar a = []string" + "{" + `"--dump-dom"` + "}\n",
		"package p\nvar a = \"--virtual-time-budget=6000\"\n",
		"package p\nfunc f() { run(b, \"--headless\", \"--dump-dom\", u) }\n",
	} {
		if len(driverFlagsIn(bad)) == 0 {
			t.Errorf("the sweep passed %q, so it cannot fail", bad)
		}
	}
	prose := "package p\n// this package must never take a reading out of --dump-dom under --virtual-time-budget\n"
	if got := driverFlagsIn(prose); len(got) != 0 {
		t.Errorf("the sweep reported the prose that states the rule (%v); the reason has to be writable beside it", got)
	}
	// And the exemption's own half must bite: a file that starts a process is reported.
	if got := processCalls("package p\nfunc f() { exec.Command(\"x\").Run() }\n"); len(got) == 0 {
		t.Error("the process check passed a source that calls exec.Command, so it cannot fail")
	}
	if got := processCalls("package p\n// exec.Command is what this file forbids\nvar s = \"exec.Command\"\n"); len(got) != 0 {
		t.Errorf("the process check reported a mention rather than a call (%v)", got)
	}
}

// driverFlagsIn reports every retired browser-driving flag appearing in a STRING LITERAL
// of src. It parses rather than greps precisely so that a comment naming a flag - which is
// where the reason for the rule lives - is not the same act as using it.
func driverFlagsIn(src string) []string {
	var found []string
	inspect(src, &found, func(v string, out *[]string) {
		for _, flag := range retiredDriverFlags {
			if strings.Contains(v, flag) {
				*out = append(*out, flag)
			}
		}
	}, nil)
	return found
}

// processCalls reports every os/exec call site in src. A mention in a comment or a string
// is not one: the question is whether this file can START a browser, which only a call can.
func processCalls(src string) []string {
	var found []string
	inspect(src, &found, nil, func(sel string, out *[]string) {
		switch sel {
		case "exec.Command", "exec.CommandContext", "syscall.Exec", "os.StartProcess":
			*out = append(*out, sel)
		}
	})
	return found
}

// inspect walks src's syntax, handing every string literal's VALUE to onString and every
// call's `pkg.Func` selector to onCall. An unparseable source is reported rather than
// silently passing, because a check that cannot read its subject decides nothing.
func inspect(src string, out *[]string, onString func(string, *[]string), onCall func(string, *[]string)) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "src.go", src, parser.SkipObjectResolution)
	if err != nil {
		*out = append(*out, "unparseable: "+err.Error())
		return
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.BasicLit:
			if onString != nil && node.Kind == token.STRING {
				if v, err := strconv.Unquote(node.Value); err == nil {
					onString(v, out)
				}
			}
		case *ast.CallExpr:
			if onCall == nil {
				return true
			}
			sel, ok := node.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			onCall(pkg.Name+"."+sel.Sel.Name, out)
		}
		return true
	})
}
