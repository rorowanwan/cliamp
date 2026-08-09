package connect

import (
	"math"
	"testing"
	"time"

	"github.com/bjarneo/cliamp/internal/playback"
	connectpb "github.com/devgianlu/go-librespot/proto/spotify/connectstate"
)

func TestNewPutStateRequest(t *testing.T) {
	state := playback.State{
		Status:   playback.StatusPaused,
		VolumeDB: -6.0206,
		Position: 42 * time.Second,
		Track: playback.Track{
			URL:         "spotify:track:0123456789ABCDEFGHIJKL",
			Title:       "Song",
			Artist:      "Artist",
			Album:       "Album",
			Genre:       "Electronic",
			TrackNumber: 3,
			Duration:    3*time.Minute + 17*time.Second,
		},
	}

	request := newPutStateRequest(newDeviceInfo("test cliamp", "device-id"), state, connectpb.PutStateReason_NEW_DEVICE)
	if !request.IsActive {
		t.Fatal("paused Spotify playback should be active")
	}
	if request.PutStateReason != connectpb.PutStateReason_NEW_DEVICE {
		t.Fatalf("reason = %s, want NEW_DEVICE", request.PutStateReason)
	}
	if got := request.Device.DeviceInfo.Name; got != "test cliamp" {
		t.Fatalf("device name = %q", got)
	}
	player := request.Device.PlayerState
	if !player.IsPlaying || !player.IsPaused || player.PlaybackSpeed != 0 {
		t.Fatalf("paused state = playing:%t paused:%t speed:%v", player.IsPlaying, player.IsPaused, player.PlaybackSpeed)
	}
	if got := player.PositionAsOfTimestamp; got != 42_000 {
		t.Fatalf("position = %d, want 42000", got)
	}
	if got := player.Duration; got != 197_000 {
		t.Fatalf("duration = %d, want 197000", got)
	}
	if got := player.Track.Metadata["title"]; got != "Song" {
		t.Fatalf("track title = %q", got)
	}
	if got := player.Track.Metadata["artist_name"]; got != "Artist" {
		t.Fatalf("track artist = %q", got)
	}
	if got := request.Device.DeviceInfo.Volume; got < 32_700 || got > 32_850 {
		t.Fatalf("volume = %d, want approximately half-scale", got)
	}
}

func TestSpotifyStateAndVolume(t *testing.T) {
	if !isSpotifyState(playback.State{Track: playback.Track{URL: "spotify:episode:0123456789ABCDEFGHIJKL"}}) {
		t.Fatal("Spotify episode was not recognized")
	}
	if isSpotifyState(playback.State{Track: playback.Track{URL: "https://example.test/radio"}}) {
		t.Fatal("non-Spotify URL was recognized")
	}
	if got := dbToStateVolume(math.Inf(-1)); got != 0 {
		t.Fatalf("-Inf volume = %d, want 0", got)
	}
	if got := dbToStateVolume(6); got != maxStateVolume {
		t.Fatalf("positive dB volume = %d, want %d", got, maxStateVolume)
	}
}

func TestDeviceInfoAdvertisesRemoteControlCapabilities(t *testing.T) {
	device := newDeviceInfo("cliamp", "device-id")
	if !device.CanPlay {
		t.Fatal("CanPlay = false, want true")
	}
	if device.Volume != maxStateVolume || device.Capabilities.VolumeSteps != maxStateVolume {
		t.Fatalf("volume capability = %d/%d, want %d", device.Volume, device.Capabilities.VolumeSteps, maxStateVolume)
	}

	capabilities := device.Capabilities
	if !capabilities.CanBePlayer || !capabilities.IsControllable || !capabilities.CommandAcks || !capabilities.SupportsCommandRequest || !capabilities.SupportsTransferCommand || capabilities.DisableVolume || capabilities.ConnectDisabled {
		t.Fatalf("remote-control capabilities = %#v", capabilities)
	}
}
