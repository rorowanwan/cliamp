package connect

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/bjarneo/cliamp/internal/playback"
	"github.com/devgianlu/go-librespot/dealer"
	connectpb "github.com/devgianlu/go-librespot/proto/spotify/connectstate"
)

type fakeStateClient struct {
	mu        sync.Mutex
	puts      []*connectpb.PutStateRequest
	inactive  int
	putErrs   []error
	inactErrs []error
}

func (c *fakeStateClient) PutConnectState(_ context.Context, _ string, request *connectpb.PutStateRequest) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.puts = append(c.puts, request)
	if len(c.putErrs) > 0 {
		err := c.putErrs[0]
		c.putErrs = c.putErrs[1:]
		return err
	}
	return nil
}

func (c *fakeStateClient) PutConnectStateInactive(_ context.Context, _ string, _ bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inactive++
	if len(c.inactErrs) > 0 {
		err := c.inactErrs[0]
		c.inactErrs = c.inactErrs[1:]
		return err
	}
	return nil
}

func (c *fakeStateClient) snapshot() (int, int, *connectpb.PutStateRequest) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var last *connectpb.PutStateRequest
	if len(c.puts) > 0 {
		last = c.puts[len(c.puts)-1]
	}
	return len(c.puts), c.inactive, last
}

type fakeDealer struct {
	mu       sync.Mutex
	connects int
	messages chan dealer.Message
	requests chan dealer.Request
	msgPaths [][]string
	reqPaths []string
}

func newFakeDealer() *fakeDealer {
	return &fakeDealer{
		messages: make(chan dealer.Message, 1),
		requests: make(chan dealer.Request),
	}
}

func (d *fakeDealer) Connect(context.Context) error {
	d.mu.Lock()
	d.connects++
	d.mu.Unlock()
	return nil
}

func (d *fakeDealer) ReceiveMessage(paths ...string) <-chan dealer.Message {
	d.mu.Lock()
	d.msgPaths = append(d.msgPaths, paths)
	d.mu.Unlock()
	return d.messages
}

func (d *fakeDealer) ReceiveRequest(path string) <-chan dealer.Request {
	d.mu.Lock()
	d.reqPaths = append(d.reqPaths, path)
	d.mu.Unlock()
	return d.requests
}

func (d *fakeDealer) connectCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.connects
}

func (d *fakeDealer) subscriptions() ([]string, []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var messages []string
	for _, paths := range d.msgPaths {
		messages = append(messages, paths...)
	}
	return messages, append([]string(nil), d.reqPaths...)
}

func TestServicePublishesAfterPusherHandshake(t *testing.T) {
	client := &fakeStateClient{}
	dealer := newFakeDealer()
	service := New("test cliamp")
	defer service.Close()
	service.bind(endpoint{key: new(int), deviceID: "device-id", spclient: client, dealer: dealer})
	service.Update(spotifyPlaybackState())

	waitFor(t, func() bool { return dealer.connectCount() == 1 })
	messages, requests := dealer.subscriptions()
	if len(messages) != 2 || messages[0] != "hm://pusher/v1/connections/" || messages[1] != "hm://connect-state/v1/" {
		t.Fatalf("message subscriptions = %v", messages)
	}
	if len(requests) != 1 || requests[0] != playerCommandURI {
		t.Fatalf("request subscriptions = %v", requests)
	}
	if puts, _, _ := client.snapshot(); puts != 0 {
		t.Fatalf("puts before connection ID = %d, want 0", puts)
	}

	dealer.messages <- dealerMessage("connection-id")
	waitFor(t, func() bool {
		puts, _, _ := client.snapshot()
		return puts == 1
	})
	_, _, request := client.snapshot()
	if request.PutStateReason != connectpb.PutStateReason_NEW_DEVICE {
		t.Fatalf("initial reason = %s, want NEW_DEVICE", request.PutStateReason)
	}
	if got := request.Device.DeviceInfo.Name; got != "test cliamp" {
		t.Fatalf("device name = %q", got)
	}

	service.Update(playback.State{Status: playback.StatusPlaying, Track: playback.Track{URL: "https://example.test/radio"}})
	waitFor(t, func() bool {
		_, inactive, _ := client.snapshot()
		return inactive == 1
	})
}

func TestServiceCoalescesUpdates(t *testing.T) {
	client := &fakeStateClient{}
	dealer := newFakeDealer()
	service := New("cliamp")
	defer service.Close()
	service.bind(endpoint{key: new(int), deviceID: "device-id", spclient: client, dealer: dealer})
	service.Update(spotifyPlaybackState())
	waitFor(t, func() bool { return dealer.connectCount() == 1 })
	dealer.messages <- dealerMessage("connection-id")
	waitFor(t, func() bool {
		puts, _, _ := client.snapshot()
		return puts == 1
	})

	state := spotifyPlaybackState()
	state.VolumeDB = -3
	service.Update(state)
	state.VolumeDB = -12
	service.Update(state)
	time.Sleep(minPutInterval / 2)
	if puts, _, _ := client.snapshot(); puts != 1 {
		t.Fatalf("puts during throttle interval = %d, want 1", puts)
	}
	waitFor(t, func() bool {
		puts, _, _ := client.snapshot()
		return puts == 2
	})
	_, _, request := client.snapshot()
	if got := request.Device.DeviceInfo.Volume; got != dbToStateVolume(-12) {
		t.Fatalf("coalesced volume = %d, want %d", got, dbToStateVolume(-12))
	}
}

func TestServiceIgnoresPlaybackClockUpdates(t *testing.T) {
	client := &fakeStateClient{}
	dealer := newFakeDealer()
	service := New("cliamp")
	defer service.Close()
	service.bind(endpoint{key: new(int), deviceID: "device-id", spclient: client, dealer: dealer})
	service.Update(spotifyPlaybackState())
	waitFor(t, func() bool { return dealer.connectCount() == 1 })
	dealer.messages <- dealerMessage("connection-id")
	waitFor(t, func() bool {
		puts, _, _ := client.snapshot()
		return puts == 1
	})

	for _, position := range []time.Duration{2 * time.Second, 3 * time.Second, 4 * time.Second} {
		state := spotifyPlaybackState()
		state.Position = position
		service.Update(state)
	}
	time.Sleep(minPutInterval + 100*time.Millisecond)
	if puts, _, _ := client.snapshot(); puts != 1 {
		t.Fatalf("puts after playback clock updates = %d, want 1", puts)
	}
}

func TestServicePublishesExplicitSeek(t *testing.T) {
	client := &fakeStateClient{}
	dealer := newFakeDealer()
	service := New("cliamp")
	defer service.Close()
	service.bind(endpoint{key: new(int), deviceID: "device-id", spclient: client, dealer: dealer})
	service.Update(spotifyPlaybackState())
	waitFor(t, func() bool { return dealer.connectCount() == 1 })
	dealer.messages <- dealerMessage("connection-id")
	waitFor(t, func() bool {
		puts, _, _ := client.snapshot()
		return puts == 1
	})

	service.Seeked(42 * time.Second)
	waitFor(t, func() bool {
		puts, _, _ := client.snapshot()
		return puts == 2
	})
	_, _, request := client.snapshot()
	if got := request.Device.PlayerState.PositionAsOfTimestamp; got != 42_000 {
		t.Fatalf("seek position = %d, want 42000", got)
	}
}

func TestServiceRateLimitCoalescesAndRecovers(t *testing.T) {
	client := &fakeStateClient{putErrs: []error{
		&rateLimitedError{retryAfter: 300 * time.Millisecond, err: context.DeadlineExceeded},
	}}
	dealer := newFakeDealer()
	service := New("cliamp")
	defer service.Close()
	service.bind(endpoint{key: new(int), deviceID: "device-id", spclient: client, dealer: dealer})
	service.Update(spotifyPlaybackState())
	waitFor(t, func() bool { return dealer.connectCount() == 1 })
	dealer.messages <- dealerMessage("connection-id")
	waitFor(t, func() bool {
		puts, _, _ := client.snapshot()
		return puts == 1
	})

	state := spotifyPlaybackState()
	state.VolumeDB = -9
	service.Update(state)
	time.Sleep(150 * time.Millisecond)
	if puts, _, _ := client.snapshot(); puts != 1 {
		t.Fatalf("puts during Retry-After cooldown = %d, want 1", puts)
	}
	waitFor(t, func() bool {
		puts, _, _ := client.snapshot()
		return puts == 2
	})
	_, _, request := client.snapshot()
	if got := request.Device.DeviceInfo.Volume; got != dbToStateVolume(-9) {
		t.Fatalf("recovered volume = %d, want %d", got, dbToStateVolume(-9))
	}
}

func TestServiceRateLimitWithoutRetryAfterUsesFallback(t *testing.T) {
	client := &fakeStateClient{putErrs: []error{
		&rateLimitedError{err: context.DeadlineExceeded},
	}}
	dealer := newFakeDealer()
	service := New("cliamp")
	defer service.Close()
	service.bind(endpoint{key: new(int), deviceID: "device-id", spclient: client, dealer: dealer})
	service.Update(spotifyPlaybackState())
	waitFor(t, func() bool { return dealer.connectCount() == 1 })
	dealer.messages <- dealerMessage("connection-id")
	waitFor(t, func() bool {
		puts, _, _ := client.snapshot()
		return puts == 1
	})

	service.mu.Lock()
	retryAt, backoff, rateLimited := service.retryAt, service.retryBackoff, service.rateLimited
	service.mu.Unlock()
	if !rateLimited || backoff != initialRetryBackoff {
		t.Fatalf("fallback state = rate_limited:%t backoff:%s, want true/%s", rateLimited, backoff, initialRetryBackoff)
	}
	if remaining := time.Until(retryAt); remaining <= 0 || remaining > initialRetryBackoff {
		t.Fatalf("fallback remaining = %s, want 0 < remaining <= %s", remaining, initialRetryBackoff)
	}
}

func TestServiceRebindPublishesNewDeviceState(t *testing.T) {
	firstClient, firstDealer := &fakeStateClient{}, newFakeDealer()
	service := New("cliamp")
	defer service.Close()
	service.bind(endpoint{key: new(int), deviceID: "first", spclient: firstClient, dealer: firstDealer})
	service.Update(spotifyPlaybackState())
	waitFor(t, func() bool { return firstDealer.connectCount() == 1 })
	firstDealer.messages <- dealerMessage("first-connection")
	waitFor(t, func() bool {
		puts, _, _ := firstClient.snapshot()
		return puts == 1
	})

	secondClient, secondDealer := &fakeStateClient{}, newFakeDealer()
	service.bind(endpoint{key: new(int), deviceID: "second", spclient: secondClient, dealer: secondDealer})
	waitFor(t, func() bool { return secondDealer.connectCount() == 1 })
	secondDealer.messages <- dealerMessage("second-connection")
	waitFor(t, func() bool {
		puts, _, _ := secondClient.snapshot()
		return puts == 1
	})
	_, _, request := secondClient.snapshot()
	if request.PutStateReason != connectpb.PutStateReason_NEW_DEVICE {
		t.Fatalf("rebound reason = %s, want NEW_DEVICE", request.PutStateReason)
	}
}

func spotifyPlaybackState() playback.State {
	return playback.State{
		Status:   playback.StatusPlaying,
		Position: time.Second,
		Track: playback.Track{
			URL:      "spotify:track:0123456789ABCDEFGHIJKL",
			Duration: 3 * time.Minute,
		},
	}
}

func dealerMessage(connectionID string) dealer.Message {
	return dealer.Message{
		Uri:     "hm://pusher/v1/connections/test",
		Headers: map[string]string{"Spotify-Connection-Id": connectionID},
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for condition")
}
