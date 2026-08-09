// Package connect publishes cliamp playback state to Spotify Connect.
package connect

import (
	"math"
	"strconv"
	"strings"
	"time"

	librespot "github.com/devgianlu/go-librespot"
	connectpb "github.com/devgianlu/go-librespot/proto/spotify/connectstate"
	devicespb "github.com/devgianlu/go-librespot/proto/spotify/connectstate/devices"

	"github.com/bjarneo/cliamp/internal/playback"
)

const maxStateVolume = 65535

func isSpotifyState(state playback.State) bool {
	return strings.HasPrefix(state.Track.URL, "spotify:track:") ||
		strings.HasPrefix(state.Track.URL, "spotify:episode:")
}

func newDeviceInfo(name, deviceID string) *connectpb.DeviceInfo {
	return &connectpb.DeviceInfo{
		CanPlay:               true,
		Volume:                maxStateVolume,
		Name:                  name,
		DeviceId:              deviceID,
		DeviceType:            devicespb.DeviceType_COMPUTER,
		DeviceSoftwareVersion: librespot.VersionString(),
		ClientId:              librespot.ClientIdHex,
		SpircVersion:          "3.2.6",
		Capabilities: &connectpb.Capabilities{
			CanBePlayer:                true,
			RestrictToLocal:            false,
			GaiaEqConnectId:            true,
			SupportsLogout:             false,
			IsObservable:               true,
			VolumeSteps:                maxStateVolume,
			SupportedTypes:             []string{"audio/track", "audio/episode"},
			CommandAcks:                true,
			SupportsRename:             false,
			Hidden:                     false,
			DisableVolume:              false,
			ConnectDisabled:            false,
			SupportsPlaylistV2:         true,
			IsControllable:             true,
			SupportsExternalEpisodes:   false,
			SupportsSetBackendMetadata: true,
			SupportsTransferCommand:    true,
			SupportsCommandRequest:     true,
			IsVoiceEnabled:             false,
			NeedsFullPlayerState:       false,
			SupportsGzipPushes:         true,
			SupportsSetOptionsCommand:  true,
			ConnectCapabilities:        "",
		},
	}
}

func newPutStateRequest(device *connectpb.DeviceInfo, state playback.State, reason connectpb.PutStateReason) *connectpb.PutStateRequest {
	now := time.Now()
	position := state.Position.Milliseconds()
	duration := state.Track.Duration.Milliseconds()
	if position < 0 {
		position = 0
	}
	if duration < 0 {
		duration = 0
	}

	playing := state.Status == playback.StatusPlaying
	paused := state.Status == playback.StatusPaused
	device.Volume = dbToStateVolume(state.VolumeDB)

	return &connectpb.PutStateRequest{
		ClientSideTimestamp: uint64(now.UnixMilli()),
		MemberType:          connectpb.MemberType_CONNECT_STATE,
		PutStateReason:      reason,
		IsActive:            playing || paused,
		Device: &connectpb.Device{
			DeviceInfo: device,
			PlayerState: &connectpb.PlayerState{
				Timestamp:             now.UnixMilli(),
				ContextUri:            state.Track.URL,
				Track:                 providedTrack(state.Track),
				PlaybackSpeed:         playbackSpeed(playing),
				PositionAsOfTimestamp: position,
				Duration:              duration,
				IsPlaying:             playing || paused,
				IsPaused:              paused,
				IsBuffering:           false,
				IsSystemInitiated:     true,
				PlayOrigin:            &connectpb.PlayOrigin{FeatureIdentifier: "cliamp"},
				Options:               &connectpb.ContextPlayerOptions{},
				Suppressions:          &connectpb.Suppressions{},
			},
		},
	}
}

func playbackSpeed(playing bool) float64 {
	if playing {
		return 1
	}
	return 0
}

func providedTrack(track playback.Track) *connectpb.ProvidedTrack {
	metadata := map[string]string{
		"title":        track.Title,
		"artist_name":  track.Artist,
		"album_title":  track.Album,
		"duration":     strconv.FormatInt(track.Duration.Milliseconds(), 10),
		"track_number": strconv.Itoa(track.TrackNumber),
	}
	if track.ArtURL != "" {
		metadata["image_url"] = track.ArtURL
	}
	if track.Genre != "" {
		metadata["genre"] = track.Genre
	}

	return &connectpb.ProvidedTrack{
		Uri:      track.URL,
		Metadata: metadata,
		Provider: "context",
	}
}

func dbToStateVolume(db float64) uint32 {
	if math.IsNaN(db) || math.IsInf(db, -1) {
		return 0
	}
	linear := math.Pow(10, db/20)
	if math.IsInf(linear, 0) || linear >= 1 {
		return maxStateVolume
	}
	if linear <= 0 {
		return 0
	}
	return uint32(math.Round(linear * maxStateVolume))
}
