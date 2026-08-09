package model

import (
	"testing"
	"time"

	"github.com/bjarneo/cliamp/internal/playback"
)

func TestMultiNotifierFansOut(t *testing.T) {
	first, second := &fakeNotifier{}, &fakeNotifier{}
	notifier := MultiNotifier{first, nil, second}
	state := playback.State{Status: playback.StatusPlaying}
	notifier.Update(state)
	notifier.Seeked(12 * time.Second)

	for _, target := range []*fakeNotifier{first, second} {
		if len(target.updates) != 1 || target.updates[0] != state {
			t.Fatalf("updates = %#v, want one state", target.updates)
		}
		if len(target.seeked) != 1 || target.seeked[0] != 12*time.Second {
			t.Fatalf("seeked = %#v, want 12s", target.seeked)
		}
	}
}
