package client

import (
	"context"

	legacy "github.com/xmppo/go-xmpp"
)

// transport exposes only the stanza operations the request queue consumes.
// Every production implementation owns its sockets and uses typed XML.
type transport interface {
	Recv() (any, error)
	Send(legacy.Chat) (int, error)
	SendPresence(legacy.Presence) (int, error)
	Close() error
}

// Queued transports decide when a request may reach the device. Cancellation
// before that point must not retire an otherwise healthy connection.
type contextSender interface {
	SendContext(context.Context, legacy.Chat) (int, error)
}

type unsentError struct{ err error }

func (e *unsentError) Error() string { return e.err.Error() }
func (e *unsentError) Unwrap() error { return e.err }
