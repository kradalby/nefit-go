package client

import legacy "github.com/xmppo/go-xmpp"

// transport exposes only the stanza operations the request queue consumes.
// Every production implementation owns its sockets and uses typed XML.
type transport interface {
	Recv() (any, error)
	Send(legacy.Chat) (int, error)
	SendPresence(legacy.Presence) (int, error)
	Close() error
}
