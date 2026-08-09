package connect

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/bjarneo/cliamp/applog"
	"github.com/bjarneo/cliamp/internal/playback"
	"github.com/devgianlu/go-librespot/dealer"
	connectpb "github.com/devgianlu/go-librespot/proto/spotify/connectstate"
	"github.com/devgianlu/go-librespot/session"
)

const (
	minPutInterval        = 200 * time.Millisecond
	initialRetryBackoff   = 5 * time.Second
	maximumRetryBackoff   = time.Minute
	publishRequestTimeout = 10 * time.Second
)

type stateClient interface {
	PutConnectState(context.Context, string, *connectpb.PutStateRequest) error
	PutConnectStateInactive(context.Context, string, bool) error
}

type dealerClient interface {
	Connect(context.Context) error
	ReceiveMessage(...string) <-chan dealer.Message
	ReceiveRequest(string) <-chan dealer.Request
}

type endpoint struct {
	key      any
	deviceID string
	spclient stateClient
	dealer   dealerClient
}

// Service translates cliamp playback updates into Spotify Connect state puts
// and dealer commands into Bubbletea playback messages.
type Service struct {
	name string

	mu       sync.Mutex
	endpoint endpoint
	version  uint64
	state    playback.State
	hasState bool

	connectionID string
	registered   bool
	lastPut      time.Time
	stateVersion uint64
	dirty        bool
	retryAt      time.Time
	retryBackoff time.Duration
	rateLimited  bool
	coalesced    uint
	lastErr      time.Time
	receiver     <-chan dealer.Message
	requests     <-chan dealer.Request
	send         func(tea.Msg)

	wake chan struct{}
	done chan struct{}
	wg   sync.WaitGroup
}

// New starts an idle service. Bind must be called once a librespot session is
// available; this is delayed because cliamp authenticates Spotify lazily.
func New(name string) *Service {
	if name == "" {
		name = "cliamp"
	}
	s := &Service{
		name: name,
		wake: make(chan struct{}, 1),
		done: make(chan struct{}),
	}
	s.wg.Add(1)
	go s.run()
	return s
}

// Bind replaces the session used for Connect publishing. It is safe to call
// after cliamp's silent Spotify reconnect has swapped the underlying session.
func (s *Service) Bind(sess *session.Session, deviceID string) {
	if s == nil || sess == nil || deviceID == "" {
		return
	}
	s.bind(endpoint{
		key:      sess,
		deviceID: deviceID,
		spclient: newSpclientStateClient(sess.Spclient(), deviceID),
		dealer:   sess.Dealer(),
	})
}

// SetSender sets the Bubbletea message sink used for remote Spotify Connect
// commands. It may be called before or after Bind.
func (s *Service) SetSender(send func(tea.Msg)) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.send = send
	s.mu.Unlock()
}

func (s *Service) bind(next endpoint) {
	s.mu.Lock()
	if s.endpoint.key == next.key {
		s.mu.Unlock()
		return
	}
	s.endpoint = next
	s.version++
	s.connectionID = ""
	s.registered = false
	s.lastPut = time.Time{}
	s.stateVersion++
	s.dirty = s.hasState
	s.retryAt = time.Time{}
	s.retryBackoff = 0
	s.rateLimited = false
	s.coalesced = 0
	s.receiver = nil
	s.requests = nil
	s.mu.Unlock()
	s.signal()
}

func (s *Service) Update(state playback.State) {
	if s == nil {
		return
	}
	s.mu.Lock()
	changed := !s.hasState || stateNeedsPublish(s.state, state)
	s.state = state
	s.hasState = true
	if changed {
		s.markDirtyLocked()
	}
	s.mu.Unlock()
	if changed {
		s.signal()
	}
}

func (s *Service) Seeked(position time.Duration) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.hasState {
		s.state.Position = position
		s.markDirtyLocked()
	}
	s.mu.Unlock()
	s.signal()
}

// stateNeedsPublish deliberately excludes Position: while playing, Spotify
// derives position from PlayerState.Timestamp and PlaybackSpeed. The model
// emits a notification every second, and publishing those clock ticks causes
// a sustained PUT stream that eventually receives HTTP 429.
func stateNeedsPublish(previous, next playback.State) bool {
	return previous.Status != next.Status || previous.Track != next.Track ||
		math.Float64bits(previous.VolumeDB) != math.Float64bits(next.VolumeDB)
}

func (s *Service) markDirtyLocked() {
	s.stateVersion++
	s.dirty = true
	if s.rateLimited && time.Now().Before(s.retryAt) {
		s.coalesced++
	}
}

func (s *Service) Close() {
	if s == nil {
		return
	}
	select {
	case <-s.done:
		return
	default:
		close(s.done)
	}
	s.wg.Wait()
}

func (s *Service) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Service) run() {
	defer s.wg.Done()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		s.mu.Lock()
		receiver := s.receiver
		requests := s.requests
		s.mu.Unlock()

		select {
		case <-s.done:
			s.publishInactive()
			return
		case <-s.wake:
		case <-ticker.C:
		case msg, ok := <-receiver:
			if !ok {
				s.mu.Lock()
				if s.receiver == receiver {
					s.receiver = nil
					s.connectionID = ""
					s.registered = false
				}
				s.mu.Unlock()
				continue
			}
			s.handleDealerMessage(msg)
		case req, ok := <-requests:
			if !ok {
				s.mu.Lock()
				if s.requests == requests {
					s.requests = nil
				}
				s.mu.Unlock()
				continue
			}
			s.handleDealerRequest(req)
		}

		s.reconcile()
	}
}

func (s *Service) reconcile() {
	s.mu.Lock()
	state, hasState := s.state, s.hasState
	ep, version := s.endpoint, s.version
	connectionID, registered, lastPut, receiver := s.connectionID, s.registered, s.lastPut, s.receiver
	stateVersion, dirty, retryAt := s.stateVersion, s.dirty, s.retryAt
	s.mu.Unlock()

	if !hasState || ep.key == nil {
		return
	}
	if !isSpotifyState(state) {
		if !dirty {
			return
		}
		if connectionID == "" {
			s.clearDirty(version, connectionID, stateVersion)
			return
		}
		if !retryAt.IsZero() && time.Now().Before(retryAt) {
			return
		}
		if !lastPut.IsZero() && time.Since(lastPut) < minPutInterval {
			return
		}
		if registered {
			s.publishInactiveState(ep, version, connectionID, stateVersion)
		} else {
			s.clearDirty(version, connectionID, stateVersion)
		}
		return
	}
	if connectionID == "" {
		if receiver == nil {
			s.connectDealer(ep, version)
		}
		return
	}
	if !dirty {
		return
	}
	if !retryAt.IsZero() && time.Now().Before(retryAt) {
		return
	}
	if !lastPut.IsZero() && time.Since(lastPut) < minPutInterval {
		return
	}
	reason := connectpb.PutStateReason_PLAYER_STATE_CHANGED
	if !registered {
		reason = connectpb.PutStateReason_NEW_DEVICE
	}
	s.publishState(ep, version, connectionID, stateVersion, state, reason, !registered)
}

func (s *Service) publishState(ep endpoint, version uint64, connectionID string, stateVersion uint64, state playback.State, reason connectpb.PutStateReason, initial bool) {
	if !s.recordAttempt(version, connectionID) {
		return
	}
	applog.Debug("spotify connect: publishing state reason=%s", reason)
	request := newPutStateRequest(newDeviceInfo(s.name, ep.deviceID), state, reason)
	ctx, cancel := context.WithTimeout(context.Background(), publishRequestTimeout)
	err := ep.spclient.PutConnectState(ctx, connectionID, request)
	cancel()
	if err != nil {
		s.handlePublishError("put Connect state", version, connectionID, err)
		return
	}
	s.completePublish(version, connectionID, stateVersion, true)
	if initial {
		applog.Debug("spotify connect: published initial device state")
	}
}

func (s *Service) publishInactiveState(ep endpoint, version uint64, connectionID string, stateVersion uint64) {
	if !s.recordAttempt(version, connectionID) {
		return
	}
	applog.Debug("spotify connect: publishing inactive device state")
	ctx, cancel := context.WithTimeout(context.Background(), publishRequestTimeout)
	err := ep.spclient.PutConnectStateInactive(ctx, connectionID, false)
	cancel()
	if err != nil {
		s.handlePublishError("put inactive Connect state", version, connectionID, err)
		return
	}
	s.completePublish(version, connectionID, stateVersion, false)
}

func (s *Service) recordAttempt(version uint64, connectionID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.version != version || s.connectionID != connectionID {
		return false
	}
	s.lastPut = time.Now()
	return true
}

func (s *Service) completePublish(version uint64, connectionID string, stateVersion uint64, registered bool) {
	s.mu.Lock()
	if s.version != version || s.connectionID != connectionID {
		s.mu.Unlock()
		return
	}
	wasRateLimited := s.rateLimited
	coalesced := s.coalesced
	s.registered = registered
	s.retryAt = time.Time{}
	s.retryBackoff = 0
	s.rateLimited = false
	s.coalesced = 0
	if s.stateVersion == stateVersion {
		s.dirty = false
	}
	s.mu.Unlock()
	if wasRateLimited {
		applog.Debug("spotify connect: publishing resumed after rate limit; coalesced_updates=%d", coalesced)
	}
}

func (s *Service) clearDirty(version uint64, connectionID string, stateVersion uint64) {
	s.mu.Lock()
	if s.version == version && s.connectionID == connectionID && s.stateVersion == stateVersion {
		s.dirty = false
	}
	s.mu.Unlock()
}

func (s *Service) handlePublishError(action string, version uint64, connectionID string, err error) {
	now := time.Now()
	s.mu.Lock()
	if s.version != version || s.connectionID != connectionID {
		s.mu.Unlock()
		return
	}

	var limited *rateLimitedError
	rateLimited := errors.As(err, &limited)
	delay := nextRetryDelay(s.retryBackoff)
	if rateLimited && limited.retryAfter > 0 {
		delay = limited.retryAfter
		s.retryBackoff = 0
	} else {
		s.retryBackoff = delay
	}
	s.retryAt = now.Add(delay)
	s.rateLimited = rateLimited
	s.dirty = true
	s.mu.Unlock()

	if rateLimited {
		if limited.retryAfter > 0 {
			applog.Warn("spotify connect: rate limited; coalescing state updates for %s (Retry-After)", delay)
		} else {
			applog.Warn("spotify connect: rate limited without Retry-After; coalescing state updates for %s", delay)
		}
		return
	}
	applog.Warn("spotify connect: %s: %v; retrying latest state in %s", action, err, delay)
}

func nextRetryDelay(previous time.Duration) time.Duration {
	if previous <= 0 {
		return initialRetryBackoff
	}
	if previous >= maximumRetryBackoff/2 {
		return maximumRetryBackoff
	}
	return previous * 2
}

func (s *Service) connectDealer(ep endpoint, version uint64) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	err := ep.dealer.Connect(ctx)
	cancel()
	if err != nil {
		s.logError("connect Spotify dealer", err)
		return
	}
	// Match the daemon's v0.7.1 subscriptions exactly. Player commands are
	// dealer requests and require an exact URI; ordinary Connect messages are
	// prefix-matched by the dealer.
	receiver := ep.dealer.ReceiveMessage("hm://pusher/v1/connections/", "hm://connect-state/v1/")
	applog.Debug("spotify connect: subscribed to dealer message prefix=%q", "hm://pusher/v1/connections/")
	applog.Debug("spotify connect: subscribed to dealer message prefix=%q", "hm://connect-state/v1/")
	requests := ep.dealer.ReceiveRequest(playerCommandURI)
	applog.Debug("spotify connect: subscribed to dealer request uri=%q", playerCommandURI)

	s.mu.Lock()
	if s.version == version && s.endpoint.key == ep.key && s.receiver == nil {
		s.receiver = receiver
		s.requests = requests
	}
	s.mu.Unlock()
	applog.Debug("spotify connect: dealer connected; awaiting connection id")
}

func (s *Service) handleDealerMessage(msg dealer.Message) {
	applog.Debug("spotify connect: received dealer message uri=%q payload_bytes=%d connection_id_present=%t", msg.Uri, len(msg.Payload), msg.Headers["Spotify-Connection-Id"] != "")
	if strings.HasPrefix(msg.Uri, "hm://connect-state/v1/connect/volume") {
		message, err := volumeMessage(msg.Payload)
		if err != nil {
			s.logError("handle Spotify volume command", err)
			return
		}
		if err := s.dispatch(message); err != nil {
			s.logError("dispatch Spotify volume command", err)
		}
		return
	}
	if !strings.HasPrefix(msg.Uri, "hm://pusher/v1/connections/") {
		return
	}

	connectionID := msg.Headers["Spotify-Connection-Id"]
	if connectionID == "" {
		s.logError("read Spotify connection id", fmt.Errorf("pusher message has no Spotify-Connection-Id header"))
		return
	}

	s.mu.Lock()
	s.connectionID = connectionID
	s.registered = false
	s.lastPut = time.Time{}
	s.retryAt = time.Time{}
	s.retryBackoff = 0
	s.rateLimited = false
	s.coalesced = 0
	s.markDirtyLocked()
	s.mu.Unlock()
	applog.Debug("spotify connect: received dealer connection id")
	s.signal()
}

func (s *Service) handleDealerRequest(req dealer.Request) {
	applog.Debug("spotify connect: received dealer request uri=%q message_id=%d endpoint=%q sent_by=%q relative=%q position=%d value_type=%T data_bytes=%d context_uri=%q", req.MessageIdent, req.Payload.MessageId, req.Payload.Command.Endpoint, req.Payload.SentByDeviceId, req.Payload.Command.Relative, req.Payload.Command.Position, req.Payload.Command.Value, len(req.Payload.Command.Data), req.Payload.Command.Context.Uri)
	if err := s.dispatchCommand(req.Payload); err != nil {
		s.logError("handle Spotify player command", err)
		req.Reply(false)
		return
	}
	applog.Debug("spotify connect: acknowledged dealer request endpoint=%q", req.Payload.Command.Endpoint)
	req.Reply(true)
}

func (s *Service) dispatchCommand(req dealer.RequestPayload) error {
	message, err := commandMessage(req)
	if err != nil {
		return err
	}
	if message == nil {
		applog.Debug("spotify connect: player command endpoint=%q produced no Bubbletea message", req.Command.Endpoint)
		return nil
	}
	applog.Debug("spotify connect: parsed player command endpoint=%q into Bubbletea message type=%T", req.Command.Endpoint, message)
	return s.dispatch(message)
}

func (s *Service) dispatch(message any) error {
	s.mu.Lock()
	send := s.send
	registered := s.registered
	state := s.state
	s.mu.Unlock()
	if send == nil {
		applog.Debug("spotify connect: rejected Bubbletea message type=%T: sender is unavailable", message)
		return fmt.Errorf("Bubbletea message sender is unavailable")
	}
	if !registered || !isSpotifyState(state) {
		applog.Debug("spotify connect: rejected Bubbletea message type=%T: registered=%t spotify_state=%t", message, registered, isSpotifyState(state))
		return fmt.Errorf("Spotify Connect is not active")
	}
	applog.Debug("spotify connect: emitting Bubbletea message type=%T", message)
	send(message)
	return nil
}

func (s *Service) putInactive(ep endpoint, version uint64, connectionID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	err := ep.spclient.PutConnectStateInactive(ctx, connectionID, false)
	cancel()
	if err != nil {
		s.logError("put inactive Connect state", err)
		return
	}
	s.mu.Lock()
	if s.version == version && s.connectionID == connectionID {
		s.registered = false
		s.lastPut = time.Now()
	}
	s.mu.Unlock()
}

func (s *Service) publishInactive() {
	s.mu.Lock()
	ep, version := s.endpoint, s.version
	connectionID, registered := s.connectionID, s.registered
	s.mu.Unlock()
	if registered && ep.key != nil && connectionID != "" {
		s.putInactive(ep, version, connectionID)
	}
}

func (s *Service) logError(action string, err error) {
	s.mu.Lock()
	if time.Since(s.lastErr) < time.Second {
		s.mu.Unlock()
		return
	}
	s.lastErr = time.Now()
	s.mu.Unlock()
	applog.Warn("spotify connect: %s: %v", action, err)
}

var _ playback.Notifier = (*Service)(nil)
