package connect

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/bjarneo/cliamp/applog"
	"github.com/bjarneo/cliamp/internal/playback"
	librespot "github.com/devgianlu/go-librespot"
	"github.com/devgianlu/go-librespot/dealer"
	connectpb "github.com/devgianlu/go-librespot/proto/spotify/connectstate"
	"google.golang.org/protobuf/proto"
)

const playerCommandURI = "hm://connect-state/v1/player/command"

func commandMessage(req dealer.RequestPayload) (any, error) {
	switch req.Command.Endpoint {
	case "play", "resume":
		return playback.PlayMsg{}, nil
	case "pause":
		return playback.PauseMsg{}, nil
	case "skip_next":
		return playback.NextMsg{}, nil
	case "skip_prev":
		return playback.PrevMsg{}, nil
	case "seek_to":
		return seekMessage(req)
	case "set_volume":
		return volumeCommandMessage(req.Command.Value)
	case "transfer":
		if len(req.Command.Data) == 0 {
			return nil, nil
		}
		return transferMessage(req.Command.Data, req.Command.Options.RestorePaused)
	default:
		return nil, fmt.Errorf("unsupported player command %q", req.Command.Endpoint)
	}
}

func seekMessage(req dealer.RequestPayload) (any, error) {
	position, err := commandPosition(req)
	if err != nil {
		return nil, err
	}
	switch req.Command.Relative {
	case "current":
		return playback.SeekMsg{Offset: position}, nil
	case "beginning", "":
		return playback.SetPositionMsg{Position: position}, nil
	default:
		return nil, fmt.Errorf("unsupported seek relative position %q", req.Command.Relative)
	}
}

func commandPosition(req dealer.RequestPayload) (time.Duration, error) {
	if req.Command.Relative != "" || req.Command.Position != 0 {
		return time.Duration(req.Command.Position) * time.Millisecond, nil
	}
	value, ok := req.Command.Value.(float64)
	if !ok {
		return 0, fmt.Errorf("unsupported seek position type %T", req.Command.Value)
	}
	return time.Duration(value) * time.Millisecond, nil
}

func volumeCommandMessage(value any) (any, error) {
	volume, ok := value.(float64)
	if !ok {
		return nil, fmt.Errorf("unsupported volume type %T", value)
	}
	message := playback.SetVolumeMsg{VolumeDB: stateVolumeToDB(volume)}
	applog.Debug("spotify connect: parsed player volume command state_volume=%.0f volume_db=%.2f", volume, message.VolumeDB)
	return message, nil
}

func volumeMessage(payload []byte) (any, error) {
	var command connectpb.SetVolumeCommand
	if err := proto.Unmarshal(payload, &command); err != nil {
		return nil, fmt.Errorf("unmarshal SetVolumeCommand: %w", err)
	}
	message := playback.SetVolumeMsg{VolumeDB: stateVolumeToDB(float64(command.Volume))}
	applog.Debug("spotify connect: parsed volume message state_volume=%d volume_db=%.2f", command.Volume, message.VolumeDB)
	return message, nil
}

func stateVolumeToDB(volume float64) float64 {
	if volume <= 0 || math.IsNaN(volume) {
		return -90
	}
	if volume >= maxStateVolume {
		return 0
	}
	return 20 * math.Log10(volume/maxStateVolume)
}

func transferMessage(data []byte, restorePaused string) (any, error) {
	var state connectpb.TransferState
	if err := proto.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("unmarshal transfer state: %w", err)
	}
	if state.Playback == nil || state.Playback.CurrentTrack == nil {
		return nil, fmt.Errorf("transfer state has no current track")
	}

	contextURI := ""
	if state.CurrentSession != nil && state.CurrentSession.Context != nil {
		contextURI = state.CurrentSession.Context.Uri
	}
	tracks := make([]playback.Track, 0, 1+len(state.Queue.GetTracks()))
	seen := make(map[string]struct{})
	appendTrack := func(track *connectpb.ContextTrack) error {
		converted, err := transferTrack(contextURI, track)
		if err != nil {
			return err
		}
		if _, ok := seen[converted.URL]; ok {
			return nil
		}
		seen[converted.URL] = struct{}{}
		tracks = append(tracks, converted)
		return nil
	}
	if err := appendTrack(state.Playback.CurrentTrack); err != nil {
		return nil, err
	}
	if state.CurrentSession != nil && state.CurrentSession.Context != nil {
		for _, page := range state.CurrentSession.Context.Pages {
			if page == nil {
				applog.Debug("spotify connect: ignoring nil page in transfer state")
				continue
			}
			for _, track := range page.Tracks {
				if err := appendTrack(track); err != nil {
					return nil, err
				}
			}
		}
	}
	for _, track := range state.Queue.GetTracks() {
		if err := appendTrack(track); err != nil {
			return nil, err
		}
	}

	return playback.TransferMsg{
		Tracks:   tracks,
		Position: time.Duration(state.Playback.PositionAsOfTimestamp) * time.Millisecond,
		Paused:   state.Playback.IsPaused && restorePaused != "resume",
	}, nil
}

func transferTrack(contextURI string, track *connectpb.ContextTrack) (playback.Track, error) {
	if track == nil {
		return playback.Track{}, fmt.Errorf("transfer state contains a nil track")
	}
	uri := track.Uri
	if uri == "" {
		if len(track.Gid) != 16 {
			return playback.Track{}, fmt.Errorf("transfer track has no URI")
		}
		uri = librespot.SpotifyIdFromGid(librespot.InferSpotifyIdTypeFromContextUri(contextURI), track.Gid).Uri()
	}
	if !strings.HasPrefix(uri, "spotify:track:") && !strings.HasPrefix(uri, "spotify:episode:") {
		return playback.Track{}, fmt.Errorf("unsupported transfer URI %q", uri)
	}

	metadata := track.Metadata
	durationMS, _ := strconv.ParseInt(metadata["duration"], 10, 64)
	trackNumber, _ := strconv.Atoi(metadata["track_number"])
	return playback.Track{
		URL:         uri,
		Title:       metadata["title"],
		Artist:      metadata["artist_name"],
		Album:       metadata["album_title"],
		Genre:       metadata["genre"],
		TrackNumber: trackNumber,
		ArtURL:      metadata["image_url"],
		Duration:    time.Duration(durationMS) * time.Millisecond,
	}, nil
}
