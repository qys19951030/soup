package soup

import (
	"context"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"

	"golang.org/x/net/html"
)

// DefaultCrawlOptions supplies defaults for zero-valued CrawlOptions fields.
var DefaultCrawlOptions = CrawlOptions{
	MaxDepth:      3,
	MaxPages:      100,
	Concurrency:   4,
	SameDomain:    true,
	RespectRobots: true,
	UserAgent:     "soup-crawler/1.0 (+https://github.com/anaskhan96/soup)",
}

// CrawlOptions configures a Crawl run.
type CrawlOptions struct {
	// MaxDepth is the greatest link depth that is fetched. Seeds are depth 0.
	MaxDepth int
	// MaxPages caps the number of distinct URLs that are enqueued.
	MaxPages int
	// Concurrency bounds the number of pages fetched simultaneously.
	Concurrency int
	// SameDomain restricts enqueued links to the seed pages' hosts.
	SameDomain bool
	// RespectRobots enables robots.txt fetching and enforcement per host.
	RespectRobots bool
	// UserAgent is sent on every request and used for robots matching.
	UserAgent string
	// FetchOptions configures the underlying Fetcher.
	FetchOptions FetchOptions
}

// Page is the result of fetching one crawled URL.
type Page struct {
	URL        string
	Depth      int
	StatusCode int
	Body       []byte
	// Err holds this page's own failure. A crawl keeps going when Err is set.
	Err error
}

// CrawlStats summarizes a crawl run.
type CrawlStats struct {
	// Requests is the number of distinct URLs that were enqueued and
	// attempted, including failed pages.
	Requests int
	// Success is the number of pages fetched with a 2xx/3xx status.
	Success int
	// Failed is the number of pages whose fetch returned an error.
	Failed int
	// RobotsBlocked counts links skipped because robots.txt disallowed them.
	RobotsBlocked int
	// Duplicates counts normalized links that were already known.
	Duplicates int
}

// crawlJob is one unit of work for the worker pool.
type crawlJob struct {
	rawURL string
	depth  int
}

// crawler holds the mutable state of a single Crawl run.
type crawler struct {
	opts      CrawlOptions
	fetcher   *Fetcher
	robots    *robotsCache
	seenMu    sync.Mutex
	seen      map[string]bool
	enqueued  int
	pagesMu   sync.Mutex
	pages     []Page
	stats     CrawlStats
	statsMu   sync.Mutex
	seedHosts map[string]bool
}

// Crawl performs a breadth-first crawl starting from seeds and returns every
// fetched page together with aggregate statistics. Per-page failures are
// recorded on the corresponding Page and never abort the run; context
// cancellation returns the pages collected so far alongside the context error.
func Crawl(ctx context.Context, seeds []string, opts CrawlOptions) ([]Page, CrawlStats, error) {
	opts = withCrawlDefaults(opts)

	fetchOpts := opts.FetchOptions
	if opts.UserAgent != "" {
		if fetchOpts.Headers == nil {
			fetchOpts.Headers = make(http.Header)
		}
		if fetchOpts.Headers.Get("User-Agent") == "" {
			fetchOpts.Headers.Set("User-Agent", opts.UserAgent)
		}
	}

	c := &crawler{
		opts:      opts,
		fetcher:   NewFetcher(fetchOpts),
		robots:    newRobotsCache(),
		seen:      make(map[string]bool),
		seedHosts: make(map[string]bool),
	}

	frontier, err := c.prepareSeeds(ctx, seeds)
	if err != nil {
		return nil, CrawlStats{}, err
	}

	sem := make(chan struct{}, opts.Concurrency)
	var crawlErr error
	for depth := 0; len(frontier) > 0; depth++ {
		if err := ctx.Err(); err != nil {
			crawlErr = err
			break
		}
		nextFrontier := c.dispatchLayer(ctx, sem, frontier, depth)
		frontier = nextFrontier
		if err := ctx.Err(); err != nil {
			crawlErr = err
		}
		if depth >= opts.MaxDepth {
			break
		}
	}

	sort.SliceStable(c.pages, func(i, j int) bool {
		if c.pages[i].Depth != c.pages[j].Depth {
			return c.pages[i].Depth < c.pages[j].Depth
		}
		return c.pages[i].URL < c.pages[j].URL
	})
	return c.pages, c.snapshotStats(), crawlErr
}

// dispatchLayer feeds one BFS layer to the workers and collects the links
// discovered by that layer for the next one.
func (c *crawler) dispatchLayer(ctx context.Context, sem chan struct{}, layer []crawlJob, depth int) []crawlJob {
	discovered := make(chan []crawlJob, len(layer))
	done := make(chan struct{})
	var nextFrontier []crawlJob
	go func() {
		for links := range discovered {
			nextFrontier = append(nextFrontier, links...)
		}
		close(done)
	}()

	var layerWg sync.WaitGroup
	for _, job := range layer {
		layerWg.Add(1)
		go func(job crawlJob) {
			defer layerWg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			links := c.processPageWithLinks(ctx, job)
			select {
			case discovered <- links:
			case <-ctx.Done():
			}
		}(job)
	}
	layerWg.Wait()
	close(discovered)
	<-done
	return nextFrontier
}

// withCrawlDefaults fills zero-valued crawl options.
func withCrawlDefaults(opts CrawlOptions) CrawlOptions {
	if opts.MaxDepth <= 0 {
		opts.MaxDepth = DefaultCrawlOptions.MaxDepth
	}
	if opts.MaxPages <= 0 {
		opts.MaxPages = DefaultCrawlOptions.MaxPages
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = DefaultCrawlOptions.Concurrency
	}
	if opts.UserAgent == "" {
		opts.UserAgent = DefaultCrawlOptions.UserAgent
	}
	return opts
}

// prepareSeeds normalizes and dedupes the seed URLs, recording host names for
// SameDomain filtering and producing the depth-0 frontier. Unnormalizable
// seeds become error pages rather than aborting the whole crawl.
func (c *crawler) prepareSeeds(ctx context.Context, seeds []string) ([]crawlJob, error) {
	var frontier []crawlJob
	var badSeeds []Page
	for _, seed := range seeds {
		normalized, err := NormalizeURL(seed, "")
		if err != nil {
			badSeeds = append(badSeeds, Page{URL: seed, Depth: 0, Err: err})
			continue
		}
		host := hostOf(normalized)
		c.seedHosts[host] = true
		if c.enqueue(normalized) {
			frontier = append(frontier, crawlJob{rawURL: normalized, depth: 0})
		}
	}
	c.pagesMu.Lock()
	c.pages = append(c.pages, badSeeds...)
	c.pagesMu.Unlock()
	c.statsMu.Lock()
	c.stats.Failed += len(badSeeds)
	c.statsMu.Unlock()
	return frontier, ctx.Err()
}

// processPageWithLinks fetches one page, records it, and returns normalized
// links eligible for the next BFS layer.
func (c *crawler) processPageWithLinks(ctx context.Context, job crawlJob) []crawlJob {
	resp, err := c.fetcher.Get(ctx, job.rawURL)
	page := Page{URL: job.rawURL, Depth: job.depth}
	if err != nil {
		page.Err = err
		if statusErr, ok := err.(*HTTPStatusError); ok {
			page.StatusCode = statusErr.StatusCode
		}
		c.recordPage(page, false)
		return nil
	}
	page.StatusCode = resp.StatusCode
	page.Body = resp.Body
	c.recordPage(page, true)

	if job.depth >= c.opts.MaxDepth {
		return nil
	}
	return c.extractLinks(ctx, string(resp.Body), job.rawURL, job.depth+1)
}

// recordPage stores a fetched page and updates success/failure statistics.
func (c *crawler) recordPage(page Page, success bool) {
	c.pagesMu.Lock()
	c.pages = append(c.pages, page)
	c.pagesMu.Unlock()
	c.statsMu.Lock()
	if success {
		c.stats.Success++
	} else {
		c.stats.Failed++
	}
	c.statsMu.Unlock()
}

// extractLinks parses a body and turns every followable anchor into a
// normalized, deduped crawl job for the next layer.
func (c *crawler) extractLinks(ctx context.Context, body, pageURL string, nextDepth int) []crawlJob {
	root := HTMLParse(body)
	if root.Error != nil || root.Pointer == nil {
		return nil
	}
	anchors := root.FindAll("a")
	var jobs []crawlJob
	for _, anchor := range anchors {
		if ctx.Err() != nil {
			return jobs
		}
		href, ok := nodeAttr(anchor.Pointer, "href")
		if !ok || strings.TrimSpace(href) == "" {
			continue
		}
		normalized, err := NormalizeURL(href, pageURL)
		if err != nil {
			continue
		}
		if c.opts.SameDomain && !c.seedHosts[hostOf(normalized)] {
			continue
		}
		if c.opts.RespectRobots {
			allowed, err := c.robots.allowed(ctx, c.fetcher, c.opts.UserAgent, normalized)
			if err != nil || !allowed {
				c.bumpRobotsBlocked()
				continue
			}
		}
		if c.enqueue(normalized) {
			jobs = append(jobs, crawlJob{rawURL: normalized, depth: nextDepth})
		}
	}
	return jobs
}

// enqueue marks normalized as seen and claims a MaxPages slot. It reports
// false for duplicates and for URLs arriving after the page cap is reached.
// Duplicate encounters are counted in CrawlStats.
func (c *crawler) enqueue(normalized string) bool {
	c.seenMu.Lock()
	defer c.seenMu.Unlock()
	if c.seen[normalized] {
		c.statsMu.Lock()
		c.stats.Duplicates++
		c.statsMu.Unlock()
		return false
	}
	c.seen[normalized] = true
	if c.enqueued >= c.opts.MaxPages {
		return false
	}
	c.enqueued++
	return true
}

// hostOf extracts the lower-cased host from an already normalized URL.
func hostOf(rawurl string) string {
	parsed, err := url.Parse(rawurl)
	if err != nil {
		return ""
	}
	return parsed.Host
}

// nodeAttr returns the value of an element attribute, reporting presence.
func nodeAttr(n *html.Node, key string) (string, bool) {
	for _, attribute := range n.Attr {
		if attribute.Key == key {
			return attribute.Val, true
		}
	}
	return "", false
}

// snapshotStats returns a copy of the current statistics. Requests is the
// number of pages actually attempted, i.e. successes plus failures.
func (c *crawler) snapshotStats() CrawlStats {
	c.statsMu.Lock()
	defer c.statsMu.Unlock()
	stats := c.stats
	stats.Requests = stats.Success + stats.Failed
	return stats
}

func (c *crawler) bumpRobotsBlocked() {
	c.statsMu.Lock()
	c.stats.RobotsBlocked++
	c.statsMu.Unlock()
}
