package soup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// DefaultFetchOptions holds the values applied to zero-valued FetchOptions
// fields. They are conservative enough to be used directly for real crawl
// workloads.
var DefaultFetchOptions = FetchOptions{
	Timeout:            15 * time.Second,
	MaxBodyBytes:       10 << 20,
	MaxRetries:         3,
	BackoffBase:        500 * time.Millisecond,
	MinIntervalPerHost: 200 * time.Millisecond,
	MaxConcurrency:     8,
}

// FetchOptions configures the behavior of a Fetcher.
type FetchOptions struct {
	// Timeout bounds a single HTTP attempt, including connection, headers
	// and body reads. Zero means the DefaultFetchOptions value is used.
	Timeout time.Duration
	// MaxBodyBytes limits the accepted response body size. A larger body
	// produces ErrBodyTooLarge instead of a silently truncated response.
	MaxBodyBytes int64
	// MaxRetries is the number of additional attempts after the first one.
	MaxRetries int
	// BackoffBase is the base for exponential backoff: attempt k waits
	// BackoffBase * 2^k plus random jitter.
	BackoffBase time.Duration
	// MinIntervalPerHost enforces a minimum gap between the start times of
	// two consecutive requests targeting the same host.
	MinIntervalPerHost time.Duration
	// MaxConcurrency bounds the number of simultaneously in-flight requests
	// across all hosts.
	MaxConcurrency int
	// Headers are added to every request. Values in the package-level Headers
	// map are applied on top and take precedence.
	Headers http.Header
}

// Response is the result of a successful Fetcher.Get call.
type Response struct {
	// URL is the final URL after redirects.
	URL string
	// StatusCode is the final HTTP status code.
	StatusCode int
	// Headers are the final response headers.
	Headers http.Header
	// Body holds the complete response body.
	Body []byte
	// Attempts is the number of HTTP attempts actually performed.
	Attempts int
}

// HTTPStatusError reports a completed HTTP response whose status code is at
// least 400. It unwraps to the underlying error, if any.
type HTTPStatusError struct {
	URL        string
	StatusCode int
	Attempts   int
	Err        error
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("fetch %q returned status %d after %d attempt(s)", e.URL, e.StatusCode, e.Attempts)
}

// Unwrap exposes the underlying error to errors.Is/errors.As.
func (e *HTTPStatusError) Unwrap() error {
	return e.Err
}

// BodyLimitError reports that a response body exceeded the configured size
// limit.
type BodyLimitError struct {
	URL       string
	Limit     int64
	BytesRead int64
	Attempts  int
}

func (e *BodyLimitError) Error() string {
	return fmt.Sprintf("fetch %q body exceeded limit of %d bytes (read %d) after %d attempt(s)", e.URL, e.Limit, e.BytesRead, e.Attempts)
}

// ErrBodyTooLarge is the sentinel wrapped by BodyLimitError so callers can use
// errors.Is to detect oversized responses.
var ErrBodyTooLarge = errors.New("response body exceeded the configured size limit")

// Unwrap exposes ErrBodyTooLarge to errors.Is/errors.As.
func (e *BodyLimitError) Unwrap() error {
	return ErrBodyTooLarge
}

// Fetcher is a reusable, concurrency-safe HTTP client with retries, per-host
// rate limiting, body limits and a global concurrency cap.
type Fetcher struct {
	opts       FetchOptions
	client     *http.Client
	sem        chan struct{}
	hostMu     sync.Mutex
	hostStates map[string]*hostState
}

type hostState struct {
	mu     sync.Mutex
	lastAt time.Time
}

// NewFetcher builds a Fetcher from opts. Zero-valued fields are replaced with
// DefaultFetchOptions. The returned Fetcher is safe for concurrent use.
func NewFetcher(opts FetchOptions) *Fetcher {
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultFetchOptions.Timeout
	}
	if opts.MaxBodyBytes <= 0 {
		opts.MaxBodyBytes = DefaultFetchOptions.MaxBodyBytes
	}
	if opts.MaxRetries <= 0 {
		opts.MaxRetries = DefaultFetchOptions.MaxRetries
	}
	if opts.BackoffBase <= 0 {
		opts.BackoffBase = DefaultFetchOptions.BackoffBase
	}
	if opts.MinIntervalPerHost < 0 {
		opts.MinIntervalPerHost = DefaultFetchOptions.MinIntervalPerHost
	}
	if opts.MaxConcurrency <= 0 {
		opts.MaxConcurrency = DefaultFetchOptions.MaxConcurrency
	}
	client := &http.Client{
		Timeout: opts.Timeout,
	}
	return &Fetcher{
		opts:       opts,
		client:     client,
		sem:        make(chan struct{}, opts.MaxConcurrency),
		hostStates: make(map[string]*hostState),
	}
}

// hostStateFor returns the rate-limit state dedicated to a host.
func (f *Fetcher) hostStateFor(host string) *hostState {
	f.hostMu.Lock()
	defer f.hostMu.Unlock()
	state, ok := f.hostStates[host]
	if !ok {
		state = &hostState{}
		f.hostStates[host] = state
	}
	return state
}

// waitForHostSlot blocks until at least MinIntervalPerHost has elapsed since
// the previous request to host started, or until ctx is canceled.
func (f *Fetcher) waitForHostSlot(ctx context.Context, host string) error {
	if f.opts.MinIntervalPerHost <= 0 {
		return ctx.Err()
	}
	state := f.hostStateFor(host)
	state.mu.Lock()
	defer state.mu.Unlock()

	now := time.Now()
	if !state.lastAt.IsZero() {
		wait := f.opts.MinIntervalPerHost - now.Sub(state.lastAt)
		if wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
	state.lastAt = time.Now()
	return nil
}

// limitedReadAll reads from r while enforcing a maximum body size. It returns
// ErrBodyTooLarge (wrapped in a BodyLimitError by the caller) instead of
// silently truncating oversized bodies.
func limitedReadAll(r io.Reader, limit int64) ([]byte, int64, error) {
	buf := make([]byte, 0, min64(limit, 4096))
	chunk := make([]byte, 4096)
	var total int64
	for {
		n, err := r.Read(chunk)
		total += int64(n)
		if total > limit {
			return buf, total, ErrBodyTooLarge
		}
		buf = append(buf, chunk[:n]...)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return buf, total, nil
			}
			return buf, total, err
		}
	}
}

// min64 returns the smaller of two int64 values, used for initial capacity.
func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// isRetryableStatus reports whether an HTTP status code triggers a retry.
// Only 429 and 5xx responses are retryable.
func isRetryableStatus(statusCode int) bool {
	return statusCode == http.StatusTooManyRequests || statusCode >= 500
}

// backoff computes the wait before retry number retry (zero-based), adding
// full-range random jitter so concurrent clients do not synchronize.
func (f *Fetcher) backoff(retry int) time.Duration {
	base := float64(f.opts.BackoffBase) * float64(int64(1)<<retry)
	jitter := rand.Float64() * float64(f.opts.BackoffBase)
	return time.Duration(base + jitter)
}

// Get performs an HTTP GET against rawurl, applying retries, rate limits and
// the body size limit. A response with a status code of 400 or higher is
// returned through an *HTTPStatusError; an oversized body through a
// *BodyLimitError. Context cancellation interrupts any in-flight retry.
func (f *Fetcher) Get(ctx context.Context, rawurl string) (*Response, error) {
	parsed, err := url.Parse(rawurl)
	if err != nil {
		return nil, fmt.Errorf("fetch %q: %w", rawurl, err)
	}
	host := parsed.Hostname()

	var lastErr error
	for attempt := 0; attempt <= f.opts.MaxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := f.waitForHostSlot(ctx, host); err != nil {
			return nil, err
		}

		select {
		case f.sem <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		resp, attemptErr := f.doAttempt(ctx, rawurl, attempt+1)
		<-f.sem

		if attemptErr != nil {
			lastErr = attemptErr
			if attempt < f.opts.MaxRetries {
				if waitErr := f.waitBackoff(ctx, attempt); waitErr != nil {
					return nil, waitErr
				}
				continue
			}
			return nil, attemptErr
		}

		if isRetryableStatus(resp.StatusCode) && attempt < f.opts.MaxRetries {
			lastErr = &HTTPStatusError{
				URL:        resp.URL,
				StatusCode: resp.StatusCode,
				Attempts:   attempt + 1,
			}
			if waitErr := f.waitBackoff(ctx, attempt); waitErr != nil {
				return nil, waitErr
			}
			continue
		}

		if resp.StatusCode >= 400 {
			return nil, &HTTPStatusError{
				URL:        resp.URL,
				StatusCode: resp.StatusCode,
				Attempts:   attempt + 1,
				Err:        lastErr,
			}
		}
		resp.Attempts = attempt + 1
		return resp, nil
	}
	return nil, lastErr
}

// waitBackoff sleeps for the backoff duration of attempt, aborting early when
// ctx is canceled.
func (f *Fetcher) waitBackoff(ctx context.Context, attempt int) error {
	wait := f.backoff(attempt)
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// doAttempt performs one HTTP attempt and reads the full body while enforcing
// the configured size limit. Network errors and oversized bodies are returned
// as retryable errors; non-retryable 4xx statuses are returned as a Response
// so the caller can decide not to retry them.
func (f *Fetcher) doAttempt(ctx context.Context, rawurl string, attemptNumber int) (*Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawurl, nil)
	if err != nil {
		return nil, fmt.Errorf("fetch %q: %w", rawurl, err)
	}
	for key, values := range f.opts.Headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	setHeadersAndCookies(req)

	httpResp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %q: %w", rawurl, err)
	}
	defer httpResp.Body.Close()

	body, bytesRead, err := limitedReadAll(httpResp.Body, f.opts.MaxBodyBytes)
	if err != nil {
		if errors.Is(err, ErrBodyTooLarge) {
			return nil, &BodyLimitError{
				URL:       rawurl,
				Limit:     f.opts.MaxBodyBytes,
				BytesRead: bytesRead,
				Attempts:  attemptNumber,
			}
		}
		return nil, fmt.Errorf("fetch %q: %w", rawurl, err)
	}

	finalURL := rawurl
	if httpResp.Request != nil && httpResp.Request.URL != nil {
		finalURL = httpResp.Request.URL.String()
	}
	return &Response{
		URL:        finalURL,
		StatusCode: httpResp.StatusCode,
		Headers:    httpResp.Header.Clone(),
		Body:       body,
	}, nil
}
