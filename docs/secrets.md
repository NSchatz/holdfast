# Secrets: a credential is reached by reference

holdfast reads eight credential-bearing configuration keys, and none of them holds a
credential. Each holds a **reference** - the name of a secret and the kind of place it
lives - and the value is resolved at the point of use. That is the whole design, and it
exists because a secret an agent or an operator *read* is a secret in a transcript,
whether or not it ever reached a file.

The eight keys:

| key | what the credential is | what happens when it is absent |
|---|---|---|
| `server_auth_token` | the bearer token the MUTATING API endpoints require | `rescan`, `scan`, `pause`, `resume` answer 403: remote control is off |
| `server_read_token` | the bearer token the READ endpoints under `/api` require | those four endpoints are OPEN, which is the shipped default. They carry the full path of every file holdfast has seen, so on a non-loopback bind that is the whole library served without a credential, and holdfast says so at startup. It does not gate the plain-text root page or `/metrics` |
| `notify_url` | a shoutrrr service URL, which carries its credential in its own userinfo, host, path or query | notifications are off |
| `tautulli_api_key` | the Tautulli API key | the Plex-aware pause is off |
| `radarr_api_key` | the Radarr API key, sent in the `X-Api-Key` header | Radarr is not asked to rescan after a swap |
| `sonarr_api_key` | the Sonarr API key, sent in the `X-Api-Key` header | Sonarr is not asked to rescan after a swap |
| `plex_token` | a Plex **admin** token, sent in the `X-Plex-Token` header | Plex is not asked for a partial scan after a swap, and no file is held for being played |
| `webhook_token` | the credential a Sonarr or Radarr `Connect > Webhook` connection presents to `/api/webhook/sonarr` and `/api/webhook/radarr`, as a bearer token or as the password of HTTP Basic authentication | both intake endpoints answer 403 and queue nothing |

`tautulli_url`, `radarr_url`, `sonarr_url`, `plex_url` and `server_addr` are addresses, not
credentials. The three media-server addresses accept a scheme, a host and an optional base path
only: userinfo, a query or a fragment in one refuses to start, so a credential cannot be pasted
into an address by mistake. Each of those three targets needs its address and its credential
together, and one without the other refuses to start
([docs/post-swap-hook.md](post-swap-hook.md)).

`webhook_token` is the one credential here that holdfast **receives** and an outside service
holds. It is a key of its own so that the secret handed to an arr is the least privileged one
there is: it authorises the two intake endpoints and nothing else, the control token and the read
token are not accepted on those endpoints, a `webhook_token` written as the same reference as
`server_auth_token` or `server_read_token` refuses to start, and `serve` refuses one that resolves
to the same value as either. It is never read from a URL
([docs/api-reference.md](api-reference.md#webhook-intake)).

## The reference forms

Two, and deliberately no third.

```yaml
# The file's contents are the value. This is the form to reach for.
server_auth_token: file:/run/secrets/holdfast-control-token

# The command's STDOUT is the value.
notify_url: cmd:/usr/local/bin/fetch-notify-url --service ntfy
```

A `file:` reference is what a container orchestrator already gives you: a Docker or
Compose `secret:`, a Kubernetes secret volume, a systemd `LoadCredential=`. Put the
credential in a file the holdfast user can read and nothing else can (mode `0400`), and
point the key at it.

A `cmd:` reference covers a real secret store. The locator is split on whitespace and run
**with no shell**, so there is no quoting to get wrong and no injection surface; if you
need a pipeline or a variable, write a two-line script and point at that. One trailing
newline is stripped from either form, so a secret written by an editor and one written by
`printf` are the same credential.

### Why there is no `env:` form, and why a literal is refused

A literal credential in `config.yaml` or in `HOLDFAST_SERVER_AUTH_TOKEN` **refuses to
start**, naming the key and how to convert it. That is a deliberate breaking change, and
the reason is the encoder: holdfast starts `ffmpeg` and `ffprobe` as child processes, and a
child inherits its parent's environment. A credential in holdfast's environment is
therefore a credential in every encoder invocation's environment, readable from
`/proc/<pid>/environ` by anything that can see the process. An `env:` reference form would
have exactly the same problem, so there is not one.

If you are migrating from a deployment that set `HOLDFAST_SERVER_AUTH_TOKEN` to a literal:
write the token into a file, and set the variable (or the YAML key) to
`file:/that/path`. The variable still works - it carries the reference now, which is not a
secret.

## What the resolver may and may not do

A resolver's **stdout is the secret channel**: only the consumer of that one key ever sees
it. Nothing else reads it, and in particular:

- A resolver's **stderr is discarded unread**. It is never quoted into a log line or an
  error message, because a resolver that fails *after* printing the credential is the
  common shape and the obvious diagnostic ("resolver said: ...") is how the credential gets
  published. If you need to debug a resolver, run it by hand.
- A failure is reported by the **key, the reference, and the resolver's exit status** -
  never by anything the resolver wrote.
- At most 64 KiB of stdout is kept. A credential is small; a resolver that streams is
  broken.

### The resolver timeout bound

**A `cmd:` resolver has 5s to terminate.** Past that it is abandoned, its whole process
group is killed, and startup fails with a report that is deliberately distinguishable from
"the resolver exited non-zero": a caller retries a secret store that did not answer and
obeys one that said no.

The bound is enforced on the process **group**, not on the resolver alone. A resolver is
usually a wrapper - a script around a store's client - and killing only the direct child
leaves a grandchild alive holding the credential and holding the pipe open, which turns the
bound into no bound at all.

That number lives in exactly one place in the code (`internal/secret.ResolverTimeout`), and
a test in `make check` fails if this document and that constant disagree.

## When configuration is checked

At **start**, before the library is walked and before a single frame is encoded:

1. Every secret-bearing key is parsed. A literal refuses the process.
2. Every configured reference is resolved. One that cannot produce a value refuses the
   process, naming the key and the reference and never a candidate value.

`holdfast validate` performs step 1 and not step 2: proving that a reference *resolves*
means reaching into a secret store, which belongs to a run rather than to a configuration
check. `run` and `serve` both do both - `run` consumes only the three media-server
credentials itself, and still resolves all eight, because a configuration error an operator
finds after a four-hour pass was reported too late.

An **unresolvable reference is a configuration error and is not an outage**. A Tautulli
outage at runtime still fails OPEN and never halts transcoding; a Tautulli api key that
cannot be resolved at start stops the process. Those are different things and they are
deliberately not collapsed.

## Where a credential is never printed

No resolved value appears on stdout, on stderr, in a log line at any level including
`debug`, in an HTTP response body or header holdfast originates, in a metric or a metric
label, or in the output of `validate` or `export`. That is structural rather than
careful: a resolved value is a `secret.Value`, which renders as `<redacted>` through every
formatting route Go has - `fmt`, `slog`, JSON, text - so it cannot reach a log by being
handed to something that formats its arguments, and the `Config` struct holds the reference
rather than the value so there is nothing in it to print.

An outbound request that fails names the **key and its reference**, never the value and
never a credential-bearing part of the destination. For `notify_url` that means not even
the scheme; for Tautulli it means not the request URL, which carries the api key in its
query string. For Radarr, Sonarr and Plex the credential travels in a request header and never
in a URL, and a failed request is still reported as a target name and a failure class, never
as the request error's own text, which quotes the request URL.

<a id="credential-rotation"></a>

**Rotation comes before removal.** If a credential has ever been in a file in this
repository, in its history, or in an operator's `config.yaml`, rotate it at its provider
*first* and then remove it. A commit that deletes a secret without rotating it leaves a
live credential in the reflog, and a clone anybody took is a copy of that reflog.

# The secret scanner

`make secret-scan` refuses a tree that carries an issued credential. It runs on the
pre-commit path and inside `make check`, which is what CI and the release workflow run, so
a hit blocks a commit and a pull request.

## The one setup step

```sh
make install-hooks
```

That points `core.hooksPath` at the committed `.githooks/`, and it is the whole of it.
There is nothing to copy, so a hook that changes in the repository changes for everyone who
has run this. Undo it with `git config --unset core.hooksPath`.

The hook scans the **index**, not the worktree, because `git commit` ships the index: a
credential staged and then deleted from the worktree is what would actually land, and a
worktree scan would call that tree clean.

CI is a second barrier rather than a duplicate. `git commit --no-verify` bypasses every
hook, and no hook travels with a fork's pull request.

## What it catches, and what it does not

High-signal **vendor prefixes** and forbidden **filenames**. There is no entropy
heuristic, and that is a stated trade:

> It catches an **issued token**, whose shape is fixed by the vendor that issued it. It
> does **not** catch a password typed into a config file, a bare base64 blob, or a
> hand-made shared secret.

Nothing may read a clean run as coverage of those. The covered families are AWS access key
ids, GitHub tokens and fine-grained tokens, Slack tokens and webhooks, Google API keys,
Stripe secret keys, Anthropic API keys, OpenAI API keys, PEM private key blocks, PyPI
upload tokens, and npm registry auth directives. The forbidden names are `.env`,
`.env.<anything>`, `id_rsa`, `id_ed25519`, `.npmrc` and `.pypirc` - whatever the file
contains - allowing `.env.example`, `.env.sample` and `.env.template` exactly.

Scope is **tracked files only**. An untracked file is not on its way into a commit, and
`.gitignore` is the control for those. That is why `.gitignore` carries `control-token.txt`
and `.env`: those are the repo-relative credential files `docs/docker.md` and
`docker-compose.yml` tell an operator to create, and a hand-made control token is exactly
the shape the paragraph above says no family can recognise. A credential file this
repository's own guidance names has to be un-stageable, not merely unmatched.

A finding names the path, the line and the family, and prints at most an 8-byte prefix of
what it matched. It never prints the whole match: a report is read by humans, pasted into
issues and printed into CI logs that outlive the rotation.

## Exit codes

| code | meaning |
|---|---|
| 0 | clean: every file in scope was read, and none carried a credential |
| 3 | **FOUND**: rotate the credential at its provider, then remove it |
| 4 | **COULD NOT RUN**: git failed, a file could not be read, or the enumeration was empty. Nothing is cleared by this code |
| 2 | the invocation was wrong |

3 and 4 are distinct because a caller **retries** 4 and **obeys** 3. One code for both
makes a broken scanner indistinguishable from a leak. `./scripts/secret-scan.sh -h` prints
this list.

## The one name exemption

`scripts/check-pins.sh` section 8 *requires* a committed `.npmrc` beside every
`package.json` - it is the only surface that records the lifecycle-script decision, and
`check-pins-selftest.sh` proves that requirement bites. So this repository cannot both
satisfy its own supply-chain gate and hold no tracked `.npmrc`.

`internal/secretscan.NameExemptions` is therefore a register of exact paths the
forbidden-**name** rule does not bind. It is built to refuse growth: it is capped, every
entry carries a reason, and the scan reports **COULD NOT RUN** when an entry names a path
that is not in the tree or a filename that was never forbidden, so an entry cannot outlive
its reason. It forgives a name and never content - every credential family, including the
npm registry auth directive, still binds an exempted path in full.

## Proving it still bites

`make secret-scan-selftest` defeats the scanner on purpose against a throwaway clone: every
credential family, every forbidden name, every exit code, the truncation rule, the
exemption register, and the pre-commit hook itself driven by real `git commit` attempts in
a clone that has performed only `make install-hooks`.

Its fixtures are **composed at runtime and never committed**. A fixture that sat in the
scanned tree would make `make secret-scan` red by construction, and the only ways out of
that would be an allowlist that grows until the scanner stops scanning, or deleting the
proof.

# The identity scan

This repository is public. The owner's name and email appear in `LICENSE` and `NOTICE` and
nowhere else in a tracked file; every other place that needs a person - a fixture, an
example, a test header - uses a synthetic one. `make identity-scan` enforces it inside
`make check`, so CI and the release workflow run it too.

**Whose identity.** The scan reads it at run time from the author of the repository's
**first commit**, and it is written down nowhere, so the guard carries no copy of what it
guards. Not the configured git identity (that is whoever runs the gate, and in CI nobody),
and not HEAD's author (on a pull request, a merge commit GitHub made). A **shallow clone**
does not have the first commit, so the scan refuses one (exit 4) rather than look for
whoever authored its cut-off commit; that is why the workflow jobs that run `make check`
check out with `fetch-depth: 0`.

**What matches.** Each word of the name (two characters or more), the email, and the
email's local part: whole words, ignoring case, in every tracked file including binary
ones. Whole words is what keeps the GitHub account name in URLs and the Go module path out
of it. A finding prints the path, the line and which part matched, and never the matched
text or the identity: a public repository's CI log is public. A clean run prints a
12-digit fingerprint of the identity it derived, so a local run and a CI run can be seen
to have looked for the same person.

The exit codes are the secret scanner's: 0 clean, 3 **FOUND**, 4 **COULD NOT RUN**, 2 a
wrong invocation. `make identity-scan-selftest` proves it bites against throwaway
repositories whose first commit a synthetic owner made.
