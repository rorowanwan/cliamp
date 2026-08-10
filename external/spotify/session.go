//go:build !windows

package spotify

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bjarneo/cliamp/applog"
	"github.com/bjarneo/cliamp/internal/browser"
	"github.com/bjarneo/cliamp/playlist"

	librespot "github.com/devgianlu/go-librespot"
	librespotPlayer "github.com/devgianlu/go-librespot/player"
	devicespb "github.com/devgianlu/go-librespot/proto/spotify/connectstate/devices"
	"github.com/devgianlu/go-librespot/session"
	"golang.org/x/oauth2"
	spotifyoauth2 "golang.org/x/oauth2/spotify"
)

// playbackCreds contains only the reusable Access Point credential used by
// librespot playback, dealer, and Spotify Connect. It is issued by the
// matched-client InteractiveCredentials flow.
type playbackCreds struct {
	Username string `json:"username"`
	Data     []byte `json:"data"`
	DeviceID string `json:"device_id"`
}

// webCreds contains only the custom Spotify Developer Dashboard OAuth refresh
// token used for cliamp's Web API calls. It must never replace playbackCreds.
type webCreds struct {
	RefreshToken string `json:"refresh_token"`
}

// storedCreds is the old combined on-disk format. It is read only for
// migration; successful authentication writes the two isolated credential
// files instead.
type storedCreds struct {
	Username     string `json:"username"`
	Data         []byte `json:"data"`
	DeviceID     string `json:"device_id"`
	RefreshToken string `json:"refresh_token,omitempty"`
}

// CallbackPort is the fixed port for the OAuth2 callback server.
// Must match the redirect URI registered in the Spotify Developer app.
const CallbackPort = 19872

// authURLObserver is invoked with the OAuth URL when interactive auth begins.
// Set via SetAuthURLObserver. Used by the TUI to show the URL when the
// launched browser doesn't reach the user (containers, headless envs).
var authURLObserver atomic.Pointer[func(string)]

// SetAuthURLObserver registers a callback invoked once with the OAuth URL at
// the start of an interactive sign-in. Pass nil to remove.
func SetAuthURLObserver(fn func(string)) {
	if fn == nil {
		authURLObserver.Store(nil)
		return
	}
	authURLObserver.Store(&fn)
}

func notifyAuthURL(u string) {
	applog.Info("spotify: sign-in URL: %s", u)
	if p := authURLObserver.Load(); p != nil {
		(*p)(u)
	}
}

// Session manages a go-librespot session and player for Spotify integration.
type Session struct {
	mu          sync.RWMutex
	sess        *session.Session
	player      *librespotPlayer.Player
	devID       string
	clientID    string             // Spotify Developer app client ID
	tokenSource oauth2.TokenSource // auto-refreshing OAuth2 token source
	webRefresh  string             // last persisted Web API refresh token
}

// NewSession creates a playback session from its isolated stored credential,
// falling back to librespot's matched-client interactive login only when
// needed. It then silently refreshes, or interactively obtains, the separate
// custom-client Web API credential.
func NewSession(ctx context.Context, clientID string) (*Session, error) {
	playback, err := loadPlaybackCreds()
	if err == nil && playback.usable() {
		s, err := newSessionFromPlaybackCreds(ctx, clientID, playback)
		if err == nil {
			if err := s.configureWebAPI(ctx, true); err != nil {
				s.Close()
				return nil, err
			}
			return s, nil
		}
		applog.Warn("spotify auth: persisted playback credential failed; bootstrapping a replacement: %v", err)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		applog.Warn("spotify auth: cannot use persisted playback credential: %v", err)
	}
	return newInteractiveSession(ctx, clientID)
}

// NewSessionSilent is like NewSession but only uses stored credentials.
// Returns an error if interactive auth is required.
func NewSessionSilent(ctx context.Context, clientID string) (*Session, error) {
	playback, err := loadPlaybackCreds()
	if err != nil || !playback.usable() {
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			applog.Warn("spotify auth: silent session cannot read persisted playback credential: %v", err)
		} else {
			applog.Debug("spotify auth: silent session has no usable persisted playback credential")
		}
		return nil, fmt.Errorf("no stored credentials")
	}
	s, err := newSessionFromPlaybackCreds(ctx, clientID, playback)
	if err != nil {
		return nil, err
	}
	// Web API failure must not tear down a healthy private playback session.
	if err := s.configureWebAPI(ctx, false); err != nil {
		applog.Warn("spotify auth: silent Web API authorization unavailable; playback remains active: %v", err)
	}
	return s, nil
}

func (c *playbackCreds) usable() bool {
	return c != nil && c.Username != "" && len(c.Data) > 0
}

// newSessionFromPlaybackCreds authenticates AP and login5 exclusively with the
// persisted matched-client credential. It does not read or write Web API OAuth
// state.
func newSessionFromPlaybackCreds(ctx context.Context, clientID string, creds *playbackCreds) (*Session, error) {
	devID := creds.DeviceID
	if devID == "" {
		devID = generateDeviceID()
	}

	applog.Debug("spotify auth: authenticating AP and login5 from persisted playback credential")
	sess, err := session.NewSessionFromOptions(ctx, &session.Options{
		Log:        newCredentialLifecycleLogger(),
		DeviceType: devicespb.DeviceType_COMPUTER,
		DeviceId:   devID,
		Credentials: session.StoredCredentials{
			Username: creds.Username,
			Data:     creds.Data,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("spotify: stored playback auth: %w", err)
	}
	applog.Debug("spotify auth: persisted AP/login5 authentication succeeded")
	s := &Session{sess: sess, devID: devID, clientID: clientID}
	if err := savePlaybackCreds(&playbackCreds{Username: sess.Username(), Data: sess.StoredCredentials(), DeviceID: devID}); err != nil {
		applog.Warn("spotify auth: failed migrating persisted playback credential: %v", err)
	}

	if err := s.initPlayer(); err != nil {
		sess.Close()
		return nil, err
	}
	return s, nil
}

// oauthScopes are the Spotify Web API scopes needed for cliamp.
// See: https://developer.spotify.com/documentation/web-api/concepts/scopes
var oauthScopes = []string{
	// Playlist browsing
	"playlist-read-collaborative",
	"playlist-read-private",
	// Playlist modification (save queue, create playlists)
	"playlist-modify-public",
	"playlist-modify-private",
	// Playback and remote control. app-remote-control matches go-librespot's
	// InteractiveCredentials flow and is needed for the Spotify app's remote
	// playback authorization.
	"app-remote-control",
	"streaming",
	// Library (liked songs, saved albums)
	"user-library-read",
	"user-library-modify",
	// User profile
	"user-read-private",
	// Playback state (current track, queue)
	"user-read-playback-state",
	"user-modify-playback-state",
	"user-read-currently-playing",
	// Recently played / top tracks
	"user-read-recently-played",
	"user-top-read",
	// Following (artists, users)
	"user-follow-read",
	"user-follow-modify",
}

// spotifyOAuthConfig returns the OAuth2 config for the given client ID.
func spotifyOAuthConfig(clientID string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:    clientID,
		RedirectURL: fmt.Sprintf("http://127.0.0.1:%d/login", CallbackPort),
		Scopes:      oauthScopes,
		Endpoint:    spotifyoauth2.Endpoint,
	}
}

// silentTokenRefresh uses a stored refresh token to get a new access token
// without opening a browser.
func silentTokenRefresh(clientID, refreshToken string) (*oauth2.Token, error) {
	conf := spotifyOAuthConfig(clientID)
	src := conf.TokenSource(context.Background(), &oauth2.Token{RefreshToken: refreshToken})
	return src.Token()
}

// isInvalidGrant reports whether err is an OAuth2 invalid_grant response
// from the token endpoint, indicating the refresh token is dead and
// retrying with the same token will not succeed.
func isInvalidGrant(err error) bool {
	var rerr *oauth2.RetrieveError
	if !errors.As(err, &rerr) {
		return false
	}
	return rerr.ErrorCode == "invalid_grant"
}

// oauthCallbackHTML is the response sent to the browser after a successful OAuth2 callback.
const oauthCallbackHTML = `<!DOCTYPE html>
<html><head><meta charset="utf-8"><title>cliamp</title></head>
<body style="font-family:system-ui;display:flex;justify-content:center;align-items:center;height:100vh;margin:0;background:#1a1a2e;color:#e0e0e0">
<div style="text-align:center">
<h2>✅ Authenticated!</h2>
<p>You can close this tab now.</p>
<script>setTimeout(function(){window.close()},1500)</script>
</div></body></html>`

// performOAuth2PKCE runs an OAuth2 PKCE flow: opens a browser for user consent,
// waits for the callback, and exchanges the code for a token.
func performOAuth2PKCE(ctx context.Context, clientID string) (*oauth2.Token, error) {
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", CallbackPort))
	if err != nil {
		return nil, fmt.Errorf("listen on port %d: %w", CallbackPort, err)
	}
	defer lis.Close() // always release the port

	oauthConf := spotifyOAuthConfig(clientID)

	verifier := oauth2.GenerateVerifier()
	authURL := oauthConf.AuthCodeURL("", oauth2.S256ChallengeOption(verifier))

	notifyAuthURL(authURL)
	applog.Debug("spotify auth: PKCE authorization started; awaiting loopback callback")

	codeCh := make(chan string, 1)
	go func() {
		if err := http.Serve(lis, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			code := r.URL.Query().Get("code")
			if code != "" {
				codeCh <- code
			}
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(oauthCallbackHTML))
		})); err != nil && !errors.Is(err, net.ErrClosed) {
			applog.UserError("spotify: auth callback server error: %v", err)
		}
	}()

	_ = browser.Open(authURL) // best-effort — user can open the URL manually if this fails

	var code string
	select {
	case code = <-codeCh:
	case <-ctx.Done():
		return nil, fmt.Errorf("authentication cancelled: %w", ctx.Err())
	}

	token, httpStatus, err := exchangeOAuthCode(ctx, oauthConf, code, verifier)
	if err != nil {
		applog.Warn("spotify auth: PKCE code exchange failed (http_status=%d): %v", httpStatus, err)
		return nil, fmt.Errorf("token exchange: %w", err)
	}
	logOAuthToken("PKCE code exchange", token, httpStatus)

	return token, nil
}

// doWebAPIAuth performs an OAuth2 PKCE flow to get a fresh Web API access token.
// Opens a browser for user consent, returns the full token (including refresh token).
func doWebAPIAuth(ctx context.Context, clientID string) (*oauth2.Token, error) {
	token, err := performOAuth2PKCE(ctx, clientID)
	if err != nil {
		return nil, err
	}
	fmt.Println("Spotify: Web API token refreshed.")
	return token, nil
}

// newInteractiveSession performs the two one-time authorizations required on
// a clean install. The matched-client playback bootstrap runs first; after it
// succeeds its reusable AP credential is saved independently before the custom
// Developer Dashboard Web API PKCE flow begins.
func newInteractiveSession(ctx context.Context, clientID string) (*Session, error) {
	s, err := newInteractivePlaybackSession(ctx, clientID)
	if err != nil {
		return nil, err
	}
	if err := s.configureWebAPI(ctx, true); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// newInteractivePlaybackSession uses go-librespot's own InteractiveCredentials
// flow. Its OAuth client identity therefore matches the identity presented to
// login5, unlike cliamp's custom Web API client ID.
func newInteractivePlaybackSession(ctx context.Context, clientID string) (*Session, error) {
	devID := generateDeviceID()
	applog.Info("spotify auth: playback bootstrap requires librespot authorization")
	sess, err := session.NewSessionFromOptions(ctx, &session.Options{
		Log:        newCredentialLifecycleLogger(),
		DeviceType: devicespb.DeviceType_COMPUTER,
		DeviceId:   devID,
		Credentials: session.InteractiveCredentials{
			CallbackPort: CallbackPort,
		},
	})
	if err != nil {
		applog.Warn("spotify auth: matched-client playback bootstrap failed: %v", err)
		return nil, fmt.Errorf("spotify: interactive playback session: %w", err)
	}

	if err := savePlaybackCreds(&playbackCreds{
		Username: sess.Username(),
		Data:     sess.StoredCredentials(),
		DeviceID: devID,
	}); err != nil {
		sess.Close()
		return nil, fmt.Errorf("spotify: persist playback credential: %w", err)
	}

	s := &Session{sess: sess, devID: devID, clientID: clientID}
	if err := s.initPlayer(); err != nil {
		sess.Close()
		return nil, err
	}
	applog.Info("spotify auth: matched-client playback bootstrap succeeded")
	return s, nil
}

// configureWebAPI restores custom-client Web API access without changing the
// already established playback session. When interactive is false, missing or
// invalid Web API credentials leave playback usable and tokenSource nil.
func (s *Session) configureWebAPI(ctx context.Context, interactive bool) error {
	web, err := loadWebCreds()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		if !interactive {
			return fmt.Errorf("load Web API credential: %w", err)
		}
		// A damaged Web API store must not invalidate a separately healthy
		// playback session. Interactive startup can replace only this lane.
		applog.Warn("spotify auth: cannot read custom Web API credential; authorization required: %v", err)
		web = nil
	}
	if web != nil && web.RefreshToken != "" {
		applog.Debug("spotify auth: custom Web API refresh-token renewal started")
		token, err := silentTokenRefresh(s.clientID, web.RefreshToken)
		if err == nil {
			if token.RefreshToken == "" {
				token.RefreshToken = web.RefreshToken
			}
			if err := s.setWebToken(token); err != nil {
				return err
			}
			applog.Debug("spotify auth: custom Web API refresh-token renewal succeeded")
			return nil
		}
		if !interactive {
			return fmt.Errorf("refresh custom Web API token: %w", err)
		}
		applog.Warn("spotify auth: custom Web API refresh-token renewal failed; authorization required: %v", err)
	}
	if !interactive {
		return fmt.Errorf("no custom Web API credential: %w", playlist.ErrNeedsAuth)
	}

	applog.Info("spotify auth: Web API authorization requires your configured Spotify client")
	token, err := doWebAPIAuth(ctx, s.clientID)
	if err != nil {
		return fmt.Errorf("spotify: Web API authorization: %w", err)
	}
	return s.setWebToken(token)
}

func (s *Session) setWebToken(token *oauth2.Token) error {
	if token == nil || token.AccessToken == "" || token.RefreshToken == "" {
		return fmt.Errorf("spotify: Web API authorization returned an incomplete token")
	}
	if err := saveWebCreds(&webCreds{RefreshToken: token.RefreshToken}); err != nil {
		return fmt.Errorf("persist Web API credential: %w", err)
	}
	conf := spotifyOAuthConfig(s.clientID)
	s.mu.Lock()
	s.tokenSource = conf.TokenSource(context.Background(), token)
	s.webRefresh = token.RefreshToken
	s.mu.Unlock()
	return nil
}

// exchangeStatusTransport records only the HTTP status code. It deliberately
// does not retain request URLs, headers, or bodies, all of which can contain
// OAuth secrets during a token exchange.
type exchangeStatusTransport struct {
	base   http.RoundTripper
	status atomic.Int32
}

func (t *exchangeStatusTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if resp != nil {
		t.status.Store(int32(resp.StatusCode))
	}
	return resp, err
}

// exchangeOAuthCode performs a PKCE code exchange while observing its HTTP
// status for diagnostics. oauth2.Config.Exchange otherwise intentionally only
// exposes the decoded token or an error.
func exchangeOAuthCode(ctx context.Context, conf *oauth2.Config, code, verifier string) (*oauth2.Token, int, error) {
	baseClient := http.DefaultClient
	if client, ok := ctx.Value(oauth2.HTTPClient).(*http.Client); ok && client != nil {
		baseClient = client
	}
	client := *baseClient
	base := client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	transport := &exchangeStatusTransport{base: base}
	client.Transport = transport
	exchangeCtx := context.WithValue(ctx, oauth2.HTTPClient, &client)
	token, err := conf.Exchange(exchangeCtx, code, oauth2.VerifierOption(verifier))
	return token, int(transport.status.Load()), err
}

// logOAuthToken records authentication-relevant metadata without exposing any
// credential material. OAuth2 treats a non-nil token returned without error as
// a successful token-endpoint response.
func logOAuthToken(stage string, token *oauth2.Token, httpStatus int) {
	if token == nil {
		applog.Warn("spotify auth: %s returned no token (http_status=%d)", stage, httpStatus)
		return
	}
	expiresIn := int64(-1)
	if !token.Expiry.IsZero() {
		expiresIn = int64(time.Until(token.Expiry).Round(time.Second).Seconds())
	}
	scopes, _ := token.Extra("scope").(string)
	applog.Debug("spotify auth: %s succeeded (http_status=%d token_type=%q access_token_present=%t refresh_token_present=%t expires_in_seconds=%d valid=%t scopes=%q)", stage, httpStatus, token.TokenType, token.AccessToken != "", token.RefreshToken != "", expiresIn, token.Valid(), scopes)
}

// initPlayer creates the go-librespot player. We only use NewStream() for
// decoded AudioSources — audio output is routed through cliamp's Beep pipeline,
// not go-librespot's output backend.
func (s *Session) initPlayer() error {
	// go-librespot uses this for media restriction checks but Premium
	// accounts can play all tracks regardless.
	countryCode := "US"
	p, err := librespotPlayer.NewPlayer(&librespotPlayer.Options{
		Spclient:             s.sess.Spclient(),
		AudioKey:             s.sess.AudioKey(),
		Events:               s.sess.Events(),
		Log:                  &librespot.NullLogger{},
		CountryCode:          &countryCode,
		NormalisationEnabled: true,
		AudioBackend:         "pipe",
		AudioOutputPipe:      os.DevNull,
	})
	if err != nil {
		return fmt.Errorf("spotify: player init: %w", err)
	}
	s.player = p
	return nil
}

// NewStream creates a decoded audio stream for the given Spotify track ID.
//
// Holds s.mu.RLock() across the librespot network call. Multiple concurrent
// NewStream / webApi callers can run in parallel (RLock is shared), so rapid
// track skipping does not serialize. reconnect() and Close() take the full
// Lock and will wait for in-flight callers to finish before tearing down the
// player — without this, the swap could call oldPlayer.Close() while we are
// still reading from it.
func (s *Session) NewStream(ctx context.Context, spotID librespot.SpotifyId, bitrate int) (*librespotPlayer.Stream, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.player == nil {
		return nil, fmt.Errorf("spotify: session closed")
	}
	return s.player.NewStream(ctx, http.DefaultClient, spotID, bitrate, 0)
}

// ConnectEndpoint returns the live librespot session and stable device ID used
// by the Spotify Connect publisher. Callers must treat the returned session as
// a snapshot: Reconnect can replace it, after which the provider rebinds the
// publisher to the new session.
func (s *Session) ConnectEndpoint() (*session.Session, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sess, s.devID
}

// webApiWithBody calls the Spotify Web API using the OAuth2 access token.
//
// The spclient/login5 token from librespot is NOT accepted by the Web API
// for endpoints like /v1/search and /v1/me/playlists — Spotify returns
// misleading errors ("Invalid limit", 429) instead of a clear auth failure.
// So if there is no OAuth2 token source, fail loudly with ErrNeedsAuth
// rather than attempting the call with the wrong token.
func (s *Session) webApiWithBody(ctx context.Context, method, path string, query url.Values, body io.Reader, contentType string) (*http.Response, error) {
	s.mu.RLock()
	ts := s.tokenSource
	s.mu.RUnlock()

	if ts == nil {
		return nil, fmt.Errorf("spotify: web api token unavailable, run 'cliamp spotify reset' and sign in again: %w", playlist.ErrNeedsAuth)
	}
	tok, err := ts.Token()
	if err != nil {
		return nil, fmt.Errorf("refresh access token: %w", err)
	}
	s.persistWebRefreshToken(tok)
	token := tok.AccessToken

	u, _ := url.Parse("https://api.spotify.com")
	u = u.JoinPath(path)
	if query != nil {
		u.RawQuery = query.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	return http.DefaultClient.Do(req)
}

// persistWebRefreshToken records a rotated custom OAuth refresh token without
// touching the independent playback credential store.
func (s *Session) persistWebRefreshToken(token *oauth2.Token) {
	if token == nil || token.RefreshToken == "" {
		return
	}
	s.mu.Lock()
	if s.webRefresh == token.RefreshToken {
		s.mu.Unlock()
		return
	}
	s.webRefresh = token.RefreshToken
	s.mu.Unlock()
	if err := saveWebCreds(&webCreds{RefreshToken: token.RefreshToken}); err != nil {
		applog.Warn("spotify auth: failed persisting rotated Web API refresh token: %v", err)
	}
}

// Close releases all session and player resources.
func (s *Session) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.player != nil {
		s.player.Close()
	}
	if s.sess != nil {
		s.sess.Close()
	}
}

// Reconnect rebuilds the private playback session from its stored AP
// credential (no browser). A missing or expired Web API credential is logged
// but does not interrupt playback.
func (s *Session) Reconnect(ctx context.Context) error {
	applog.Debug("spotify auth: starting silent session reconnect")
	return s.reconnect(ctx, NewSessionSilent)
}

// ReconnectInteractive starts the browser authorization required by whichever
// credential lane needs replacement. Existing credential files are retained
// until their individual replacement succeeds.
func (s *Session) ReconnectInteractive(ctx context.Context) error {
	applog.Debug("spotify auth: starting interactive session reconnect")
	return s.reconnect(ctx, newInteractiveSession)
}

// reconnect replaces the live session using the provided builder function.
// The new session is established before tearing down the old one to avoid a
// window where s.sess/s.player are nil (which would crash concurrent callers).
//
// The swap-and-teardown phase is done under s.mu (full Lock), which waits for
// any in-flight NewStream / webApi RLockers to drain. This guarantees that
// oldPlayer.Close() is never called while a NewStream is still using the
// old player pointer.
func (s *Session) reconnect(ctx context.Context, build func(context.Context, string) (*Session, error)) error {
	s.mu.RLock()
	clientID := s.clientID
	s.mu.RUnlock()

	newSess, err := build(ctx, clientID)
	if err != nil {
		applog.Warn("spotify auth: session reconnect build failed: %v", err)
		return fmt.Errorf("spotify: reconnect: %w", err)
	}

	// Swap and tear down the old session under a single write lock so
	// in-flight NewStream / webApi calls finish before oldPlayer.Close()
	// runs. The expensive build() above happened lock-free.
	s.mu.Lock()
	oldPlayer := s.player
	oldSess := s.sess
	s.sess = newSess.sess
	s.player = newSess.player
	s.devID = newSess.devID
	s.tokenSource = newSess.tokenSource
	s.webRefresh = newSess.webRefresh
	if oldPlayer != nil {
		oldPlayer.Close()
	}
	if oldSess != nil {
		oldSess.Close()
	}
	s.mu.Unlock()

	// Prevent newSess.Close() from tearing down the resources we just adopted.
	newSess.mu.Lock()
	newSess.sess = nil
	newSess.player = nil
	newSess.mu.Unlock()

	const reauthMsg = "spotify: re-authenticated successfully"
	applog.Info(reauthMsg)
	applog.Debug("spotify auth: session reconnect completed and replaced the previous session")
	applog.Status(reauthMsg)
	return nil
}

func generateDeviceID() string {
	b := make([]byte, 20)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func loadLegacyCreds() (*storedCreds, error) {
	path, err := CredsPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			applog.Debug("spotify auth: persisted credential file not found")
		} else {
			applog.Warn("spotify auth: failed reading persisted credential file: %v", err)
		}
		return nil, err
	}
	var creds storedCreds
	if err := json.Unmarshal(data, &creds); err != nil {
		applog.Warn("spotify auth: persisted credential file is malformed: %v", err)
		return nil, err
	}
	applog.Debug("spotify auth: loaded legacy combined credential file (playback_present=%t web_refresh_present=%t)", creds.Username != "" && len(creds.Data) > 0, creds.RefreshToken != "")
	return &creds, nil
}

func loadPlaybackCreds() (*playbackCreds, error) {
	path, err := PlaybackCredsPath()
	if err != nil {
		return nil, err
	}
	var creds playbackCreds
	if err := readCredentialFile(path, &creds); err == nil {
		applog.Debug("spotify auth: loaded isolated playback credential (username_present=%t stored_blob_present=%t device_id_present=%t)", creds.Username != "", len(creds.Data) > 0, creds.DeviceID != "")
		return &creds, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	legacy, err := loadLegacyCreds()
	if err != nil {
		return nil, err
	}
	if legacy.Username == "" || len(legacy.Data) == 0 {
		return nil, os.ErrNotExist
	}
	applog.Info("spotify auth: using legacy playback credential; it will migrate after a successful login")
	return &playbackCreds{Username: legacy.Username, Data: legacy.Data, DeviceID: legacy.DeviceID}, nil
}

func loadWebCreds() (*webCreds, error) {
	path, err := WebCredsPath()
	if err != nil {
		return nil, err
	}
	var creds webCreds
	if err := readCredentialFile(path, &creds); err == nil {
		applog.Debug("spotify auth: loaded isolated Web API credential (refresh_token_present=%t)", creds.RefreshToken != "")
		return &creds, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	legacy, err := loadLegacyCreds()
	if err != nil {
		return nil, err
	}
	if legacy.RefreshToken == "" {
		return nil, os.ErrNotExist
	}
	applog.Info("spotify auth: using legacy Web API refresh token; it will migrate after a successful refresh")
	return &webCreds{RefreshToken: legacy.RefreshToken}, nil
}

func savePlaybackCreds(creds *playbackCreds) error {
	if creds == nil || !creds.usable() {
		return errors.New("incomplete playback credentials")
	}
	path, err := PlaybackCredsPath()
	if err != nil {
		return err
	}
	if err := writeCredentialFile(path, creds); err != nil {
		return err
	}
	applog.Info("spotify auth: persisted isolated playback credential (device_id_present=%t)", creds.DeviceID != "")
	return nil
}

func saveWebCreds(creds *webCreds) error {
	if creds == nil || creds.RefreshToken == "" {
		return errors.New("incomplete Web API credentials")
	}
	path, err := WebCredsPath()
	if err != nil {
		return err
	}
	if err := writeCredentialFile(path, creds); err != nil {
		return err
	}
	applog.Info("spotify auth: persisted isolated Web API refresh credential")
	return nil
}

func readCredentialFile(path string, target any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			applog.Warn("spotify auth: failed reading credential file: %v", err)
		}
		return err
	}
	if err := json.Unmarshal(data, target); err != nil {
		applog.Warn("spotify auth: credential file is malformed: %v", err)
		return err
	}
	return nil
}

func writeCredentialFile(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		applog.Warn("spotify auth: failed persisting credential file: %v", err)
		return err
	}
	return nil
}
