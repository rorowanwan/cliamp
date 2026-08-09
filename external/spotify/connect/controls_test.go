package connect

import (
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/bjarneo/cliamp/internal/playback"
	"github.com/devgianlu/go-librespot/dealer"
	connectpb "github.com/devgianlu/go-librespot/proto/spotify/connectstate"
	"google.golang.org/protobuf/proto"
)

func TestCommandMessage(t *testing.T) {
	tests := []struct {
		name      string
		endpoint  string
		configure func(*dealer.RequestPayload)
		want      any
	}{
		{"play", "play", nil, playback.PlayMsg{}},
		{"resume", "resume", nil, playback.PlayMsg{}},
		{"pause", "pause", nil, playback.PauseMsg{}},
		{"next", "skip_next", nil, playback.NextMsg{}},
		{"previous", "skip_prev", nil, playback.PrevMsg{}},
		{
			"relative seek", "seek_to",
			func(req *dealer.RequestPayload) { req.Command.Relative, req.Command.Position = "current", 1_500 },
			playback.SeekMsg{Offset: 1500 * time.Millisecond},
		},
		{
			"absolute seek", "seek_to",
			func(req *dealer.RequestPayload) { req.Command.Relative, req.Command.Position = "beginning", 42_000 },
			playback.SetPositionMsg{Position: 42 * time.Second},
		},
		{
			"value seek", "seek_to",
			func(req *dealer.RequestPayload) { req.Command.Value = float64(5_000) },
			playback.SetPositionMsg{Position: 5 * time.Second},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var req dealer.RequestPayload
			req.Command.Endpoint = tt.endpoint
			if tt.configure != nil {
				tt.configure(&req)
			}
			got, err := commandMessage(req)
			if err != nil {
				t.Fatalf("commandMessage() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("commandMessage() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestTransferMessage(t *testing.T) {
	transfer := &connectpb.TransferState{
		Playback: &connectpb.Playback{
			PositionAsOfTimestamp: 42_000,
			IsPaused:              true,
			CurrentTrack: &connectpb.ContextTrack{
				Uri: "spotify:track:0123456789ABCDEFGHIJKL",
				Metadata: map[string]string{
					"title":        "Song",
					"artist_name":  "Artist",
					"album_title":  "Album",
					"duration":     "180000",
					"track_number": "3",
				},
			},
		},
		Queue: &connectpb.Queue{Tracks: []*connectpb.ContextTrack{{
			Uri: "spotify:track:ABCDEFGHIJKL0123456789",
		}}},
	}
	data, err := proto.Marshal(transfer)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	message, err := transferMessage(data, "")
	if err != nil {
		t.Fatalf("transferMessage() error = %v", err)
	}
	got := message.(playback.TransferMsg)
	if len(got.Tracks) != 2 {
		t.Fatalf("tracks = %d, want 2", len(got.Tracks))
	}
	if got.Tracks[0].Title != "Song" || got.Tracks[0].Duration != 3*time.Minute {
		t.Fatalf("current track = %#v", got.Tracks[0])
	}
	if got.Position != 42*time.Second || !got.Paused {
		t.Fatalf("transfer = %#v, want paused at 42s", got)
	}

	message, err = transferMessage(data, "resume")
	if err != nil {
		t.Fatalf("transferMessage(resume) error = %v", err)
	}
	if message.(playback.TransferMsg).Paused {
		t.Fatal("restore_paused=resume should start playback")
	}
}

func TestVolumeMessage(t *testing.T) {
	payload, err := proto.Marshal(&connectpb.SetVolumeCommand{Volume: maxStateVolume / 2})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	message, err := volumeMessage(payload)
	if err != nil {
		t.Fatalf("volumeMessage() error = %v", err)
	}
	got := message.(playback.SetVolumeMsg)
	if got.VolumeDB < -6.1 || got.VolumeDB > -5.9 {
		t.Fatalf("volume = %f dB, want approximately -6 dB", got.VolumeDB)
	}
}

func TestServiceDispatchesDealerVolumeMessage(t *testing.T) {
	service := New("cliamp")
	defer service.Close()
	var got tea.Msg
	service.SetSender(func(msg tea.Msg) { got = msg })
	service.mu.Lock()
	service.registered = true
	service.state = spotifyPlaybackState()
	service.mu.Unlock()

	payload, err := proto.Marshal(&connectpb.SetVolumeCommand{Volume: maxStateVolume / 2})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	service.handleDealerMessage(dealer.Message{
		Uri:     "hm://connect-state/v1/connect/volume/command",
		Payload: payload,
	})
	if got == nil {
		t.Fatal("volume message was not dispatched")
	}
	if _, ok := got.(playback.SetVolumeMsg); !ok {
		t.Fatalf("dispatched %T, want playback.SetVolumeMsg", got)
	}
}

func TestDispatchCommandRequiresActiveConnectState(t *testing.T) {
	service := New("cliamp")
	defer service.Close()
	service.SetSender(func(tea.Msg) {})

	var request dealer.RequestPayload
	request.Command.Endpoint = "pause"
	if err := service.dispatchCommand(request); err == nil {
		t.Fatal("inactive Connect state accepted a remote command")
	}

	var got tea.Msg
	service.SetSender(func(msg tea.Msg) { got = msg })
	service.mu.Lock()
	service.registered = true
	service.state = spotifyPlaybackState()
	service.mu.Unlock()
	if err := service.dispatchCommand(request); err != nil {
		t.Fatalf("dispatchCommand() error = %v", err)
	}
	if _, ok := got.(playback.PauseMsg); !ok {
		t.Fatalf("dispatched %T, want playback.PauseMsg", got)
	}
}
