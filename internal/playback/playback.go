package playback

import "time"

type (
	PlayPauseMsg   struct{}
	PlayMsg        struct{}
	PauseMsg       struct{}
	NextMsg        struct{}
	PrevMsg        struct{}
	StopMsg        struct{}
	QuitMsg        struct{}
	SeekMsg        struct{ Offset time.Duration }
	SetPositionMsg struct {
		Position time.Duration
	}
	SetVolumeMsg struct{ VolumeDB float64 }
	TransferMsg  struct {
		Tracks   []Track
		Position time.Duration
		Paused   bool
	}
)

type Status string

const (
	StatusStopped Status = "Stopped"
	StatusPlaying Status = "Playing"
	StatusPaused  Status = "Paused"
)

type Track struct {
	Title       string
	Artist      string
	Album       string
	Genre       string
	TrackNumber int
	URL         string
	ArtURL      string
	Duration    time.Duration
}

type State struct {
	Status   Status
	Track    Track
	VolumeDB float64
	Position time.Duration
	Seekable bool
}

type Notifier interface {
	Update(State)
	Seeked(time.Duration)
}
