package relay

import (
	"time"

	"github.com/satbooth/satbooth/nostr"
)

// MaxMessage is the read limit for one client frame: the largest event plus room for the envelope
// and a few filters. The reader enforces it; the parser does not.
const MaxMessage = nostr.MaxEventJSON + 4096

// Limits shared by the parser and the server.
const (
	WriteTimeout        = 10 * time.Second
	PingInterval        = 30 * time.Second
	PingTimeout         = 10 * time.Second
	QueueFrames         = 256
	MaxConnsPerListener = 32
	MaxSubsPerConn      = 16
	MaxFiltersPerReq    = 4
	MaxAuthed           = 4
	AuthDeadline        = 10 * time.Second // the private listener closes a connection that has not authenticated by then
)
