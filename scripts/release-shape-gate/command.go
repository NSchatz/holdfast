package main

// Reading a `run:` script the way the SHELL reads it, and deciding where each command in it
// sends what it builds.
//
// This is the `run:` half of the act catalogue, and it is shaped like the `uses:` half in
// workflow.go on purpose, because both halves answer the same question and both have the
// same way of being wrong.
//
// The wrong way is silence. A catalogue of command SPELLINGS answers "does this step
// publish?" with either yes or nothing at all, and nothing reads as no: `docker push` was
// modelled and `docker image push` - the management-command form of the same thing - was
// not, `--push` was modelled and `--output=type=registry` - which buildx documents `--push`
// as shorthand FOR - was not. Each of those is a dry run pushing an image to a public
// registry while the gate prints "NONE of them publishes anything" and exits 0. Adding the
// missing spelling closes the spelling, never the class, and the spelling after that is
// always the one nobody wrote a pattern for.
//
// So this half has a DESTINATION MODEL and an EDGE, which is what the `uses:` half already
// has:
//
//   - The destination model. A build's destination is read from the flags that SET it -
//     `--push`, `--load`, `--output=<spec>`, `-o <spec>` - and each `<spec>` goes through
//     decideBuildxOutputs, the very same function that decides a `docker/build-push-action`
//     step's `outputs:` input. One model of what reaches a registry, two spellings of it.
//     A local exporter (`type=docker,dest=…`) is still local, so this is not "red every
//     --output=".
//   - The edge. registryTools is the set of programs that can reach a registry, a remote or
//     a package index. EVERY invocation of one has to land on a rule in commandRules -
//     either a published act with its kind, or a checked-and-local entry with the reason a
//     human wrote down. An invocation on neither list is UNDECIDED, and undecided reds by
//     name. `crane push`, `crane copy` and `regctl image copy` appear nowhere in this file
//     and every one of them reds, which is the difference between a catalogue with an edge
//     and a catalogue that has to grow a row per spelling.
//
// The boundary this draws, stated rather than implied: a program OUTSIDE registryTools
// performs no published act on its own, and a script it runs (`./scripts/smoke-image.sh`)
// is out of this gate's reach by construction. That is the same boundary shell.go already
// bets on - it stubs exactly these programs before executing any step, because these are
// the ones that could reach a registry - and the two lists are the same list, asserted by a
// test, so a tool worth neutralising is a tool worth deciding.

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
)

// --- what the shell would run ---------------------------------------------------------

// Command is one command a `run:` script executes: the words the shell would hand the
// program, with quoting removed and redirections dropped.
type Command struct {
	Words []string
}

// String renders the command as the gate read it, which is what every message about it
// prints - a reader has to be able to see the thing the gate decided about.
func (c Command) String() string {
	return strings.Join(strings.Fields(strings.Join(c.Words, " ")), " ")
}

// ShellCommands reads a `run:` script as the shell reads it and returns the commands it
// would execute, in order.
//
// It is ONE pass, and that is load-bearing. The previous shape was two - strip comments per
// physical line, then join continuations - and the two passes disagreed about what a line
// is: the stripper reset its quote state at every physical line while the joiner erased
// exactly those line endings. A `#` inside a quoted argument on a continuation line was
// therefore read as a comment, the rest of the LOGICAL line was deleted including the
// trailing backslash, and a `--push` below it became unreachable. Quote state has to survive
// a continuation, because after that join a continuation is not a line ending; the way to
// guarantee that is to have one reader, not two.
//
// What it models, and why each rule is the shell's:
//
//   - `\` before a newline is a continuation, outside quotes and inside double quotes alike,
//     and the pair vanishes with NOTHING in its place - `push\` + `foo` is the single token
//     `pushfoo`, which is not a push. Backslash parity falls out of this rather than being a
//     rule: `\\` is an escaped literal backslash, so the newline after it ends the command.
//   - Single quotes take everything literally; double quotes take everything but `\`, `$`
//     and a backtick literally. A `#` inside either is text, not a comment.
//   - `#` starts a comment only at the start of a word, which is where the shell starts one.
//   - `$(…)` and backticks are lexed as scripts in their own right, so a command hidden in a
//     substitution is still a command this gate has to decide.
//
// An unterminated quote is the one case where the shell and this reader must part company.
// The shell would refuse the whole script, so its content cannot be trusted to be inert -
// and reading it as inert is the fail-open direction, since a `--push` sitting inside an
// accidentally unclosed string would then perform no act. A quote that is never closed is
// therefore not treated as a quote at all: the scan restarts with that one quote character
// demoted to ordinary text. The gate then reads MORE commands than the shell would run,
// which can add an act or an error and can hide neither.
func ShellCommands(script string) []Command {
	demoted := map[int]bool{}
	for attempt := 0; attempt <= len(script); attempt++ {
		cmds, open := lexScript(script, demoted)
		if open < 0 || demoted[open] {
			return cmds
		}
		demoted[open] = true
	}
	cmds, _ := lexScript(script, demoted)
	return cmds
}

// lexScript is one pass of the reader. It returns the commands it read and, when a quote was
// opened and never closed, the byte offset of that quote so the caller can demote it.
func lexScript(src string, demoted map[int]bool) (cmds []Command, unterminated int) {
	unterminated = -1
	var (
		words []string
		cur   strings.Builder
		has   bool
	)
	endWord := func() {
		if has {
			words = append(words, cur.String())
			cur.Reset()
			has = false
		}
	}
	endCommand := func() {
		endWord()
		if len(words) > 0 {
			if kept := dropRedirections(words); len(kept) > 0 {
				cmds = append(cmds, Command{Words: kept})
			}
			words = nil
		}
	}

	atWordStart := true
	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == '\\':
			switch {
			case i+1 < len(src) && src[i+1] == '\n':
				i += 2
			case i+2 < len(src) && src[i+1] == '\r' && src[i+2] == '\n':
				i += 3
			case i+1 < len(src):
				cur.WriteByte(src[i+1])
				has, atWordStart = true, false
				i += 2
			default:
				i++
			}

		case c == '\'' && !demoted[i]:
			j := strings.IndexByte(src[i+1:], '\'')
			if j < 0 {
				return cmds, i
			}
			cur.WriteString(src[i+1 : i+1+j])
			has, atWordStart = true, false
			i += j + 2

		case c == '"' && !demoted[i]:
			next, nested, closed := lexDoubleQuoted(src, i, &cur)
			if !closed {
				return cmds, i
			}
			cmds = append(cmds, nested...)
			has, atWordStart = true, false
			i = next

		case c == '$' && i+1 < len(src) && src[i+1] == '(':
			end, inner, ok := readBalanced(src, i+1)
			if !ok {
				cur.WriteByte(c)
				has, atWordStart = true, false
				i++
				break
			}
			cmds = append(cmds, ShellCommands(inner)...)
			cur.WriteString("$(...)")
			has, atWordStart = true, false
			i = end

		case c == '`':
			j := strings.IndexByte(src[i+1:], '`')
			if j < 0 {
				cur.WriteByte(c)
				has, atWordStart = true, false
				i++
				break
			}
			cmds = append(cmds, ShellCommands(src[i+1:i+1+j])...)
			cur.WriteString("`...`")
			has, atWordStart = true, false
			i += j + 2

		case c == '#' && atWordStart:
			for i < len(src) && src[i] != '\n' {
				i++
			}

		case c == '&':
			switch {
			case i+1 < len(src) && src[i+1] == '&':
				endCommand()
				atWordStart = true
				i += 2
			case i+1 < len(src) && src[i+1] == '>':
				cur.WriteString("&>")
				has, atWordStart = true, false
				i += 2
			case has && endsWithRedirectionOperator(cur.String()):
				cur.WriteByte('&')
				i++
			default:
				endCommand()
				atWordStart = true
				i++
			}

		case c == '|':
			endCommand()
			atWordStart = true
			if i+1 < len(src) && src[i+1] == '|' {
				i += 2
			} else {
				i++
			}

		case c == '\n' || c == ';' || c == '(' || c == ')':
			endCommand()
			atWordStart = true
			i++

		case c == ' ' || c == '\t' || c == '\r':
			endWord()
			atWordStart = true
			i++

		default:
			cur.WriteByte(c)
			has, atWordStart = true, false
			i++
		}
	}
	endCommand()
	return cmds, -1
}

// lexDoubleQuoted consumes a double-quoted span, appending its text to cur. Inside double
// quotes the shell still honours `\` (before `"`, `\`, `$`, a backtick or a newline) and
// still expands `$(…)`; everything else, `#` and a raw newline included, is text.
func lexDoubleQuoted(src string, start int, cur *strings.Builder) (next int, nested []Command, closed bool) {
	i := start + 1
	for i < len(src) {
		c := src[i]
		switch {
		case c == '\\' && i+1 < len(src):
			switch n := src[i+1]; {
			case n == '\n':
				i += 2
			case n == '\r' && i+2 < len(src) && src[i+2] == '\n':
				i += 3
			case n == '"' || n == '\\' || n == '$' || n == '`':
				cur.WriteByte(n)
				i += 2
			default:
				cur.WriteByte(c)
				i++
			}
		case c == '"':
			return i + 1, nested, true
		case c == '$' && i+1 < len(src) && src[i+1] == '(':
			end, inner, ok := readBalanced(src, i+1)
			if !ok {
				cur.WriteByte(c)
				i++
				continue
			}
			nested = append(nested, ShellCommands(inner)...)
			cur.WriteString("$(...)")
			i = end
		case c == '`':
			j := strings.IndexByte(src[i+1:], '`')
			if j < 0 {
				cur.WriteByte(c)
				i++
				continue
			}
			nested = append(nested, ShellCommands(src[i+1:i+1+j])...)
			cur.WriteString("`...`")
			i += j + 2
		default:
			cur.WriteByte(c)
			i++
		}
	}
	return len(src), nested, false
}

// readBalanced consumes a `(…)` span starting at open, respecting nesting and quotes, and
// returns the offset just past its `)` together with what was inside it.
func readBalanced(src string, open int) (next int, inner string, ok bool) {
	depth := 0
	for i := open; i < len(src); {
		switch src[i] {
		case '\\':
			i += 2
			continue
		case '\'':
			j := strings.IndexByte(src[i+1:], '\'')
			if j < 0 {
				return 0, "", false
			}
			i += j + 2
			continue
		case '"':
			i++
			for i < len(src) && src[i] != '"' {
				if src[i] == '\\' {
					i++
				}
				i++
			}
			if i >= len(src) {
				return 0, "", false
			}
			i++
			continue
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i + 1, src[open+1 : i], true
			}
		}
		i++
	}
	return 0, "", false
}

// reRedirection matches a word that is (or begins) a redirection rather than an argument.
var reRedirection = regexp.MustCompile(`^([0-9]*(>>|>|<<|<)|&>|[0-9]*[<>]&)`)

// dropRedirections removes a command's redirections, which are plumbing rather than argv:
// `docker image rm -f "$REF" >/dev/null 2>&1` invokes docker with five words, not seven.
func dropRedirections(words []string) []string {
	out := make([]string, 0, len(words))
	for i := 0; i < len(words); i++ {
		w := words[i]
		if m := reRedirection.FindString(w); m != "" {
			if w == m {
				i++ // the operator's target sits in the next word
			}
			continue
		}
		out = append(out, w)
	}
	return out
}

func endsWithRedirectionOperator(s string) bool {
	return strings.HasSuffix(s, ">") || strings.HasSuffix(s, "<")
}

// --- which program a command actually invokes ------------------------------------------

var (
	// shellKeywords never name the program; they sit in front of it.
	shellKeywords = map[string]bool{
		"if": true, "then": true, "elif": true, "else": true, "fi": true,
		"do": true, "done": true, "while": true, "until": true, "for": true,
		"in": true, "case": true, "esac": true, "select": true, "function": true,
		"!": true, "{": true, "}": true, "[[": true, "]]": true, "coproc": true,
	}
	// commandWrappers run another program, so the act belongs to what comes after them.
	// `sudo docker push` is a push.
	commandWrappers = map[string]bool{
		"sudo": true, "env": true, "command": true, "exec": true, "builtin": true,
		"nohup": true, "nice": true, "stdbuf": true, "time": true, "timeout": true,
		"xargs": true, "eval": true,
	}
	reAssignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\[[^]]*\])?\+?=`)
)

// invocation reports the program this command runs, and the words from that program on.
// Leading shell keywords, environment assignments (`CGO_ENABLED=0 go build`) and wrapper
// commands are stepped over, because none of them is the thing being run.
func (c Command) invocation() (prog string, args []string, ok bool) {
	words := c.Words
	for len(words) > 0 {
		w := words[0]
		switch {
		case shellKeywords[w] || reAssignment.MatchString(w):
			words = words[1:]
		case commandWrappers[path.Base(w)]:
			words = words[1:]
			for len(words) > 0 && (strings.HasPrefix(words[0], "-") || reAssignment.MatchString(words[0])) {
				words = words[1:]
			}
		default:
			return path.Base(w), words, true
		}
	}
	return "", nil, false
}

// strayRegistryTool reports a registry tool NAMED by a command whose program is something
// else - `xargs -0 docker push`, or a `sudo` whose option parsing this gate got wrong. The
// invocation cannot then be decided, and an invocation that cannot be decided must red
// rather than fall through as "the program was `xargs`, so nothing publishes".
func (c Command) strayRegistryTool(prog string) string {
	for _, w := range c.Words {
		if strings.ContainsAny(w, " \t\n") {
			continue // a quoted argument, not a program name
		}
		b := path.Base(w)
		if b == prog {
			continue
		}
		if _, ok := registryTools[b]; ok {
			return b
		}
	}
	return ""
}

// --- the catalogue ---------------------------------------------------------------------

// registryTools is the set of programs that can reach a registry, a remote or a package
// index, and therefore the set whose every invocation this gate has to DECIDE. It is the
// same set shell.go stubs before executing any step; stubbedCommands unions it with the
// clients below and a test holds the two in step, because a tool worth neutralising before
// a step runs is a tool worth deciding when the definition is read.
var registryTools = map[string]string{
	"docker":  "the container CLI: `push`, `manifest push` and a build's destination flags all reach a registry",
	"podman":  "docker's command surface, and its `push` reaches a registry the same way",
	"nerdctl": "docker's command surface again, over containerd",
	"buildah": "builds images and pushes them",
	"buildx":  "the standalone builder: its destination flags decide whether a build is published",
	"skopeo":  "copies images between registries",
	"crane":   "reads and WRITES registry content",
	"regctl":  "reads and WRITES registry content",
	"oras":    "pushes OCI artefacts",
	"gh":      "cuts GitHub releases, and its `api` can perform any REST call the token allows",
	"git":     "pushes refs to a remote",
	"npm":     "publishes to a package index",
	"pnpm":    "publishes to a package index",
	"yarn":    "publishes to a package index",
	"cargo":   "publishes to a package index",
	"helm":    "pushes charts to a registry",
	"twine":   "uploads distributions to a package index",
}

// clientsCheckedAndNotInventoried is the honest edge of the edge. A general-purpose HTTP
// client or cloud CLI can upload something, but its argv names an API rather than a
// destination this gate can model, so deciding every invocation of one would mean modelling
// every API - the unbounded catalogue this whole file refuses to become. They are STUBBED
// (nothing this gate executes can reach the network through them) and they are written down
// here with the reason, rather than left as an unstated silence.
var clientsCheckedAndNotInventoried = map[string]string{
	"curl":   "a general-purpose HTTP client; what it uploads is named by a URL and a method, not by a destination this gate can model",
	"wget":   "a general-purpose HTTP client, and its usual direction is a download",
	"aws":    "a cloud CLI whose every service is its own API surface",
	"gcloud": "a cloud CLI whose every service is its own API surface",
	"az":     "a cloud CLI whose every service is its own API surface",
}

// stubbedCommands is the union: everything the gate neutralises on PATH before it executes
// a step's shell. Its single writer is the pair of maps above.
func stubbedCommands() []string {
	out := make([]string, 0, len(registryTools)+len(clientsCheckedAndNotInventoried))
	for name := range registryTools {
		out = append(out, name)
	}
	for name := range clientsCheckedAndNotInventoried {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// dockerFamily are CLIs that take docker's own command surface, so one set of rules decides
// all of them - `podman manifest push` is `docker manifest push`.
var dockerFamily = map[string]bool{"docker": true, "podman": true, "nerdctl": true}

// commandRule classifies ONE invocation of a registry tool: the leading words that select
// it, and either the act it performs (kind, with the reason) or the reason a human checked
// it and found it performs none (an empty kind). decide is for the invocations where the
// FLAGS decide rather than the subcommand.
type commandRule struct {
	words  []string
	kind   ActKind
	what   string
	decide func(c Command, args []string) (ActKind, string, error)
}

// commandRules is the catalogue. Every entry is a question a human answered once, in a file
// under review - which is the point, because the alternative is answering it by omission.
//
// It is deliberately NOT exhaustive over each tool. `crane push`, `crane copy` and
// `regctl image copy` are absent, and every one of them reds by name rather than passing:
// that is what makes this a catalogue with an edge instead of one that owes a row to every
// spelling anybody might use.
var commandRules = []commandRule{
	// --- images reaching a registry
	{words: []string{"docker", "push"}, kind: ActImagePush, what: "docker push - an image reaches a registry"},
	{words: []string{"docker", "image", "push"}, kind: ActImagePush, what: "docker image push - the management-command spelling of docker push"},
	{words: []string{"docker", "manifest", "push"}, kind: ActTagMove, what: "docker manifest push - a manifest list reaches a registry under its own name"},
	{words: []string{"buildah", "push"}, kind: ActImagePush, what: "buildah push - an image reaches a registry"},

	// --- a build, whose destination is decided from the flags that SET it
	{words: []string{"docker", "build"}, decide: decideBuildDestination},
	{words: []string{"docker", "buildx", "build"}, decide: decideBuildDestination},
	{words: []string{"buildx", "build"}, decide: decideBuildDestination},
	{words: []string{"docker", "buildx", "bake"}, decide: decideBakeDestination},
	{words: []string{"buildx", "bake"}, decide: decideBakeDestination},

	// --- a floating reference being pointed somewhere new
	{words: []string{"docker", "buildx", "imagetools", "create"}, kind: ActTagMove, what: "docker buildx imagetools create - a floating reference is pointed somewhere new"},
	{words: []string{"buildx", "imagetools", "create"}, kind: ActTagMove, what: "buildx imagetools create - a floating reference is pointed somewhere new"},
	{words: []string{"crane", "tag"}, kind: ActTagMove, what: "crane tag - a registry tag write"},
	{words: []string{"crane", "index"}, kind: ActTagMove, what: "crane index - a registry index write"},
	{words: []string{"regctl", "tag"}, kind: ActTagMove, what: "regctl tag - a registry tag write"},
	{words: []string{"regctl", "index"}, kind: ActTagMove, what: "regctl index - a registry index write"},

	// --- published releases and refs
	{words: []string{"gh", "release", "create"}, kind: ActRelease, what: "gh release create - a published release object"},
	{words: []string{"gh", "release", "edit"}, kind: ActRelease, what: "gh release edit - a published release object is changed"},
	{words: []string{"gh", "release", "upload"}, kind: ActRelease, what: "gh release upload - an asset is added to a published release"},
	{words: []string{"gh", "release", "delete"}, kind: ActRelease, what: "gh release delete - a published release object is removed"},
	{words: []string{"gh", "api"}, decide: decideGhAPI},
	{words: []string{"git", "push"}, kind: ActRefPush, what: "git push - a ref reaches the remote"},

	// --- packages and artefacts
	{words: []string{"npm", "publish"}, kind: ActArtefact, what: "npm publish"},
	{words: []string{"pnpm", "publish"}, kind: ActArtefact, what: "pnpm publish"},
	{words: []string{"yarn", "publish"}, kind: ActArtefact, what: "yarn publish"},
	{words: []string{"cargo", "publish"}, kind: ActArtefact, what: "cargo publish"},
	{words: []string{"helm", "push"}, kind: ActArtefact, what: "helm push"},
	{words: []string{"twine", "upload"}, kind: ActArtefact, what: "twine upload"},
	{words: []string{"skopeo", "copy"}, kind: ActArtefact, what: "skopeo copy - a registry copy/push"},
	{words: []string{"skopeo", "sync"}, kind: ActArtefact, what: "skopeo sync - a registry copy/push"},
	{words: []string{"oras", "copy"}, kind: ActArtefact, what: "oras copy - a registry copy/push"},
	{words: []string{"oras", "push"}, kind: ActArtefact, what: "oras push - an OCI artefact reaches a registry"},

	// --- checked and found to perform no published act. Each of these is the `run:` half's
	// --- classifiedLocalActions: a reason a human wrote down, not an omission.
	{words: []string{"docker", "pull"}, what: "reads an image FROM a registry"},
	{words: []string{"docker", "image", "pull"}, what: "reads an image FROM a registry"},
	{words: []string{"docker", "buildx", "imagetools", "inspect"}, what: "reads a manifest from the registry"},
	{words: []string{"buildx", "imagetools", "inspect"}, what: "reads a manifest from the registry"},
	{words: []string{"docker", "manifest", "create"}, what: "assembles a manifest list in the LOCAL manifest store; `manifest push` is what sends it"},
	{words: []string{"docker", "manifest", "annotate"}, what: "annotates a manifest list in the LOCAL manifest store"},
	{words: []string{"docker", "manifest", "inspect"}, what: "reads a manifest list"},
	{words: []string{"docker", "tag"}, what: "names an image inside the LOCAL daemon; a push is what sends it"},
	{words: []string{"docker", "image", "tag"}, what: "names an image inside the LOCAL daemon; a push is what sends it"},
	{words: []string{"docker", "image", "rm"}, what: "removes an image from the LOCAL daemon"},
	{words: []string{"docker", "image", "prune"}, what: "removes images from the LOCAL daemon"},
	{words: []string{"docker", "image", "ls"}, what: "lists local images"},
	{words: []string{"docker", "image", "inspect"}, what: "reads a local image"},
	{words: []string{"docker", "image", "save"}, what: "writes a local image to a tar"},
	{words: []string{"docker", "image", "load"}, what: "reads an image from a local tar"},
	{words: []string{"docker", "rmi"}, what: "removes an image from the LOCAL daemon"},
	{words: []string{"docker", "images"}, what: "lists local images"},
	{words: []string{"docker", "save"}, what: "writes a local image to a tar"},
	{words: []string{"docker", "load"}, what: "reads an image from a local tar"},
	{words: []string{"docker", "run"}, what: "runs a container on this machine"},
	{words: []string{"docker", "exec"}, what: "runs a process in a local container"},
	{words: []string{"docker", "create"}, what: "creates a local container"},
	{words: []string{"docker", "start"}, what: "starts a local container"},
	{words: []string{"docker", "stop"}, what: "stops a local container"},
	{words: []string{"docker", "rm"}, what: "removes a local container"},
	{words: []string{"docker", "ps"}, what: "lists local containers"},
	{words: []string{"docker", "logs"}, what: "reads a local container's logs"},
	{words: []string{"docker", "cp"}, what: "copies files in or out of a local container"},
	{words: []string{"docker", "inspect"}, what: "reads local state"},
	{words: []string{"docker", "version"}, what: "prints a version"},
	{words: []string{"docker", "info"}, what: "prints local daemon information"},
	{words: []string{"docker", "login"}, what: "authenticates to a registry; a credential is not a publish, and every push it enables is decided on its own command"},
	{words: []string{"docker", "logout"}, what: "discards a credential"},
	{words: []string{"docker", "buildx", "create"}, what: "creates a local builder instance"},
	{words: []string{"docker", "buildx", "use"}, what: "selects a local builder instance"},
	{words: []string{"docker", "buildx", "rm"}, what: "removes a local builder instance"},
	{words: []string{"docker", "buildx", "prune"}, what: "prunes the local build cache"},
	{words: []string{"docker", "buildx", "version"}, what: "prints a version"},
	{words: []string{"docker", "compose", "config"}, what: "validates a compose file"},
	{words: []string{"docker", "compose", "pull"}, what: "reads images FROM a registry"},
	{words: []string{"docker", "compose", "up"}, what: "runs containers on this machine"},
	{words: []string{"docker", "compose", "down"}, what: "stops containers on this machine"},
	{words: []string{"gh", "release", "view"}, what: "reads a release"},
	{words: []string{"gh", "release", "list"}, what: "lists releases"},
	{words: []string{"gh", "release", "download"}, what: "downloads release assets"},
	{words: []string{"gh", "auth", "login"}, what: "authenticates; a credential is not a publish"},
	{words: []string{"gh", "auth", "status"}, what: "reports whether a credential works"},
	{words: []string{"git", "clone"}, what: "reads a repository"},
	{words: []string{"git", "fetch"}, what: "reads refs from the remote"},
	{words: []string{"git", "ls-remote"}, what: "reads refs from the remote"},
	{words: []string{"git", "checkout"}, what: "moves the local working tree"},
	{words: []string{"git", "switch"}, what: "moves the local working tree"},
	{words: []string{"git", "add"}, what: "stages a local change"},
	{words: []string{"git", "commit"}, what: "writes a commit to the LOCAL repository"},
	{words: []string{"git", "tag"}, what: "creates a tag in the LOCAL repository; `git push` is what sends it"},
	{words: []string{"git", "config"}, what: "reads or writes local configuration"},
	{words: []string{"git", "remote"}, what: "reads or writes the local remote list"},
	{words: []string{"git", "rev-parse"}, what: "reads local state"},
	{words: []string{"git", "rev-list"}, what: "reads local state"},
	{words: []string{"git", "for-each-ref"}, what: "reads local state"},
	{words: []string{"git", "describe"}, what: "reads local state"},
	{words: []string{"git", "log"}, what: "reads local state"},
	{words: []string{"git", "show"}, what: "reads local state"},
	{words: []string{"git", "diff"}, what: "reads local state"},
	{words: []string{"git", "status"}, what: "reads local state"},
	{words: []string{"npm", "ci"}, what: "installs dependencies"},
	{words: []string{"npm", "install"}, what: "installs dependencies"},
	{words: []string{"npm", "run"}, what: "runs a package script"},
	{words: []string{"npm", "test"}, what: "runs a package's tests"},
	{words: []string{"pnpm", "install"}, what: "installs dependencies"},
	{words: []string{"pnpm", "run"}, what: "runs a package script"},
	{words: []string{"yarn", "install"}, what: "installs dependencies"},
	{words: []string{"cargo", "build"}, what: "builds locally"},
	{words: []string{"cargo", "test"}, what: "runs tests locally"},
	{words: []string{"helm", "lint"}, what: "reads a chart"},
	{words: []string{"helm", "template"}, what: "renders a chart locally"},
}

// Act reports the published act this command performs, the reason, or an error when the
// invocation cannot be decided. An error is never an omission: returning no act for a
// command this gate does not understand is how "NONE of them publishes anything" gets
// printed over a dry run that pushes.
func (c Command) Act() (ActKind, string, error) {
	prog, args, ok := c.invocation()
	if !ok {
		return "", "", nil
	}
	if _, decided := registryTools[prog]; !decided {
		if stray := c.strayRegistryTool(prog); stray != "" {
			return "", "", fmt.Errorf("runs `%s`, whose program is `%s` but which NAMES `%s` - a tool that can reach a registry.\nThis gate decides an act from the program a command invokes, and it cannot tell whether `%s` runs here. Spell the invocation so the tool is the program (`%s …`), or the act it performs cannot be inventoried at all",
				c.String(), prog, stray, stray, stray)
		}
		return "", "", nil
	}

	lead := leadingWords(args)
	best := -1
	for i, r := range commandRules {
		if !hasWordPrefix(lead, r.words) {
			continue
		}
		if best < 0 || len(r.words) > len(commandRules[best].words) {
			best = i
		}
	}
	if best < 0 {
		return "", "", fmt.Errorf("runs `%s`, and this gate does not model that invocation of `%s` (%s).\nEvery command that invokes a tool which can reach a registry, a remote or a package index has to land on a rule in commandRules in scripts/release-shape-gate/command.go: one that names the published act it performs, or one that records the reason a human checked it and found it performs none. An invocation on NEITHER is undecided, which is not the same as harmless - `crane push`, `crane copy` and `regctl image copy` all publish, and answering silence with \"no\" is how an unreviewed publish ships. Add it to the catalogue, with the reason",
			c.String(), prog, registryTools[prog])
	}
	r := commandRules[best]
	if r.decide != nil {
		return r.decide(c, args)
	}
	return r.kind, r.what, nil
}

// leadingWords is the subcommand path a rule matches against: the program, then the run of
// words before the first flag. `docker image push ghcr.io/o/r:dev` selects on
// `docker image push`; `docker buildx build --platform …` selects on `docker buildx build`.
func leadingWords(args []string) []string {
	prog := path.Base(args[0])
	if dockerFamily[prog] {
		prog = "docker"
	}
	out := []string{prog}
	for _, w := range args[1:] {
		if w == "" || strings.HasPrefix(w, "-") {
			break
		}
		out = append(out, w)
	}
	return out
}

func hasWordPrefix(words, prefix []string) bool {
	if len(words) < len(prefix) {
		return false
	}
	for i, p := range prefix {
		if words[i] != p {
			return false
		}
	}
	return true
}

// decideBuildDestination decides where a build writes what it built, from the flags that set
// the destination - and it decides an `--output` specification through the SAME function
// that decides a `docker/build-push-action` step's `outputs:` input, because they are one
// model with two spellings.
//
// Fail-closed at every branch, in exactly the ways the `uses:` half is:
//
//   - `--push` publishes. buildx documents it as shorthand for `--output=type=registry`, so
//     the two are the same act and both are here.
//   - `--load` is `--output=type=docker`: a LOCAL load, and it must stay local. A fix that
//     called every `--output=` a publish would be the false positive this gate refuses as
//     hard as it refuses the silence.
//   - no destination flag at all: the build stays where buildx put it, which is not a
//     published act.
//   - an `--output` whose exporter this gate does not model, whose `type=` is missing, or
//     which cannot be read as attributes: an ERROR that names the specification.
func decideBuildDestination(c Command, args []string) (ActKind, string, error) {
	for i := 1; i < len(args); i++ {
		w := args[i]
		var spec string
		switch {
		case w == "--push":
			return ActImagePush, "`--push`, which buildx documents as shorthand for `--output=type=registry`", nil
		case strings.HasPrefix(w, "--push="):
			switch v := strings.ToLower(strings.TrimPrefix(w, "--push=")); v {
			case "true":
				return ActImagePush, "`" + w + "`, which buildx documents as shorthand for `--output=type=registry`", nil
			case "false":
				continue
			default:
				return "", "", fmt.Errorf("runs `%s`, whose `%s` is not a boolean, so whether the build reaches a registry is unknown", c.String(), w)
			}
		case w == "--load" || w == "--load=true" || w == "--load=false":
			continue // `--load` IS `--output=type=docker`: a local load
		case w == "--output" || w == "-o":
			if i+1 >= len(args) {
				return "", "", fmt.Errorf("runs `%s`, whose `%s` names no destination, so where the build is written is unknown", c.String(), w)
			}
			spec = args[i+1]
			i++
		case strings.HasPrefix(w, "--output="):
			spec = strings.TrimPrefix(w, "--output=")
		case strings.HasPrefix(w, "-o="):
			spec = strings.TrimPrefix(w, "-o=")
		case w == "--set" || strings.HasPrefix(w, "--set="):
			return "", "", fmt.Errorf("runs `%s`, and `--set` can rewrite a target's own output destination. Where this build is written is therefore decided somewhere this gate does not read, so it will not report that the build publishes nothing", c.String())
		default:
			continue
		}
		publishes, detail, err := decideBuildxOutputs("--output", spec)
		if err != nil {
			return "", "", fmt.Errorf("runs `%s`: %w", c.String(), err)
		}
		if publishes {
			return ActImagePush, detail, nil
		}
	}
	return "", "", nil
}

// decideBakeDestination is decideBuildDestination's fail-closed cousin. A bake target names
// its own output in the bake FILE, which this gate does not read, so a bake with no
// destination flag on the command line is undecided rather than local.
func decideBakeDestination(c Command, args []string) (ActKind, string, error) {
	kind, detail, err := decideBuildDestination(c, args)
	if err != nil || kind != "" {
		return kind, detail, err
	}
	return "", "", fmt.Errorf("runs `%s`, and a bake target's output destination lives in the bake definition, which this gate does not read. A bake with no destination on the command line is therefore undecided, not local - `docker buildx bake --push` is the spelling this gate can decide", c.String())
}

// decideGhAPI. `gh api` can perform any REST call the token allows, so the subcommand says
// nothing about the act; only the path does. A call against /releases creates or changes a
// published release. Anything else is undecided rather than harmless.
func decideGhAPI(c Command, args []string) (ActKind, string, error) {
	for _, w := range args[1:] {
		if strings.Contains(w, "/releases") {
			return ActRelease, "a gh api call against /releases", nil
		}
	}
	return "", "", fmt.Errorf("runs `%s`. `gh api` performs whatever REST call its path and method name, and this gate cannot decide from the argv alone whether that publishes. Spell the act as the `gh` subcommand that performs it, or classify this call in commandRules with the reason", c.String())
}
