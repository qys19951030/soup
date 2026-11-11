package soup

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fastFetchOptions() FetchOptions {
	return FetchOptions{
		Timeout:            2 * time.Second,
		MaxBodyBytes:       1 << 20,
		MaxRetries:         3,
		BackoffBase:        time.Millisecond,
		MinIntervalPerHost: 0,
		MaxConcurrency:     4,
	}
}

func TestFetcherGetSuccess(t *testing.T) {
	var gotUA string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		w.Header().Set("X-Test", "yes")
		_, _ = w.Write([]byte("hello body"))
	}))
	defer ts.Close()

	opts := fastFetchOptions()
	opts.Headers = http.Header{"User-Agent": []string{"soup-test-agent"}}
	fetcher := NewFetcher(opts)
	resp, err := fetcher.Get(context.Background(), ts.URL)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "hello body", string(resp.Body))
	assert.Equal(t, 1, resp.Attempts)
	assert.Equal(t, ts.URL, resp.URL)
	assert.Equal(t, "yes", resp.Headers.Get("X-Test"))
	assert.Equal(t, "soup-test-agent", gotUA)
}

func TestFetcherStatusErrorNoRetryOn4xx(t *testing.T) {
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("missing"))
	}))
	defer ts.Close()

	fetcher := NewFetcher(fastFetchOptions())
	resp, err := fetcher.Get(context.Background(), ts.URL)
	require.Error(t, err)
	assert.Nil(t, resp)
	assert.Equal(t, int32(1), atomic.LoadInt32(&calls))

	var statusErr *HTTPStatusError
	require.True(t, errors.As(err, &statusErr))
	assert.Equal(t, http.StatusNotFound, statusErr.StatusCode)
	assert.Equal(t, 1, statusErr.Attempts)
	assert.Equal(t, ts.URL, statusErr.URL)
}

func TestFetcherRetriesOn429And5xx(t *testing.T) {
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer ts.Close()

	fetcher := NewFetcher(fastFetchOptions())
	_, err := fetcher.Get(context.Background(), ts.URL)
	require.Error(t, err)
	var statusErr *HTTPStatusError
	require.True(t, errors.As(err, &statusErr))
	assert.Equal(t, http.StatusTooManyRequests, statusErr.StatusCode)
	assert.Equal(t, 4, statusErr.Attempts)
	assert.Equal(t, int32(4), atomic.LoadInt32(&calls))
}

func TestFetcherRetriesThenSucceeds(t *testing.T) {
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte("recovered"))
	}))
	defer ts.Close()

	fetcher := NewFetcher(fastFetchOptions())
	resp, err := fetcher.Get(context.Background(), ts.URL)
	require.NoError(t, err)
	assert.Equal(t, "recovered", string(resp.Body))
	assert.Equal(t, 3, resp.Attempts)
}

func TestFetcherBodyLimit(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 1000)))
	}))
	defer ts.Close()

	opts := fastFetchOptions()
	opts.MaxBodyBytes = 100
	opts.MaxRetries = 1
	fetcher := NewFetcher(opts)
	_, err := fetcher.Get(context.Background(), ts.URL)
	require.Error(t, err)
	var bodyErr *BodyLimitError
	require.True(t, errors.As(err, &bodyErr))
	assert.True(t, errors.Is(err, ErrBodyTooLarge))
	assert.Equal(t, int64(100), bodyErr.Limit)
	assert.GreaterOrEqual(t, bodyErr.BytesRead, int64(101))
}

func TestFetcherContextCancellation(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
	}))
	defer ts.Close()
	defer close(release)

	fetcher := NewFetcher(fastFetchOptions())
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := fetcher.Get(ctx, ts.URL)
		errCh <- err
	}()
	<-started
	cancel()

	select {
	case err := <-errCh:
		assert.True(t, errors.Is(err, context.Canceled), "expected context.Canceled, got %v", err)
	case <-time.After(time.Second):
		t.Fatal("Get did not return within one second of cancellation")
	}
}

func TestFetcherPerHostInterval(t *testing.T) {
	var mu sync.Mutex
	var starts []time.Time
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		starts = append(starts, time.Now())
		mu.Unlock()
		_, _ = w.Write([]byte("ok"))
	}))
	defer ts.Close()

	opts := fastFetchOptions()
	opts.MinIntervalPerHost = 80 * time.Millisecond
	fetcher := NewFetcher(opts)

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := fetcher.Get(context.Background(), ts.URL)
			assert.NoError(t, err)
		}()
	}
	wg.Wait()

	require.Len(t, starts, 3)
	for i := 1; i < len(starts); i++ {
		gap := starts[i].Sub(starts[i-1])
		assert.GreaterOrEqual(t, int64(gap/time.Millisecond), int64(70))
	}
}

func TestFetcherGlobalConcurrencyCap(t *testing.T) {
	var inFlight int32
	var maxInFlight int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := atomic.AddInt32(&inFlight, 1)
		for {
			max := atomic.LoadInt32(&maxInFlight)
			if current <= max || atomic.CompareAndSwapInt32(&maxInFlight, max, current) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		atomic.AddInt32(&inFlight, -1)
		_, _ = w.Write([]byte("ok"))
	}))
	defer ts.Close()

	opts := fastFetchOptions()
	opts.MaxConcurrency = 2
	fetcher := NewFetcher(opts)

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := fetcher.Get(context.Background(), ts.URL)
			assert.NoError(t, err)
		}()
	}
	wg.Wait()
	assert.LessOrEqual(t, atomic.LoadInt32(&maxInFlight), int32(2))
}

func TestNewFetcherAppliesDefaults(t *testing.T) {
	fetcher := NewFetcher(FetchOptions{})
	assert.Equal(t, DefaultFetchOptions.Timeout, fetcher.opts.Timeout)
	assert.Equal(t, DefaultFetchOptions.MaxBodyBytes, fetcher.opts.MaxBodyBytes)
	assert.Equal(t, DefaultFetchOptions.MaxRetries, fetcher.opts.MaxRetries)
	assert.Equal(t, DefaultFetchOptions.BackoffBase, fetcher.opts.BackoffBase)
	assert.Equal(t, DefaultFetchOptions.MinIntervalPerHost, fetcher.opts.MinIntervalPerHost)
	assert.Equal(t, DefaultFetchOptions.MaxConcurrency, fetcher.opts.MaxConcurrency)
}

func TestFetcherRetriesNetworkErrors(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	ts.Close()

	opts := fastFetchOptions()
	opts.MaxRetries = 2
	fetcher := NewFetcher(opts)
	start := time.Now()
	_, err := fetcher.Get(context.Background(), ts.URL)
	require.Error(t, err)
	assert.Less(t, time.Since(start).Milliseconds(), int64(2000))
}
