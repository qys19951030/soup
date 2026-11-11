package soup

import (
	"net/url"
	"path"
	"strconv"
	"strings"
)

// allowedCrawlSchemes are the only schemes the crawler follows.
var allowedCrawlSchemes = map[string]bool{
	"http":  true,
	"https": true,
}

// defaultPorts maps a scheme to the port that is implicit and therefore
// removed during normalization.
var defaultPorts = map[string]string{
	"http":  "80",
	"https": "443",
}

// NormalizeURL resolves raw against base and returns a canonical absolute
// http(s) URL. Relative references are resolved against base, fragments are
// removed, scheme and host are lower-cased, default ports are stripped and
// dot segments in the path are cleaned. Non-http(s) schemes and inputs that
// cannot be resolved against a missing base are rejected with an error.
func NormalizeURL(raw, base string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}

	if base != "" {
		baseURL, err := url.Parse(base)
		if err != nil {
			return "", err
		}
		parsed = baseURL.ResolveReference(parsed)
	}

	scheme := strings.ToLower(parsed.Scheme)
	if !allowedCrawlSchemes[scheme] {
		return "", &URLError{URL: raw, Reason: "unsupported scheme: " + parsed.Scheme}
	}
	parsed.Scheme = scheme

	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return "", &URLError{URL: raw, Reason: "missing host"}
	}
	port := parsed.Port()
	if port != "" && defaultPorts[scheme] == port {
		port = ""
	}
	if port != "" {
		parsed.Host = host + ":" + port
	} else {
		parsed.Host = host
	}

	parsed.Fragment = ""
	parsed.Path = cleanPath(parsed.Path)
	return parsed.String(), nil
}

// cleanPath resolves dot segments while preserving a trailing slash.
func cleanPath(p string) string {
	if p == "" {
		return ""
	}
	hasTrailingSlash := strings.HasSuffix(p, "/")
	cleaned := path.Clean(p)
	if hasTrailingSlash && !strings.HasSuffix(cleaned, "/") {
		cleaned += "/"
	}
	return cleaned
}

// URLError reports a URL that could not be normalized or followed.
type URLError struct {
	URL    string
	Reason string
}

func (e *URLError) Error() string {
	return "url " + strconv.Quote(e.URL) + " cannot be normalized: " + e.Reason
}
