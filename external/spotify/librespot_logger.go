//go:build !windows

package spotify

import (
	"sync/atomic"

	"github.com/bjarneo/cliamp/applog"
	"github.com/bjarneo/cliamp/internal/browser"
	librespot "github.com/devgianlu/go-librespot"
)

// credentialLifecycleLogger forwards only a small, audited set of lifecycle
// messages from go-librespot. A general-purpose librespot logger is unsafe here:
// v0.7.1 logs client and access tokens at debug level.
//
// The selected messages contain no credentials and expose the login5 renewal
// path which is otherwise hidden by cliamp's historical NullLogger.
type credentialLifecycleLogger struct {
	login5Renewing atomic.Bool
}

var _ librespot.Logger = (*credentialLifecycleLogger)(nil)

func newCredentialLifecycleLogger() librespot.Logger { return &credentialLifecycleLogger{} }

func (*credentialLifecycleLogger) Tracef(string, ...interface{}) {}
func (l *credentialLifecycleLogger) Infof(format string, args ...interface{}) {
	switch format {
	case "to complete authentication visit the following link: %s":
		// The URL is supplied by v0.7.1's InteractiveCredentials flow. It
		// contains no credential value; forwarding it keeps the bootstrap
		// visible in cliamp's TUI just like the custom PKCE flow.
		if len(args) == 1 {
			if url, ok := args[0].(string); ok {
				applog.Info("spotify auth: librespot playback bootstrap authorization URL generated")
				notifyAuthURL(url)
				_ = browser.Open(url)
			}
		}
	case "authenticated AP":
		applog.Debug("spotify auth: Access Point authentication succeeded; reusable credential received")
	case "authenticated Login5":
		if l.login5Renewing.Swap(false) {
			applog.Debug("spotify auth: login5 access-token renewal succeeded")
		} else {
			applog.Debug("spotify auth: login5 authentication succeeded")
		}
	}
}
func (*credentialLifecycleLogger) Warnf(string, ...interface{}) {}
func (*credentialLifecycleLogger) Errorf(format string, _ ...interface{}) {
	if format == "failed reconnecting dealer" {
		applog.Warn("spotify auth: go-librespot dealer reconnect gave up")
	}
}

func (*credentialLifecycleLogger) Trace(...interface{}) {}
func (*credentialLifecycleLogger) Info(...interface{})  {}
func (*credentialLifecycleLogger) Warn(...interface{})  {}
func (*credentialLifecycleLogger) Error(...interface{}) {}
func (l *credentialLifecycleLogger) Debug(args ...interface{}) {
	if len(args) != 1 {
		return
	}
	message, _ := args[0].(string)
	switch message {
	case "renewing login5 access token":
		l.login5Renewing.Store(true)
		applog.Debug("spotify auth: login5 access-token renewal started")
	case "dealer connection opened":
		applog.Debug("spotify auth: go-librespot dealer connection opened")
	case "re-established dealer connection":
		applog.Debug("spotify auth: go-librespot dealer reconnected")
	case "dealer connection closed":
		applog.Debug("spotify auth: go-librespot dealer connection closed")
	}
}
func (*credentialLifecycleLogger) Debugf(format string, _ ...interface{}) {
	if format == "dealer connection already opened" {
		applog.Debug("spotify auth: go-librespot dealer connection already open")
	}
}

func (l *credentialLifecycleLogger) WithField(string, interface{}) librespot.Logger { return l }
func (l *credentialLifecycleLogger) WithError(error) librespot.Logger               { return l }
