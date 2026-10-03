package config

import (
	"fmt"
	"net/url"
	"strings"
)

// The media-server client keys (docs/design/media-clients.md#media-clients): three targets,
// each an address, a credential reached by reference and an optional path map. Every one is
// off until its address and its credential are both set.
const (
	radarrURLKey     = "radarr_url"
	radarrAPIKeyKey  = "radarr_api_key"
	radarrPathMapKey = "radarr_path_map"
	sonarrURLKey     = "sonarr_url"
	sonarrAPIKeyKey  = "sonarr_api_key"
	sonarrPathMapKey = "sonarr_path_map"
	plexURLKey       = "plex_url"
	plexTokenKey     = "plex_token"
	plexPathMapKey   = "plex_path_map"
)

// pathMapKeys are the three path-map keys, in the order Load parses them.
var pathMapKeys = []string{radarrPathMapKey, sonarrPathMapKey, plexPathMapKey}

// MediaTarget is one media-server target as the configuration states it.
type MediaTarget struct {
	// Name is the target's name in every log record: radarr, sonarr or plex.
	Name string
	// URLKey, CredentialKey and PathMapKey are the three configuration keys it is read from.
	URLKey, CredentialKey, PathMapKey string
	// URL is the configured address, trimmed, with no trailing slash.
	URL string
	// PathMap translates holdfast's view of the library to this target's.
	PathMap PathMap
	// Enabled is true when the address and the credential reference are both set.
	Enabled bool

	credential string
}

// MediaTargets is the three targets in a fixed order: radarr, sonarr, plex.
func (c *Config) MediaTargets() []MediaTarget {
	targets := []MediaTarget{
		{Name: "radarr", URLKey: radarrURLKey, CredentialKey: radarrAPIKeyKey, PathMapKey: radarrPathMapKey,
			URL: c.RadarrURL, credential: c.RadarrAPIKey, PathMap: c.RadarrPathMap},
		{Name: "sonarr", URLKey: sonarrURLKey, CredentialKey: sonarrAPIKeyKey, PathMapKey: sonarrPathMapKey,
			URL: c.SonarrURL, credential: c.SonarrAPIKey, PathMap: c.SonarrPathMap},
		{Name: "plex", URLKey: plexURLKey, CredentialKey: plexTokenKey, PathMapKey: plexPathMapKey,
			URL: c.PlexURL, credential: c.PlexToken, PathMap: c.PlexPathMap},
	}
	for i := range targets {
		t := &targets[i]
		t.URL = strings.TrimRight(strings.TrimSpace(t.URL), "/")
		t.credential = strings.TrimSpace(t.credential)
		t.Enabled = t.URL != "" && t.credential != ""
	}
	return targets
}

// MediaTarget returns the target with that name. It panics on any other name, because every
// caller names a constant.
func (c *Config) MediaTarget(name string) MediaTarget {
	for _, t := range c.MediaTargets() {
		if t.Name == name {
			return t
		}
	}
	panic("config: no media target named " + name)
}

// validateMediaTargets refuses a half-configured target, an address that is not an absolute
// http or https URL, and a path map with a side that is not absolute, naming the key. It
// runs after the literal-credential refusal, so a pasted credential is reported as one.
func (c *Config) validateMediaTargets() error {
	for _, t := range c.MediaTargets() {
		switch {
		case t.URL != "" && t.credential == "":
			return fmt.Errorf("%s is set but %s is not: the %s target needs both, so it is half-configured. "+
				"Set %s to a reference (file:<path> or cmd:<argv>) or remove %s",
				t.URLKey, t.CredentialKey, t.Name, t.CredentialKey, t.URLKey)
		case t.URL == "" && t.credential != "":
			return fmt.Errorf("%s is set but %s is not: the %s target needs both, so it is half-configured. "+
				"Set %s to the service's address (http://host:port) or remove %s",
				t.CredentialKey, t.URLKey, t.Name, t.URLKey, t.CredentialKey)
		}
		if t.URL != "" {
			if err := checkTargetURL(t.URLKey, t.URL); err != nil {
				return err
			}
		}
		if err := t.PathMap.validate(t.PathMapKey); err != nil {
			return err
		}
	}
	return nil
}

// checkTargetURL accepts an absolute http or https URL made of a scheme, a host and an
// optional base path, and nothing else. Userinfo, a query and a fragment are refused: each
// is a place a credential gets pasted, and the address is not a secret-bearing key. The
// refusal never quotes the value for the same reason.
func checkTargetURL(key, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Hostname() == "" {
		return fmt.Errorf("%s must be an absolute http or https URL (http://host:port, with an optional "+
			"base path): the configured value is not one", key)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(raw, "#?") {
		return fmt.Errorf("%s must carry only a scheme, a host and an optional base path: the configured "+
			"value has userinfo, a query or a fragment, and a credential belongs in its own key, by reference",
			key)
	}
	return nil
}

// misplacedPathMapError is the refusal for a path map written in the environment. A path map
// is a list of pairs of paths, and a path may hold any character a separator could be, so
// there is no environment spelling that could not be misread.
func misplacedPathMapError(key string) error {
	return fmt.Errorf("%s%s is set, but %s has no environment form: it is a list of from/to pairs "+
		"and is written in the config file", envPrefix, strings.ToUpper(key), key)
}
