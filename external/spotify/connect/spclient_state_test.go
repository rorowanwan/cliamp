package connect

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	connectpb "github.com/devgianlu/go-librespot/proto/spotify/connectstate"
)

type fakeRequester struct {
	mu        sync.Mutex
	responses []*http.Response
	requests  int
}

func (r *fakeRequester) Request(_ context.Context, _ string, _ string, _ url.Values, _ http.Header, _ []byte) (*http.Response, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests++
	response := r.responses[0]
	r.responses = r.responses[1:]
	return response, nil
}

func (r *fakeRequester) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.requests
}

func response(status int, retryAfter string) *http.Response {
	header := make(http.Header)
	if retryAfter != "" {
		header.Set("Retry-After", retryAfter)
	}
	return &http.Response{
		StatusCode: status,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(`{"message":"test"}`)),
	}
}

func TestStateClientDoesNotRetryRateLimitedRequests(t *testing.T) {
	requester := &fakeRequester{responses: []*http.Response{response(http.StatusTooManyRequests, "7")}}
	client := &spclientStateClient{client: requester, deviceID: "device-id"}
	err := client.PutConnectState(context.Background(), "connection-id", &connectpb.PutStateRequest{})

	var limited *rateLimitedError
	if !isRateLimited(err) || !errors.As(err, &limited) {
		t.Fatalf("PutConnectState() error = %v, want rateLimitedError", err)
	}
	if limited.retryAfter != 7*time.Second {
		t.Fatalf("Retry-After = %s, want 7s", limited.retryAfter)
	}
	if got := requester.count(); got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}
}

func TestParseRetryAfter(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{"seconds", "12", 12 * time.Second},
		{"missing", "", 0},
		{"invalid", "later", 0},
		{"zero", "0", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			header := make(http.Header)
			header.Set("Retry-After", tt.value)
			if got := parseRetryAfter(header); got != tt.want {
				t.Fatalf("parseRetryAfter(%q) = %s, want %s", tt.value, got, tt.want)
			}
		})
	}

	header := make(http.Header)
	header.Set("Retry-After", time.Now().Add(2*time.Second).UTC().Format(http.TimeFormat))
	if got := parseRetryAfter(header); got <= 0 || got > 2*time.Second {
		t.Fatalf("HTTP-date Retry-After = %s, want 0 < delay <= 2s", got)
	}
}

func TestRetryBackoffIsBounded(t *testing.T) {
	if got := nextRetryDelay(0); got != initialRetryBackoff {
		t.Fatalf("initial backoff = %s, want %s", got, initialRetryBackoff)
	}
	if got := nextRetryDelay(30 * time.Second); got != maximumRetryBackoff {
		t.Fatalf("bounded backoff = %s, want %s", got, maximumRetryBackoff)
	}
}
