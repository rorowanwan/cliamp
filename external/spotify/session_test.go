//go:build !windows

package spotify

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestOAuthScopesIncludeAppRemoteControl(t *testing.T) {
	if !slices.Contains(oauthScopes, "app-remote-control") {
		t.Fatal("oauthScopes does not include app-remote-control")
	}
}

func TestExchangeStatusTransportRecordsOnlyStatus(t *testing.T) {
	transport := &exchangeStatusTransport{base: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("token-body"))}, nil
	})}
	response, err := transport.RoundTrip(&http.Request{})
	if err != nil {
		t.Fatalf("RoundTrip() error = %v", err)
	}
	defer response.Body.Close()
	if got := transport.status.Load(); got != http.StatusOK {
		t.Fatalf("recorded status = %d, want %d", got, http.StatusOK)
	}
}

func TestLogOAuthTokenMetadata(t *testing.T) {
	// This is intentionally a smoke test: the logger accepts a normal OAuth
	// token without formatting any credential fields into an error or panicking.
	logOAuthToken("test", &oauth2.Token{
		AccessToken:  "access-token",
		RefreshToken: "refresh-token",
		TokenType:    "Bearer",
		Expiry:       time.Now().Add(time.Hour),
	}, http.StatusOK)
	logOAuthToken("test", nil, 0)
}

func TestSeparatedCredentialRoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	wantPlayback := &playbackCreds{
		Username: "test-user",
		Data:     []byte{1, 2, 3},
		DeviceID: "0123456789abcdef0123456789abcdef01234567",
	}
	if err := savePlaybackCreds(wantPlayback); err != nil {
		t.Fatalf("savePlaybackCreds() error = %v", err)
	}
	wantWeb := &webCreds{RefreshToken: "test-refresh-token"}
	if err := saveWebCreds(wantWeb); err != nil {
		t.Fatalf("saveWebCreds() error = %v", err)
	}

	gotPlayback, err := loadPlaybackCreds()
	if err != nil {
		t.Fatalf("loadPlaybackCreds() error = %v", err)
	}
	if gotPlayback.Username != wantPlayback.Username || gotPlayback.DeviceID != wantPlayback.DeviceID || !bytes.Equal(gotPlayback.Data, wantPlayback.Data) {
		t.Fatalf("loadPlaybackCreds() = %#v, want %#v", gotPlayback, wantPlayback)
	}
	gotWeb, err := loadWebCreds()
	if err != nil {
		t.Fatalf("loadWebCreds() error = %v", err)
	}
	if gotWeb.RefreshToken != wantWeb.RefreshToken {
		t.Fatalf("loadWebCreds().RefreshToken = %q, want %q", gotWeb.RefreshToken, wantWeb.RefreshToken)
	}

	for _, pathFn := range []func() (string, error){PlaybackCredsPath, WebCredsPath} {
		path, err := pathFn()
		if err != nil {
			t.Fatalf("credential path error = %v", err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat credential file: %v", err)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Fatalf("credential file mode = %o, want 600", mode)
		}
	}
}

func TestWebCredentialDoesNotMutatePlaybackCredential(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	playback := &playbackCreds{Username: "test-user", Data: []byte{1, 2, 3}, DeviceID: "device"}
	if err := savePlaybackCreds(playback); err != nil {
		t.Fatal(err)
	}
	path, err := PlaybackCredsPath()
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := saveWebCreds(&webCreds{RefreshToken: "first"}); err != nil {
		t.Fatal(err)
	}
	if err := saveWebCreds(&webCreds{RefreshToken: "rotated"}); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("Web API credential update mutated playback credential file")
	}
}

func TestPlaybackCredentialReplacementDoesNotMutateWebCredential(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if err := saveWebCreds(&webCreds{RefreshToken: "web-refresh"}); err != nil {
		t.Fatal(err)
	}
	webPath, err := WebCredsPath()
	if err != nil {
		t.Fatal(err)
	}
	webBefore, err := os.ReadFile(webPath)
	if err != nil {
		t.Fatal(err)
	}

	if err := savePlaybackCreds(&playbackCreds{Username: "first", Data: []byte{1}, DeviceID: "one"}); err != nil {
		t.Fatal(err)
	}
	want := &playbackCreds{Username: "replacement", Data: []byte{2, 3}, DeviceID: "two"}
	if err := savePlaybackCreds(want); err != nil {
		t.Fatal(err)
	}
	got, err := loadPlaybackCreds()
	if err != nil {
		t.Fatal(err)
	}
	if got.Username != want.Username || got.DeviceID != want.DeviceID || !bytes.Equal(got.Data, want.Data) {
		t.Fatalf("loadPlaybackCreds() = %#v, want %#v", got, want)
	}
	webAfter, err := os.ReadFile(webPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(webBefore, webAfter) {
		t.Fatal("playback credential replacement mutated Web API credential file")
	}
}

func TestPersistedPlaybackCredentialIsUsableWithoutWebCredential(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	want := &playbackCreds{Username: "test-user", Data: []byte{1, 2, 3}, DeviceID: "device"}
	if err := savePlaybackCreds(want); err != nil {
		t.Fatal(err)
	}
	got, err := loadPlaybackCreds()
	if err != nil {
		t.Fatalf("loadPlaybackCreds() error = %v", err)
	}
	if !got.usable() {
		t.Fatal("persisted playback credential is not usable for a silent playback session")
	}
	if _, err := loadWebCreds(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("loadWebCreds() error = %v, want not exist", err)
	}
}

func TestMalformedWebCredentialDoesNotCorruptPlaybackCredential(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	want := &playbackCreds{Username: "test-user", Data: []byte{1, 2, 3}, DeviceID: "device"}
	if err := savePlaybackCreds(want); err != nil {
		t.Fatal(err)
	}
	webPath, err := WebCredsPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(webPath, []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadWebCreds(); err == nil {
		t.Fatal("loadWebCreds() succeeded for malformed Web API credential")
	}
	got, err := loadPlaybackCreds()
	if err != nil {
		t.Fatalf("loadPlaybackCreds() error after Web API failure = %v", err)
	}
	if got.Username != want.Username || got.DeviceID != want.DeviceID || !bytes.Equal(got.Data, want.Data) {
		t.Fatalf("playback credential changed after Web API failure: %#v", got)
	}
}

func TestLegacyCredentialFallbackDoesNotOverwriteLegacyFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	legacy := &storedCreds{Username: "test-user", Data: []byte{1, 2, 3}, DeviceID: "device", RefreshToken: "refresh"}
	path, err := CredsPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := writeCredentialFile(path, legacy); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	playback, err := loadPlaybackCreds()
	if err != nil || !playback.usable() {
		t.Fatalf("loadPlaybackCreds() = %#v, %v", playback, err)
	}
	web, err := loadWebCreds()
	if err != nil || web.RefreshToken != legacy.RefreshToken {
		t.Fatalf("loadWebCreds() = %#v, %v", web, err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("legacy credential file was unexpectedly modified")
	}
}

func TestIsInvalidGrant(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain error", errors.New("network blip"), false},
		{"oauth invalid_grant", &oauth2.RetrieveError{ErrorCode: "invalid_grant"}, true},
		{"oauth invalid_request", &oauth2.RetrieveError{ErrorCode: "invalid_request"}, false},
		{"wrapped invalid_grant", fmt.Errorf("refresh failed: %w", &oauth2.RetrieveError{ErrorCode: "invalid_grant"}), true},
		{"wrapped non-oauth", fmt.Errorf("refresh failed: %w", errors.New("transport error")), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isInvalidGrant(tt.err)
			if got != tt.want {
				t.Errorf("isInvalidGrant(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestDeleteCreds(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	t.Run("missing file", func(t *testing.T) {
		removed, err := DeleteCreds()
		if err != nil {
			t.Errorf("DeleteCreds() on missing file returned %v, want nil", err)
		}
		if removed {
			t.Error("DeleteCreds() reported removed=true for missing file")
		}
	})

	t.Run("removes every credential store", func(t *testing.T) {
		dir := filepath.Join(home, ".config", "cliamp")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		paths := []string{
			filepath.Join(dir, "spotify_credentials.json"),
			filepath.Join(dir, "spotify_playback_credentials.json"),
			filepath.Join(dir, "spotify_web_credentials.json"),
		}
		for _, path := range paths {
			if err := os.WriteFile(path, []byte(`{"username":"x"}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}

		removed, err := DeleteCreds()
		if err != nil {
			t.Fatalf("DeleteCreds() = %v, want nil", err)
		}
		if !removed {
			t.Error("DeleteCreds() reported removed=false after removing file")
		}
		for _, path := range paths {
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Errorf("credential file still exists after DeleteCreds: %q stat err = %v", path, err)
			}
		}
	})
}

func TestCredsPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	got, err := CredsPath()
	if err != nil {
		t.Fatalf("CredsPath() error = %v", err)
	}
	want := filepath.Join(home, ".config", "cliamp", "spotify_credentials.json")
	if got != want {
		t.Errorf("CredsPath() = %q, want %q", got, want)
	}
}

func TestIsolatedCredentialPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".config", "cliamp")

	tests := []struct {
		name string
		path func() (string, error)
		file string
	}{
		{"playback", PlaybackCredsPath, "spotify_playback_credentials.json"},
		{"web", WebCredsPath, "spotify_web_credentials.json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.path()
			if err != nil {
				t.Fatal(err)
			}
			if want := filepath.Join(dir, tt.file); got != want {
				t.Errorf("credential path = %q, want %q", got, want)
			}
		})
	}
}
