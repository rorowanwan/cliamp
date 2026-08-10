package spotify

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/bjarneo/cliamp/applog"
	"github.com/bjarneo/cliamp/internal/appdir"
)

// DefaultClientID is the librespot keymaster client_id, shared by spotify-player
// and other librespot-based players. Used when the user hasn't configured their
// own client_id — Spotify's loopback exception lets it work with any 127.0.0.1
// port, and it predates the Nov 27, 2024 dev-mode quota restriction so /v1/search
// and other catalog endpoints stay accessible.
const DefaultClientID = "65b708073fc0480ea92a077233ca87bd"

// CredsPath returns the absolute path to the stored Spotify credentials file.
func CredsPath() (string, error) {
	dir, err := appdir.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "spotify_credentials.json"), nil
}

// PlaybackCredsPath returns the isolated librespot Access Point credential
// store. These credentials are never used for the Spotify Web API.
func PlaybackCredsPath() (string, error) {
	dir, err := appdir.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "spotify_playback_credentials.json"), nil
}

// WebCredsPath returns the isolated custom-client OAuth refresh-token store.
// It is intentionally separate from librespot's playback credential.
func WebCredsPath() (string, error) {
	dir, err := appdir.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "spotify_web_credentials.json"), nil
}

// DeleteCreds removes the legacy combined credential file and both isolated
// credential stores. Returns true if at least one file was removed.
func DeleteCreds() (bool, error) {
	legacy, err := CredsPath()
	if err != nil {
		return false, err
	}
	playback, err := PlaybackCredsPath()
	if err != nil {
		return false, err
	}
	web, err := WebCredsPath()
	if err != nil {
		return false, err
	}

	removed := false
	for _, path := range []string{legacy, playback, web} {
		if err := os.Remove(path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			applog.Warn("spotify auth: failed removing credential file: %v", err)
			return removed, err
		}
		removed = true
	}
	if removed {
		applog.Info("spotify auth: removed persisted playback and Web API credentials")
	} else {
		applog.Debug("spotify auth: reset found no persisted credential files")
	}
	return removed, nil
}
