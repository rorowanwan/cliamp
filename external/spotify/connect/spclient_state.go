package connect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	connectpb "github.com/devgianlu/go-librespot/proto/spotify/connectstate"
	"github.com/devgianlu/go-librespot/spclient"
	"google.golang.org/protobuf/proto"
)

const (
	maxStateRequestAttempts = 3
	stateRequestRetryDelay  = time.Second
)

// spclientRequester is the part of Spclient required to backport the
// connect-state 429 handling added upstream after v0.7.1.
type spclientRequester interface {
	Request(context.Context, string, string, url.Values, http.Header, []byte) (*http.Response, error)
}

type spclientStateClient struct {
	client   spclientRequester
	deviceID string
}

func newSpclientStateClient(client *spclient.Spclient, deviceID string) stateClient {
	return &spclientStateClient{client: client, deviceID: deviceID}
}

func (c *spclientStateClient) PutConnectState(ctx context.Context, connectionID string, request *connectpb.PutStateRequest) error {
	body, err := proto.Marshal(request)
	if err != nil {
		return fmt.Errorf("marshal PutStateRequest: %w", err)
	}
	return c.request(ctx, fmt.Sprintf("/connect-state/v1/devices/%s", c.deviceID), nil, connectionID, body, http.StatusOK)
}

func (c *spclientStateClient) PutConnectStateInactive(ctx context.Context, connectionID string, notify bool) error {
	return c.request(ctx, fmt.Sprintf("/connect-state/v1/devices/%s/inactive", c.deviceID), url.Values{"notify": []string{strconv.FormatBool(notify)}}, connectionID, nil, http.StatusNoContent)
}

func (c *spclientStateClient) request(ctx context.Context, path string, query url.Values, connectionID string, body []byte, wantStatus int) error {
	var lastErr error
	for attempt := 0; attempt < maxStateRequestAttempts; attempt++ {
		resp, err := c.client.Request(ctx, http.MethodPut, path, query, http.Header{
			"X-Spotify-Connection-Id": []string{connectionID},
			"Content-Type":            []string{"application/x-protobuf"},
		}, body)
		if err == nil {
			err = stateResponseError(resp, wantStatus)
		}
		if err == nil {
			return nil
		}
		lastErr = err

		if isClientStateError(err) || isRateLimited(err) || attempt == maxStateRequestAttempts-1 {
			return err
		}
		if err := waitForRetry(ctx, stateRequestRetryDelay); err != nil {
			return fmt.Errorf("wait to retry connect state: %w", err)
		}
	}
	return lastErr
}

func stateResponseError(resp *http.Response, wantStatus int) error {
	if resp == nil {
		return fmt.Errorf("empty connect state response")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == wantStatus {
		return nil
	}

	var response struct {
		Message string `json:"message"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&response)
	if response.Message == "" {
		response.Message = http.StatusText(resp.StatusCode)
	}
	err := &stateHTTPError{
		status: resp.StatusCode,
		err:    fmt.Errorf("put state request failed with status %d: %s", resp.StatusCode, response.Message),
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return &rateLimitedError{retryAfter: parseRetryAfter(resp.Header), err: err}
	}
	return err
}

func isClientStateError(err error) bool {
	var responseErr *stateHTTPError
	return errors.As(err, &responseErr) && responseErr.status >= http.StatusBadRequest && responseErr.status < http.StatusInternalServerError
}

func isRateLimited(err error) bool {
	var limited *rateLimitedError
	return errors.As(err, &limited)
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// rateLimitedError is the Connect-local equivalent of the typed error added
// to go-librespot after v0.7.1. A zero retryAfter means the server omitted or
// sent an unusable Retry-After header.
type rateLimitedError struct {
	retryAfter time.Duration
	err        error
}

func (e *rateLimitedError) Error() string { return e.err.Error() }
func (e *rateLimitedError) Unwrap() error { return e.err }

type stateHTTPError struct {
	status int
	err    error
}

func (e *stateHTTPError) Error() string { return e.err.Error() }
func (e *stateHTTPError) Unwrap() error { return e.err }

func parseRetryAfter(header http.Header) time.Duration {
	value := strings.TrimSpace(header.Get("Retry-After"))
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds > 0 {
			return time.Duration(seconds) * time.Second
		}
		return 0
	}
	if retryAt, err := http.ParseTime(value); err == nil {
		if delay := time.Until(retryAt); delay > 0 {
			return delay
		}
	}
	return 0
}
