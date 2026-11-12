package soup

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEndToEndCrawlParseExtract exercises the full pipeline against the
// four-page site described in the acceptance criteria: index -> A/B, A -> C.
func TestEndToEndCrawlParseExtract(t *testing.T) {
	mux := http.NewServeMux()
	site := map[string]struct {
		title string
		links []string
	}{
		"/":  {"Home", []string{"/a", "/b"}},
		"/a": {"Page A", []string{"/c"}},
		"/b": {"Page B", nil},
		"/c": {"Page C", nil},
	}
	for path, page := range site {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			html := fmt.Sprintf(`<html><head><title>%s</title></head><body>`, page.title)
			for _, link := range page.links {
				html += `<a class="nav" href="` + link + `">` + link + `</a>`
			}
			html += `</body></html>`
			_, _ = fmt.Fprint(w, html)
		})
	}
	ts := httptest.NewServer(mux)
	defer ts.Close()

	opts := CrawlOptions{
		MaxDepth:      2,
		MaxPages:      20,
		Concurrency:   2,
		SameDomain:    true,
		RespectRobots: false,
		UserAgent:     "soup-e2e",
		FetchOptions: FetchOptions{
			Timeout:            time.Second,
			MaxBodyBytes:       1 << 20,
			MaxRetries:         1,
			BackoffBase:        time.Millisecond,
			MinIntervalPerHost: 0,
			MaxConcurrency:     2,
		},
	}

	pages, stats, err := Crawl(context.Background(), []string{ts.URL + "/"}, opts)
	require.NoError(t, err)
	require.Len(t, pages, 4)
	assert.Equal(t, 4, stats.Requests)
	assert.Equal(t, 4, stats.Success)
	assert.Equal(t, 0, stats.Failed)

	extractor := NewExtractor([]Field{
		{Name: "title", Tag: "title", Mode: ValueText, Required: true},
		{Name: "nav", Tag: "a", Attrs: map[string]string{"class": "nav"},
			Mode: ValueAttr, Attribute: "href", Multiple: true, AbsoluteURL: true},
	})

	titles := make(map[string]string)
	for _, page := range pages {
		root := HTMLParse(string(page.Body))
		require.Nil(t, root.Error, page.URL)
		values, extractErr := extractor.Extract(root, page.URL)
		require.NoError(t, extractErr)
		titles[page.URL] = values["title"].(string)
	}
	assert.Equal(t, "Home", titles[ts.URL+"/"])
	assert.Equal(t, "Page A", titles[ts.URL+"/a"])
	assert.Equal(t, "Page C", titles[ts.URL+"/c"])
}

// TestEndToEndBodyLimitWithinCrawl verifies an oversized page is recorded as a
// page error while the rest of the crawl continues.
func TestEndToEndBodyLimitWithinCrawl(t *testing.T) {
	ts := newCrawlTestServer(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/large", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(strings.Repeat("z", 5000)))
		})
	})
	opts := fastCrawlOptions()
	opts.FetchOptions.MaxBodyBytes = 1000
	opts.MaxDepth = 1
	pages, stats, err := Crawl(context.Background(), []string{ts.URL + "/large", ts.URL + "/b"}, opts)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Failed)
	assert.Equal(t, 1, stats.Success)

	var sawBodyLimit bool
	for _, page := range pages {
		if page.Err != nil {
			var bodyErr *BodyLimitError
			sawBodyLimit = errors.As(page.Err, &bodyErr)
		}
	}
	assert.True(t, sawBodyLimit)
}
