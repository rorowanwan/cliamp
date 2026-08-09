package connect

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/bjarneo/cliamp/applog"
	"github.com/bjarneo/cliamp/internal/playback"
	"github.com/devgianlu/go-librespot/dealer"
	connectpb "github.com/devgianlu/go-librespot/proto/spotify/connectstate"
	"github.com/devgianlu/go-librespot/session"
)

const minPutInterval = 200 * time.Millisecond

type stateClient interface {
	PutConnectState(context.Context, string, *connectpb.PutStateRequest) error
	PutConnectStateInactive(context.Context, string, bool) error
}

type dealerClient interface {
	Connect(context.Context) error
	ReceiveMessage(...string) <-chan dealer.Message
}

type endpoint struct {
	key      any
	deviceID string
	spclient stateClient
	dealer   dealerClient
}

// Service translates cliamp playback updates into Spotify Connect state puts.
// It intentionally only receives the pusher connection-id message in Stage 1;
// player-command requests are added in Stage 2.
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
	lastErr      time.Time
	receiver     <-chan dealer.Message

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
		spclient: sess.Spclient(),
		dealer:   sess.Dealer(),
	})
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
	s.receiver = nil
	s.mu.Unlock()
	s.signal()
}

func (s *Service) Update(state playback.State) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.state = state
	s.hasState = true
	s.mu.Unlock()
	s.signal()
}

func (s *Service) Seeked(position time.Duration) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.hasState {
		s.state.Position = position
	}
	s.mu.Unlock()
	s.signal()
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
		}

		s.reconcile()
	}
}

func (s *Service) reconcile() {
	s.mu.Lock()
	state, hasState := s.state, s.hasState
	ep, version := s.endpoint, s.version
	connectionID, registered, lastPut := s.connectionID, s.registered, s.lastPut
	s.mu.Unlock()

	if !hasState || ep.key == nil {
		return
	}
	if !isSpotifyState(state) {
		if registered && connectionID != "" {
			s.putInactive(ep, version, connectionID)
		}
		return
	}
	if connectionID == "" {
		s.connectDealer(ep, version)
		return
	}
	if !lastPut.IsZero() && time.Since(lastPut) < minPutInterval {
		return
	}

	reason := connectpb.PutStateReason_PLAYER_STATE_CHANGED
	if !registered {
		reason = connectpb.PutStateReason_NEW_DEVICE
	}
	request := newPutStateRequest(newDeviceInfo(s.name, ep.deviceID), state, reason)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	err := ep.spclient.PutConnectState(ctx, connectionID, request)
	cancel()
	if err != nil {
		s.logError("put Connect state", err)
		return
	}

	s.mu.Lock()
	if s.version == version && s.connectionID == connectionID {
		s.registered = true
		s.lastPut = time.Now()
	}
	s.mu.Unlock()
	if !registered {
		applog.Debug("spotify connect: published initial device state")
	}
}

func (s *Service) connectDealer(ep endpoint, version uint64) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	err := ep.dealer.Connect(ctx)
	cancel()
	if err != nil {
		s.logError("connect Spotify dealer", err)
		return
	}
	receiver := ep.dealer.ReceiveMessage("hm://pusher/v1/connections/")

	s.mu.Lock()
	if s.version == version && s.endpoint.key == ep.key && s.receiver == nil {
		s.receiver = receiver
	}
	s.mu.Unlock()
	applog.Debug("spotify connect: dealer connected; awaiting connection id")
}

func (s *Service) handleDealerMessage(msg dealer.Message) {
	connectionID := msg.Headers["Spotify-Connection-Id"]
	if connectionID == "" {
		s.logError("read Spotify connection id", fmt.Errorf("pusher message has no Spotify-Connection-Id header"))
		return
	}

	s.mu.Lock()
	s.connectionID = connectionID
	s.registered = false
	s.lastPut = time.Time{}
	s.mu.Unlock()
	applog.Debug("spotify connect: received dealer connection id")
	s.signal()
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
