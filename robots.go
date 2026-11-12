package soup

import (
	"context"
	"net/url"
	"strings"
	"sync"
)

// robotsRule is a single Allow/Disallow rule with its path prefix.
type robotsRule struct {
	allowed bool
	path    string
}

// robotsRules holds the rules applicable to one or more user agents.
type robotsRules struct {
	rules []robotsRule
}

// robotsGroup represents one user-agent group in a robots.txt file.
type robotsGroup struct {
	agents []string
	rules  []robotsRule
}

// robotsData is the parsed representation of a host's robots.txt.
type robotsData struct {
	// wildcard holds the rules of the User-agent: * group.
	wildcard *robotsRules
	// agents maps a lower-cased user agent name to its specific rules.
	agents map[string]*robotsRules
}

// parseRobots parses the textual contents of a robots.txt file. Unknown
// directives are ignored and comments are stripped.
func parseRobots(contents string) *robotsData {
	data := &robotsData{agents: make(map[string]*robotsRules)}
	var pendingAgents []string
	var pendingRules []robotsRule
	flushGroup := func() {
		if len(pendingAgents) == 0 {
			pendingRules = nil
			return
		}
		for _, agent := range pendingAgents {
			if agent == "*" {
				if data.wildcard == nil {
					data.wildcard = &robotsRules{}
				}
				data.wildcard.rules = append(data.wildcard.rules, pendingRules...)
				continue
			}
			rules, ok := data.agents[agent]
			if !ok {
				rules = &robotsRules{}
				data.agents[agent] = rules
			}
			rules.rules = append(rules.rules, pendingRules...)
		}
		pendingAgents = nil
		pendingRules = nil
	}

	for _, line := range strings.Split(contents, "\n") {
		line = strings.TrimSpace(stripRobotsComment(line))
		if line == "" {
			continue
		}
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)

		switch key {
		case "user-agent":
			if len(pendingRules) > 0 {
				flushGroup()
			}
			pendingAgents = append(pendingAgents, strings.ToLower(value))
		case "allow":
			pendingRules = append(pendingRules, robotsRule{allowed: true, path: value})
		case "disallow":
			rule := robotsRule{allowed: false, path: value}
			if value == "" {
				rule = robotsRule{allowed: true, path: "/"}
			}
			pendingRules = append(pendingRules, rule)
		default:
			flushGroup()
		}
	}
	flushGroup()
	return data
}

// stripRobotsComment removes an optional inline comment introduced by #.
func stripRobotsComment(line string) string {
	if idx := strings.IndexByte(line, '#'); idx >= 0 {
		return line[:idx]
	}
	return line
}

// allowed reports whether path may be fetched for userAgent. Matching uses
// prefix rules with the longest match winning; on equal lengths Allow wins.
// When no rule matches, access is allowed by default.
func (d *robotsData) allowed(userAgent, path string) bool {
	rules := d.rulesFor(userAgent)
	var bestMatch *robotsRule
	for i := range rules {
		rule := &rules[i]
		if rule.path == "" || !strings.HasPrefix(path, rule.path) {
			continue
		}
		if bestMatch == nil || len(rule.path) > len(bestMatch.path) {
			bestMatch = rule
		}
		// On equal-length prefixes, Allow wins over Disallow.
		if bestMatch != nil && rule != bestMatch &&
			len(rule.path) == len(bestMatch.path) && rule.allowed && !bestMatch.allowed {
			bestMatch = rule
		}
	}
	if bestMatch == nil {
		return true
	}
	return bestMatch.allowed
}

// rulesFor resolves the applicable rule set for userAgent, preferring an
// exact agent match and falling back to the wildcard group.
func (d *robotsData) rulesFor(userAgent string) []robotsRule {
	name := strings.ToLower(strings.TrimSpace(userAgent))
	if name != "" {
		if specific, ok := d.agents[name]; ok {
			return specific.rules
		}
		// A robots group may name only the first token of the product token.
		if first, _, ok := strings.Cut(name, " "); ok {
			if specific, ok := d.agents[first]; ok {
				return specific.rules
			}
		}
	}
	if d.wildcard != nil {
		return d.wildcard.rules
	}
	return nil
}

// robotsCache fetches and caches one parsed robots.txt per host. Fetch
// failures, including 404s, are cached as "everything allowed" per the
// robots.txt convention.
type robotsCache struct {
	mu       sync.Mutex
	entries  map[string]*robotsData
	inflight map[string]chan struct{}
}

func newRobotsCache() *robotsCache {
	return &robotsCache{
		entries:  make(map[string]*robotsData),
		inflight: make(map[string]chan struct{}),
	}
}

// allowed fetches (once per host) and evaluates the robots.txt for rawurl.
func (c *robotsCache) allowed(ctx context.Context, fetcher *Fetcher, userAgent, rawurl string) (bool, error) {
	parsed, err := url.Parse(rawurl)
	if err != nil {
		return false, err
	}
	host := parsed.Host
	c.mu.Lock()
	data, ok := c.entries[host]
	c.mu.Unlock()
	if !ok {
		data = c.fetchOrJoin(ctx, fetcher, parsed.Scheme, host)
	}
	return data.allowed(userAgent, parsed.EscapedPath()), nil
}

// fetchOrJoin loads a host's robots.txt once: concurrent callers join the
// in-flight load instead of issuing duplicate requests.
func (c *robotsCache) fetchOrJoin(ctx context.Context, fetcher *Fetcher, scheme, host string) *robotsData {
	c.mu.Lock()
	if data, ok := c.entries[host]; ok {
		c.mu.Unlock()
		return data
	}
	if wait, ok := c.inflight[host]; ok {
		c.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			// On cancellation return a permissive result without poisoning cache.
			return &robotsData{agents: make(map[string]*robotsRules)}
		}
		c.mu.Lock()
		data := c.entries[host]
		c.mu.Unlock()
		if data != nil {
			return data
		}
		return &robotsData{agents: make(map[string]*robotsRules)}
	}
	ready := make(chan struct{})
	c.inflight[host] = ready
	c.mu.Unlock()

	robotsURL := scheme + "://" + host + "/robots.txt"
	data := &robotsData{agents: make(map[string]*robotsRules)}
	resp, err := fetcher.Get(ctx, robotsURL)
	if err == nil && resp.StatusCode < 400 {
		data = parseRobots(string(resp.Body))
	}

	c.mu.Lock()
	c.entries[host] = data
	delete(c.inflight, host)
	c.mu.Unlock()
	close(ready)
	return data
}
