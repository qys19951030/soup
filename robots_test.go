package soup

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRobotsDisallowAndAllowLongestMatch(t *testing.T) {
	contents := "User-agent: *\n" +
		"Disallow: /private\n" +
		"Allow: /private/public\n" +
		"Disallow: /admin\n"
	data := parseRobots(contents)

	assert.False(t, data.allowed("any-agent", "/private/secret"))
	assert.True(t, data.allowed("any-agent", "/private/public/page"))
	assert.False(t, data.allowed("any-agent", "/admin"))
	assert.True(t, data.allowed("any-agent", "/open"))
}

func TestRobotsSpecificAgentBeatsWildcard(t *testing.T) {
	contents := "User-agent: *\n" +
		"Disallow: /\n" +
		"User-agent: soup-bot\n" +
		"Allow: /\n" +
		"Disallow: /secret\n"
	data := parseRobots(contents)

	assert.True(t, data.allowed("soup-bot", "/home"))
	assert.False(t, data.allowed("soup-bot", "/secret"))
	assert.False(t, data.allowed("other-bot", "/home"))
}

func TestRobotsEqualLengthAllowWins(t *testing.T) {
	contents := "User-agent: *\n" +
		"Disallow: /same\n" +
		"Allow: /same\n"
	data := parseRobots(contents)
	assert.True(t, data.allowed("bot", "/same"))
}

func TestRobotsCommentsAndEmptyDisallow(t *testing.T) {
	contents := "# a comment\n" +
		"User-agent: *  # trailing comment\n" +
		"Disallow:\n" +
		"Disallow: /x # inline\n"
	data := parseRobots(contents)
	assert.True(t, data.allowed("bot", "/anything"))
	assert.False(t, data.allowed("bot", "/x/y"))
}

func TestRobotsMultipleAgentsShareGroup(t *testing.T) {
	contents := "User-agent: bot-a\n" +
		"User-agent: bot-b\n" +
		"Disallow: /shared\n"
	data := parseRobots(contents)
	assert.False(t, data.allowed("bot-a", "/shared"))
	assert.False(t, data.allowed("bot-b", "/shared"))
	assert.True(t, data.allowed("bot-c", "/shared"))
}

func TestRobotsEmptyFileAllowsEverything(t *testing.T) {
	data := parseRobots("")
	assert.True(t, data.allowed("bot", "/anything"))
}

func TestRobotsFetchedOncePerHostUnderConcurrency(t *testing.T) {
	var robotsCalls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/robots.txt":
			atomic.AddInt32(&robotsCalls, 1)
			time.Sleep(20 * time.Millisecond)
			_, _ = w.Write([]byte("User-agent: *\nDisallow: /private\n"))
		default:
			_, _ = w.Write([]byte("ok"))
		}
	}))
	defer ts.Close()

	fetcher := NewFetcher(FetchOptions{
		Timeout:            time.Second,
		MaxBodyBytes:       1 << 20,
		MaxRetries:         0,
		BackoffBase:        time.Millisecond,
		MinIntervalPerHost: 0,
		MaxConcurrency:     8,
	})
	cache := newRobotsCache()

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			path := "/allowed"
			if i%2 == 0 {
				path = "/private/x"
			}
			allowed, err := cache.allowed(context.Background(), fetcher, "bot", ts.URL+path)
			require.NoError(t, err)
			if i%2 == 0 {
				assert.False(t, allowed)
			} else {
				assert.True(t, allowed)
			}
		}(i)
	}
	wg.Wait()
	assert.Equal(t, int32(1), atomic.LoadInt32(&robotsCalls))
}
