package model

import (
	"time"

	"github.com/bjarneo/cliamp/internal/playback"
)

// MultiNotifier fans playback notifications out to independent integrations
// such as MPRIS and Spotify Connect.
type MultiNotifier []playback.Notifier

func (n MultiNotifier) Update(state playback.State) {
	for _, notifier := range n {
		if notifier != nil {
			notifier.Update(state)
		}
	}
}

func (n MultiNotifier) Seeked(position time.Duration) {
	for _, notifier := range n {
		if notifier != nil {
			notifier.Seeked(position)
		}
	}
}

var _ playback.Notifier = MultiNotifier(nil)
