// Command clientreport is the Go half of scripts/client-report.sh: the owner's live check of
// their own Plex, Sonarr or Radarr, which writes a redacted report for them to commit under
// testdata/client-reports/ (brief T49, docs/client-reports.md).
//
// No goal, test or gate ever runs it against a real service: the tests here drive it against
// httptest fakes. The owner runs it, on their own network, against their own services.
//
// It reads the target the way holdfast does - the configuration's `*_url`, the credential by
// reference through internal/secret, the path map - and sends its requests through
// internal/mediaclient, the clients holdfast ships, so a report proves the shipped path.
//
// REDACTION is the point. A report is one closed struct (report.go): versions, HTTP status
// codes, counts, booleans, failure classes, this build's version and commit, the service
// kind and the date. Nothing a service answered is copied through: no address, host name,
// port, URL, credential, machine identifier, title, path, user or device name has a field to
// land in. Before a report is written it is checked twice, and either check refusing means no
// file: Verify (every string is one of this build's own words or shaped like a version), and
// identityScan (the resolved credential and the configured address are not in the bytes).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/mediaclient"
	"github.com/NSchatz/holdfast/internal/secret"
	"github.com/NSchatz/holdfast/internal/version"
)

// The exit codes.
const (
	exitWritten = 0 // a report was written (it may record failed requests), or --verify passed
	exitRefused = 1 // no report: the configuration, the credential, the checks or the file refused
	exitUsage   = 2 // the invocation was wrong
)

const usage = `clientreport - a redacted live check of the owner's own Plex, Sonarr or Radarr (T49)

Usage:
  clientreport --service plex|sonarr|radarr --config CONFIG --out REPORT [write check]
  clientreport --verify REPORT

  --service KIND     the service to check: plex, sonarr or radarr
  --config CONFIG    the holdfast configuration; the address, the credential reference and
                     the path map are read from it exactly as holdfast reads them
  --out REPORT       the report to write; an existing file is never overwritten
  --verify REPORT    check that an existing report carries only what a report may carry

The checks are read-only unless ONE write check is asked for by name:
  --refresh-dir DIR  plex only: one partial refresh of that one directory
  --rescan-dir DIR   sonarr or radarr only: one RescanSeries or RescanMovie, by id, for the
                     series or movie that owns that directory
DIR is a directory as holdfast sees it. Neither can refresh a whole section or library.

Exit codes: 0 a report was written (read it: it may record failed requests), 1 no report
was written, 2 the invocation was wrong. docs/client-reports.md is the reference.
`

// requestBound bounds one read check; a write check, which is two requests, gets twice it.
// It is the bound the shipped clients apply to every request. A test shortens it.
var requestBound = mediaclient.RequestTimeout

// now is the clock the report's date is read from. A test fixes it.
var now = time.Now

func main() { os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr)) }

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("clientreport", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	service := fs.String("service", "", "")
	cfgPath := fs.String("config", "", "")
	out := fs.String("out", "", "")
	verify := fs.String("verify", "", "")
	refreshDir := fs.String("refresh-dir", "", "")
	rescanDir := fs.String("rescan-dir", "", "")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, usage)
			return exitWritten
		}
		fmt.Fprintf(stderr, "clientreport: %v\n%s", err, usage)
		return exitUsage
	}
	fail := func(code int, format string, a ...any) int {
		fmt.Fprintf(stderr, "clientreport: "+format+"\n", a...)
		return code
	}
	if fs.NArg() != 0 {
		return fail(exitUsage, "unexpected argument (every value is given by flag)")
	}

	if *verify != "" {
		if *service != "" || *cfgPath != "" || *out != "" || *refreshDir != "" || *rescanDir != "" {
			return fail(exitUsage, "--verify takes no other flag")
		}
		b, err := os.ReadFile(*verify)
		if err != nil {
			return fail(exitRefused, "the report could not be read")
		}
		if err := Verify(b); err != nil {
			return fail(exitRefused, "the report does not pass: %v", err)
		}
		fmt.Fprintln(stdout, "clientreport: the report carries only what a report may carry")
		return exitWritten
	}

	switch *service {
	case servicePlex, serviceSonarr, serviceRadarr:
	default:
		return fail(exitUsage, "--service must be plex, sonarr or radarr")
	}
	if *cfgPath == "" || *out == "" {
		return fail(exitUsage, "--config and --out are both required")
	}
	writeDir, err := writeCheckDir(*service, *refreshDir, *rescanDir)
	if err != nil {
		return fail(exitUsage, "%v", err)
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		// holdfast's own start-time refusal, which never quotes a credential or an address.
		return fail(exitRefused, "%v", err)
	}
	// Validate is where a literal credential, a half-configured target and an address that
	// is not one are refused, exactly as `holdfast validate` refuses them.
	if err := cfg.Validate(); err != nil {
		return fail(exitRefused, "invalid config: %v", err)
	}
	target := cfg.MediaTarget(*service)
	if !target.Enabled {
		return fail(exitRefused, "the %s target is not configured: set %s and %s (docs/post-swap-hook.md)",
			target.Name, target.URLKey, target.CredentialKey)
	}
	ref, err := credentialRef(cfg, target.CredentialKey)
	if err != nil {
		return fail(exitRefused, "invalid config: %v", err)
	}
	credential, err := ref.Resolve(ctx)
	if err != nil {
		// The resolver's own refusal: the key, the reference and an exit status, never a value.
		return fail(exitRefused, "%v", err)
	}
	if strings.TrimSpace(credential.Expose()) == "" {
		return fail(exitRefused, "%s resolved to an empty value", target.CredentialKey)
	}

	report := newReport(*service, target, len(cfg.LibraryRoots))
	switch *service {
	case servicePlex:
		report.Plex = checkPlex(ctx, mediaclient.NewPlex(target.URL, credential, target.PathMap), cfg.LibraryRoots, writeDir)
		report.CredentialAccepted = accepted(report.Plex.Sections.Request, report.Plex.Sessions.Request)
	default:
		arr := mediaclient.NewSonarr(target.URL, credential, target.PathMap)
		if *service == serviceRadarr {
			arr = mediaclient.NewRadarr(target.URL, credential, target.PathMap)
		}
		report.Arr = checkArr(ctx, arr, cfg.LibraryRoots, writeDir)
		report.CredentialAccepted = accepted(report.Arr.Status.Request, report.Arr.Library.Request)
	}

	encoded, err := encode(report)
	if err != nil {
		return fail(exitRefused, "the report could not be encoded")
	}
	if err := Verify(encoded); err != nil {
		return fail(exitRefused, "refusing to write the report: %v", err)
	}
	if err := identityScan(encoded, credential, target.URL); err != nil {
		return fail(exitRefused, "refusing to write the report: %v", err)
	}
	if err := writeNew(*out, encoded); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fail(exitRefused, "refusing to overwrite an existing report (a report is a record; name another --out)")
		}
		return fail(exitRefused, "the report could not be written (does the directory of --out exist, and is it writable?)")
	}
	summarize(stdout, report)
	return exitWritten
}

// credentialRef is the one reference this check resolves: the chosen target's, and no other
// key's. A reference that cannot be parsed is a refusal, never an empty credential.
func credentialRef(cfg *config.Config, key string) (secret.Ref, error) {
	refs, err := cfg.SecretRefs()
	if err != nil {
		return secret.Ref{}, err
	}
	for _, r := range refs {
		if r.Key() == key && r.Configured() {
			return r, nil
		}
	}
	return secret.Ref{}, fmt.Errorf("%s carries no reference", key)
}

// writeCheckDir answers the directory of the one write check that was asked for, "" for
// none. A write flag that does not belong to the service is a usage error rather than a flag
// quietly ignored, and so is a directory that is not absolute.
func writeCheckDir(service, refreshDir, rescanDir string) (string, error) {
	dir := refreshDir
	switch {
	case refreshDir != "" && rescanDir != "":
		return "", errors.New("--refresh-dir and --rescan-dir cannot both be given")
	case refreshDir != "" && service != servicePlex:
		return "", errors.New("--refresh-dir is the plex write check; sonarr and radarr take --rescan-dir")
	case rescanDir != "" && service == servicePlex:
		return "", errors.New("--rescan-dir is the sonarr and radarr write check; plex takes --refresh-dir")
	case rescanDir != "":
		dir = rescanDir
	}
	if dir != "" && !strings.HasPrefix(dir, "/") {
		return "", errors.New("the write check's directory must be an absolute path, as holdfast sees it")
	}
	return dir, nil
}

func newReport(service string, target config.MediaTarget, roots int) *Report {
	r := &Report{
		Schema:  Schema,
		Service: service,
		Date:    now().UTC().Format(dateLayout),
		Holdfast: Build{
			Version: shaped(buildVersionShape, version.Version),
			Commit:  shaped(commitShape, version.Commit),
		},
		Config: ConfigFacts{PathMapEntries: len(target.PathMap), LibraryRoots: roots},
	}
	if u, err := url.Parse(target.URL); err == nil {
		if u.Scheme == "http" || u.Scheme == "https" {
			r.Config.URLScheme = u.Scheme
		}
		r.Config.URLHasBasePath = strings.Trim(u.Path, "/") != ""
	}
	return r
}

// bounded runs one check under n request bounds.
func bounded[T any](ctx context.Context, n int, check func(context.Context) T) T {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(n)*requestBound)
	defer cancel()
	return check(ctx)
}

func request(label string, p mediaclient.Probe) Request {
	return Request{Request: label, OK: p.OK(), Status: p.Status, FailureClass: p.FailureClass}
}

func checkPlex(ctx context.Context, plex *mediaclient.Plex, roots []string, writeDir string) *PlexReport {
	out := &PlexReport{}

	id := bounded(ctx, 1, plex.CheckIdentity)
	out.Identity = PlexIdentity{Request: request(reqPlexIdentity, id.Probe), Version: id.Version}

	sec := bounded(ctx, 1, func(ctx context.Context) mediaclient.PlexSections { return plex.CheckSections(ctx, roots) })
	out.Sections = PlexSections{
		Request:                      request(reqPlexSections, sec.Probe),
		Count:                        len(sec.Sections),
		Sections:                     []PlexSection{},
		LibraryRootsInsideALocation:  sec.RootsInsideALocation,
		LibraryRootsContainingOne:    sec.RootsContainingALocation,
		EveryLocationCarriesAPath:    sec.OK(),
		EverySectionKeyIsANumber:     sec.OK(),
		ALibraryRootMapsIntoASection: sec.RootsInsideALocation > 0,
	}
	for _, s := range sec.Sections {
		out.Sections.Sections = append(out.Sections.Sections, PlexSection{
			Type: s.Type, Locations: s.Locations, EveryLocationCarriesAPath: s.EveryLocationHasPath, KeyIsANumber: s.KeyIsSectionNumber,
		})
		out.Sections.EveryLocationCarriesAPath = out.Sections.EveryLocationCarriesAPath && s.EveryLocationHasPath
		out.Sections.EverySectionKeyIsANumber = out.Sections.EverySectionKeyIsANumber && s.KeyIsSectionNumber
	}

	ses := bounded(ctx, 1, plex.CheckSessions)
	out.Sessions = PlexSessions{
		Request:                  request(reqPlexSessions, ses.Probe),
		Count:                    ses.Sessions,
		SessionsWithMedia:        ses.SessionsWithMedia,
		SessionsWithPart:         ses.SessionsWithPart,
		SessionsEveryPartHasFile: ses.SessionsEveryPartHasFile,
		Parts:                    ses.Parts,
		PartsWithFile:            ses.PartsWithFile,
	}
	if ses.OK() && ses.Sessions > 0 {
		every := ses.SessionsEveryPartHasFile == ses.Sessions
		out.Sessions.EverySessionCarriesPartFile = &every
	}

	if writeDir != "" {
		w := bounded(ctx, 2, func(ctx context.Context) mediaclient.WriteCheck { return plex.CheckRefresh(ctx, writeDir) })
		out.Refresh = writeReport(reqPlexSections, reqPlexRefresh, w)
	}
	return out
}

func checkArr(ctx context.Context, arr *mediaclient.Arr, roots []string, writeDir string) *ArrReport {
	list, command := reqSonarrList, reqArrCommand
	if arr.Name() == serviceRadarr {
		list = reqRadarrList
	}
	out := &ArrReport{}

	st := bounded(ctx, 1, arr.CheckStatus)
	out.Status = ArrStatus{Request: request(reqArrStatus, st.Probe), Version: st.Version, App: st.App}

	lib := bounded(ctx, 1, func(ctx context.Context) mediaclient.ArrLibrary { return arr.CheckLibrary(ctx, roots) })
	out.Library = ArrLibrary{
		Request:                       request(list, lib.Probe),
		Count:                         lib.Items,
		ItemsWithIntegerID:            lib.ItemsWithIntegerID,
		ItemsWithStringPath:           lib.ItemsWithStringPath,
		ItemsUsable:                   lib.ItemsUsable,
		ItemsTheClientReads:           lib.ItemsTheClientReads,
		EveryItemHasIntegerIDAndPath:  lib.OK() && lib.ItemsWithIntegerID == lib.Items && lib.ItemsWithStringPath == lib.Items,
		LibraryRootsContainingAnItem:  lib.RootsContainingAnItem,
		LibraryRootsInsideAnItem:      lib.RootsInsideAnItem,
		ALibraryRootMapsOntoTheirPath: lib.RootsContainingAnItem > 0 || lib.RootsInsideAnItem > 0,
	}

	if writeDir != "" {
		w := bounded(ctx, 2, func(ctx context.Context) mediaclient.WriteCheck { return arr.CheckRescan(ctx, writeDir) })
		out.Rescan = writeReport(list, command, w)
	}
	return out
}

func writeReport(lookupLabel, requestLabel string, w mediaclient.WriteCheck) *WriteReport {
	out := &WriteReport{
		Lookup:        request(lookupLabel, w.Lookup),
		OwnerFound:    w.OwnerFound,
		Sent:          w.Requested,
		Accepted:      w.Accepted,
		AnswerParsed:  w.AnswerParsed,
		CommandName:   w.CommandName,
		CommandStatus: w.CommandStatus,
	}
	if w.OwnerFound {
		r := request(requestLabel, w.Request)
		out.Request = &r
	}
	return out
}

// accepted answers whether the credential was accepted, from the requests that need one:
// false when any was answered 401 or 403, true when any succeeded, and nil when neither
// happened (the service was never reached), which decides nothing.
func accepted(requests ...Request) *bool {
	var answer *bool
	for _, r := range requests {
		switch {
		case r.FailureClass == mediaclient.ClassUnauthorized:
			no := false
			return &no
		case r.OK:
			yes := true
			answer = &yes
		}
	}
	return answer
}

// identityScan is the last thing between a report and the disk, and it fails closed: a
// report whose bytes carry the resolved credential, the configured address, its host and
// port or its host name is not written. Its refusal names which, never the value.
//
// A host name of one label (`plex`, `sonarr`, a compose service name) is compared with the
// report's string VALUES that are not this build's own words: the report says `"service":
// "plex"` whatever the host is called, and that word carries nothing about the host. Every
// other needle is searched for in the bytes, case-insensitively.
func identityScan(encoded []byte, credential secret.Value, address string) error {
	hay := strings.ToLower(string(encoded))
	contains := func(needle string) bool {
		needle = strings.ToLower(strings.TrimSpace(needle))
		return needle != "" && strings.Contains(hay, needle)
	}
	if contains(credential.Expose()) {
		return errors.New("it would carry the resolved credential")
	}
	if contains(address) {
		return errors.New("it would carry the configured address")
	}
	u, err := url.Parse(address)
	if err != nil {
		return errors.New("the configured address could not be read, so the report cannot be checked against it")
	}
	if contains(u.Host) && u.Host != u.Hostname() {
		return errors.New("it would carry the configured host and port")
	}
	host := u.Hostname()
	if strings.ContainsAny(host, ".:") {
		if contains(host) {
			return errors.New("it would carry the configured host")
		}
		return nil
	}
	foreign, err := foreignStrings(encoded)
	if err != nil {
		return err
	}
	for _, s := range foreign {
		if host != "" && strings.Contains(strings.ToLower(s), strings.ToLower(host)) {
			return errors.New("it would carry the configured host name")
		}
	}
	return nil
}

// writeNew writes the report to a file that must not exist yet.
func writeNew(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}

// summarize prints what the report says, one line a request, from the report's own closed
// facts and nothing else.
func summarize(w io.Writer, r *Report) {
	var requests []Request
	var writes []*WriteReport
	if r.Plex != nil {
		requests = append(requests, r.Plex.Identity.Request, r.Plex.Sections.Request, r.Plex.Sessions.Request)
		writes = append(writes, r.Plex.Refresh)
	}
	if r.Arr != nil {
		requests = append(requests, r.Arr.Status.Request, r.Arr.Library.Request)
		writes = append(writes, r.Arr.Rescan)
	}
	for _, wr := range writes {
		if wr == nil {
			continue
		}
		if wr.Request != nil {
			requests = append(requests, *wr.Request)
		} else {
			fmt.Fprintf(w, "clientreport: %s: write check: nothing was sent (owner found: %t)\n", r.Service, wr.OwnerFound)
		}
	}
	for _, q := range requests {
		result := "ok"
		if !q.OK {
			result = q.FailureClass
		}
		fmt.Fprintf(w, "clientreport: %s: %s: %s (status %d)\n", r.Service, q.Request, result, q.Status)
	}
	if r.CredentialAccepted != nil && !*r.CredentialAccepted {
		hint := "check the api key"
		if r.Service == servicePlex {
			hint = "Plex needs the server owner's token (the admin scope)"
		}
		fmt.Fprintf(w, "clientreport: %s: the credential was refused: %s\n", r.Service, hint)
	}
	fmt.Fprintln(w, "clientreport: wrote the report")
}
