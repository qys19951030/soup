package soup

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newCrawlTestServer serves a four-page site: index links to /a and /b, and
// /a links to /c. A handler hook lets individual tests override responses.
func newCrawlTestServer(t *testing.T, muxEdits ...func(*http.ServeMux)) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	pages := map[string]string{
		"/":           page("Index", "/", []string{"/a", "/b", "/a#section"}),
		"/a":          page("Page A", "/a", []string{"/c", "/relative/c"}),
		"/b":          page("Page B", "/b", nil),
		"/c":          page("Page C", "/c", nil),
		"/relative/c": page("Relative C", "/relative/c", nil),
	}
	for path, body := range pages {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = fmt.Fprint(w, body)
		})
	}
	for _, edit := range muxEdits {
		edit(mux)
	}
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func page(title, current string, links []string) string {
	var builder strings.Builder
	builder.WriteString("<!DOCTYPE html><html><head><title>" + title + "</title></head><body>")
	builder.WriteString("<h1>" + title + "</h1>")
	for _, link := range links {
		builder.WriteString(`<a href="` + link + `">` + link + `</a>`)
	}
	builder.WriteString("</body></html>")
	return builder.String()
}

func fastCrawlOptions() CrawlOptions {
	return CrawlOptions{
		MaxDepth:      3,
		MaxPages:      50,
		Concurrency:   4,
		SameDomain:    true,
		RespectRobots: false,
		UserAgent:     "soup-test",
		FetchOptions: FetchOptions{
			Timeout:            2 * time.Second,
			MaxBodyBytes:       1 << 20,
			MaxRetries:         1,
			BackoffBase:        time.Millisecond,
			MinIntervalPerHost: 0,
			MaxConcurrency:     4,
		},
	}
}

func urlsOf(pages []Page) []string {
	urls := make([]string, 0, len(pages))
	for _, page := range pages {
		urls = append(urls, page.URL)
	}
	return urls
}

func TestCrawlDepthLimits(t *testing.T) {
	ts := newCrawlTestServer(t)

	depth1 := fastCrawlOptions()
	depth1.MaxDepth = 1
	pages, stats, err := Crawl(context.Background(), []string{ts.URL + "/"}, depth1)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{ts.URL + "/", ts.URL + "/a", ts.URL + "/b"}, urlsOf(pages))
	for _, page := range pages {
		assert.LessOrEqual(t, page.Depth, 1)
		root := HTMLParse(string(page.Body))
		assert.Nil(t, root.Error)
		assert.NotEmpty(t, root.Find("title").Text())
	}
	assert.Equal(t, 3, stats.Requests)
	assert.Equal(t, 3, stats.Success)
	assert.Equal(t, 0, stats.Failed)

	depth2 := fastCrawlOptions()
	depth2.MaxDepth = 2
	pages, _, err = Crawl(context.Background(), []string{ts.URL + "/"}, depth2)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{
		ts.URL + "/", ts.URL + "/a", ts.URL + "/b",
		ts.URL + "/c", ts.URL + "/relative/c",
	}, urlsOf(pages))
}

func TestCrawlMaxPagesStopsEnqueue(t *testing.T) {
	ts := newCrawlTestServer(t)
	opts := fastCrawlOptions()
	opts.MaxPages = 3
	pages, stats, err := Crawl(context.Background(), []string{ts.URL + "/"}, opts)
	require.NoError(t, err)
	assert.Len(t, pages, 3)
	assert.Equal(t, 3, stats.Requests)
}

func TestCrawlPageFailureDoesNotAbort(t *testing.T) {
	ts := newCrawlTestServer(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/broken", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		})
	})
	opts := fastCrawlOptions()
	opts.MaxDepth = 1
	pages, stats, err := Crawl(context.Background(), []string{ts.URL + "/broken", ts.URL + "/b"}, opts)
	require.NoError(t, err)
	assert.Len(t, pages, 2)

	byURL := make(map[string]Page, len(pages))
	for _, page := range pages {
		byURL[page.URL] = page
	}
	broken := byURL[ts.URL+"/broken"]
	assert.Equal(t, http.StatusNotFound, broken.StatusCode)
	assert.Error(t, broken.Err)
	var statusErr *HTTPStatusError
	assert.True(t, errors.As(broken.Err, &statusErr))
	assert.Equal(t, http.StatusNotFound, statusErr.StatusCode)

	good := byURL[ts.URL+"/b"]
	assert.NoError(t, good.Err)
	assert.Equal(t, 1, stats.Failed)
	assert.Equal(t, 1, stats.Success)
	assert.Equal(t, 2, stats.Requests)
}

func TestCrawlDedupesAndCounts(t *testing.T) {
	ts := newCrawlTestServer(t)
	opts := fastCrawlOptions()
	// Seed duplicates and rely on the index linking to /a again.
	pages, stats, err := Crawl(context.Background(), []string{
		ts.URL + "/", ts.URL + "/", ts.URL + "/a#frag",
	}, opts)
	require.NoError(t, err)
	seen := make(map[string]bool)
	for _, page := range pages {
		assert.False(t, seen[page.URL], "duplicate page %s", page.URL)
		seen[page.URL] = true
	}
	assert.GreaterOrEqual(t, stats.Duplicates, 2)
	assert.Equal(t, stats.Requests, stats.Success+stats.Failed)
}

func TestCrawlSortedByDepthThenURL(t *testing.T) {
	ts := newCrawlTestServer(t)
	opts := fastCrawlOptions()
	opts.MaxDepth = 2
	pages, _, err := Crawl(context.Background(), []string{ts.URL + "/"}, opts)
	require.NoError(t, err)
	for i := 1; i < len(pages); i++ {
		prev, current := pages[i-1], pages[i]
		if prev.Depth == current.Depth {
			assert.LessOrEqual(t, prev.URL, current.URL)
		} else {
			assert.Less(t, prev.Depth, current.Depth)
		}
	}
}

func TestCrawlRobotsBlockingAndAllowOverride(t *testing.T) {
	ts := newCrawlTestServer(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
			_, _ = fmt.Fprint(w, "User-agent: soup-test\nDisallow: /b\nAllow: /b\nDisallow: /c\n")
		})
	})
	opts := fastCrawlOptions()
	opts.RespectRobots = true
	opts.MaxDepth = 2
	pages, stats, err := Crawl(context.Background(), []string{ts.URL + "/"}, opts)
	require.NoError(t, err)

	fetched := make(map[string]bool)
	for _, page := range pages {
		fetched[page.URL] = true
	}
	assert.True(t, fetched[ts.URL+"/b"], "Allow should override the Disallow for /b")
	assert.False(t, fetched[ts.URL+"/c"], "Disallow should block /c")
	assert.GreaterOrEqual(t, stats.RobotsBlocked, 1)
}

func TestCrawlRobotsFetchFailureAllowsAll(t *testing.T) {
	ts := newCrawlTestServer(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		})
	})
	opts := fastCrawlOptions()
	opts.RespectRobots = true
	opts.MaxDepth = 1
	pages, _, err := Crawl(context.Background(), []string{ts.URL + "/"}, opts)
	require.NoError(t, err)
	assert.Len(t, pages, 3)
}

func TestCrawlContextCancellation(t *testing.T) {
	release := make(chan struct{})
	handlerEntered := make(chan struct{}, 1)
	var inFlight int32
	ts := newCrawlTestServer(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&inFlight, 1)
			select {
			case handlerEntered <- struct{}{}:
			default:
			}
			<-release
		})
	})
	t.Cleanup(func() { close(release) })

	opts := fastCrawlOptions()
	opts.Concurrency = 1
	opts.FetchOptions.MaxConcurrency = 1
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, _, err := Crawl(ctx, []string{ts.URL + "/slow", ts.URL + "/b", ts.URL + "/c"}, opts)
		errCh <- err
	}()

	select {
	case <-handlerEntered:
	case <-time.After(time.Second):
		t.Fatal("slow handler never started")
	}
	cancel()
	select {
	case err := <-errCh:
		assert.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("crawl did not return within one second of cancellation")
	}
	// After cancellation every crawl goroutine must have exited.
	runtime.GC()
	var stackBuf [1 << 16]byte
	stack := string(stackBuf[:runtime.Stack(stackBuf[:], true)])
	assert.NotContains(t, stack, "dispatchLayer")
	assert.NotContains(t, stack, "processPageWithLinks")
}

func TestCrawlCancellationReturnsPartialPages(t *testing.T) {
	release := make(chan struct{})
	ts := newCrawlTestServer(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
			<-release
		})
	})
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	// /b is a fast page that should complete before the slow one is canceled.
	pages, _, err := Crawl(ctx, []string{ts.URL + "/slow", ts.URL + "/b"}, fastCrawlOptions())
	assert.True(t, errors.Is(err, context.Canceled), "expected context.Canceled, got %v", err)
	assert.NotEmpty(t, pages)
}

func TestCrawlNonHTTPLinksDropped(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body := `<html><body>` +
			`<a href="mailto:a@b.com">mail</a>` +
			`<a href="javascript:void(0)">js</a>` +
			`<a href="tel:123">tel</a>` +
			`<a href="/ok">ok</a>` +
			`</body></html>`
		_, _ = fmt.Fprint(w, body)
	})
	mux.HandleFunc("/ok", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, page("OK", "/ok", nil))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	opts := fastCrawlOptions()
	opts.MaxDepth = 1
	pages, _, err := Crawl(context.Background(), []string{ts.URL + "/"}, opts)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{ts.URL + "/", ts.URL + "/ok"}, urlsOf(pages))
}

func TestCrawlDifferentHostsProgressInParallel(t *testing.T) {
	// Two independent servers each block until both handlers have started,
	// proving requests to distinct hosts run concurrently despite a large
	// per-host minimum interval.
	bothStarted := make(chan struct{})
	var once sync.Once
	var active int32
	newParallelServer := func() *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if atomic.AddInt32(&active, 1) == 2 {
				once.Do(func() { close(bothStarted) })
			}
			select {
			case <-bothStarted:
			case <-time.After(2 * time.Second):
			}
			_, _ = fmt.Fprint(w, page("Host", "/", nil))
		}))
	}
	serverA := newParallelServer()
	serverB := newParallelServer()
	t.Cleanup(serverA.Close)
	t.Cleanup(serverB.Close)

	opts := fastCrawlOptions()
	opts.Concurrency = 2
	opts.FetchOptions.MaxConcurrency = 2

	done := make(chan struct{})
	go func() {
		_, _, _ = Crawl(context.Background(), []string{serverA.URL + "/", serverB.URL + "/"}, opts)
		close(done)
	}()
	select {
	case <-bothStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("requests to different hosts did not run in parallel")
	}
	<-done
}

func TestCrawlNormalizesDefaultPortAndFragment(t *testing.T) {
	ts := newCrawlTestServer(t)
	base := strings.TrimPrefix(ts.URL, "http://")
	portURL := "http://" + base + "/b#section"
	plainURL := "http://" + base + "/b"

	normalized, err := NormalizeURL(portURL, "")
	require.NoError(t, err)
	assert.Equal(t, plainURL, normalized)

	opts := fastCrawlOptions()
	pages, stats, err := Crawl(context.Background(), []string{portURL, plainURL}, opts)
	require.NoError(t, err)
	assert.Len(t, pages, 1)
	assert.GreaterOrEqual(t, stats.Duplicates, 1)
}

func TestCrawlBadSeedRecordedButCrawlContinues(t *testing.T) {
	ts := newCrawlTestServer(t)
	opts := fastCrawlOptions()
	pages, stats, err := Crawl(context.Background(), []string{
		"mailto:not-a-page", ts.URL + "/b",
	}, opts)
	require.NoError(t, err)
	assert.Len(t, pages, 2)
	assert.Equal(t, 1, stats.Failed)
	assert.Equal(t, 1, stats.Success)

	var hasBadSeed bool
	for _, page := range pages {
		if page.URL == "mailto:not-a-page" {
			hasBadSeed = true
			assert.Error(t, page.Err)
		}
	}
	assert.True(t, hasBadSeed)
}

func TestCrawlSameDomainDropsExternalHost(t *testing.T) {
	external := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(page("External", "/", nil)))
	}))
	t.Cleanup(external.Close)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body := `<html><body><a href="` + external.URL + `/x">external</a>` +
			`<a href="/local">local</a></body></html>`
		_, _ = w.Write([]byte(body))
	})
	mux.HandleFunc("/local", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(page("Local", "/local", nil)))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	opts := fastCrawlOptions()
	opts.SameDomain = true
	opts.MaxDepth = 1
	pages, _, err := Crawl(context.Background(), []string{ts.URL + "/"}, opts)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{ts.URL + "/", ts.URL + "/local"}, urlsOf(pages))

	// With SameDomain disabled the external host is followed.
	opts.SameDomain = false
	pages, _, err = Crawl(context.Background(), []string{ts.URL + "/"}, opts)
	require.NoError(t, err)
	found := false
	for _, page := range pages {
		if strings.HasPrefix(page.URL, external.URL) {
			found = true
		}
	}
	assert.True(t, found)
}
