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
	mu       sync.Mutex
	puts     []*connectpb.PutStateRequest
	inactive int
}

func (c *fakeStateClient) PutConnectState(_ context.Context, _ string, request *connectpb.PutStateRequest) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.puts = append(c.puts, request)
	return nil
}

func (c *fakeStateClient) PutConnectStateInactive(_ context.Context, _ string, _ bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inactive++
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
}

func newFakeDealer() *fakeDealer {
	return &fakeDealer{messages: make(chan dealer.Message, 1)}
}

func (d *fakeDealer) Connect(context.Context) error {
	d.mu.Lock()
	d.connects++
	d.mu.Unlock()
	return nil
}

func (d *fakeDealer) ReceiveMessage(...string) <-chan dealer.Message { return d.messages }

func (d *fakeDealer) connectCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.connects
}

func TestServicePublishesAfterPusherHandshake(t *testing.T) {
	client := &fakeStateClient{}
	dealer := newFakeDealer()
	service := New("test cliamp")
	defer service.Close()
	service.bind(endpoint{key: new(int), deviceID: "device-id", spclient: client, dealer: dealer})
	service.Update(spotifyPlaybackState())

	waitFor(t, func() bool { return dealer.connectCount() == 1 })
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

	service.Update(playback.State{
		Status:   playback.StatusPlaying,
		Position: 10 * time.Second,
		Track:    playback.Track{URL: "spotify:track:0123456789ABCDEFGHIJKL"},
	})
	time.Sleep(minPutInterval / 2)
	if puts, _, _ := client.snapshot(); puts != 1 {
		t.Fatalf("puts during throttle interval = %d, want 1", puts)
	}
	waitFor(t, func() bool {
		puts, _, _ := client.snapshot()
		return puts == 2
	})
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
	return dealer.Message{Headers: map[string]string{"Spotify-Connection-Id": connectionID}}
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
