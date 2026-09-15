package pokerhouse

import (
	"errors"
	"sync"
	"time"
)

// Signalling relays WebRTC offers, answers and ICE candidates between the
// players at a table.
//
// The media itself never touches this server: browsers connect to each other
// directly in a mesh, and all that passes through here is the handshake needed
// to set that up. That is a deliberate choice, and the reason it is viable is
// table size — six seats is fifteen peer connections, which a browser handles
// comfortably. A media server would be needed beyond about eight participants,
// and would mean the operator's infrastructure carrying video of identifiable
// people. Not carrying it is a stronger privacy position than promising not to
// look at it.
//
// Nothing here is recorded or persisted. Messages live in a bounded in-memory
// mailbox, are delivered once, and are dropped when a player leaves.
type Signalling struct {
	mu     sync.Mutex
	tables map[int64]*tableSignals
}

// Signal is one handshake message between two seats.
type Signal struct {
	// From and To are seat numbers. A message is always addressed to a
	// specific seat: there is no broadcast, because every WebRTC negotiation
	// is between exactly two peers.
	From int    `json:"from"`
	To   int    `json:"to"`
	Kind string `json:"kind"`
	// Payload is opaque session-description or candidate data, passed through
	// without being parsed. The server has no reason to understand it and no
	// business inspecting it.
	Payload string `json:"payload"`
}

// mailboxDepth bounds what one seat can queue for another. A peer that has
// stopped reading must not be able to grow the server's memory.
const mailboxDepth = 64

// ErrMailboxFull is returned when a peer is not keeping up.
var ErrMailboxFull = errors.New("pokerhouse: signalling mailbox is full")

type tableSignals struct {
	mailboxes map[int]chan Signal
	lastSeen  map[int]time.Time
}

// NewSignalling builds the relay.
func NewSignalling() *Signalling {
	return &Signalling{tables: make(map[int64]*tableSignals)}
}

// Join opens a seat's mailbox and returns it, along with a function to close
// it. Leaving the table drops anything undelivered.
func (s *Signalling) Join(tableID int64, seat int) (<-chan Signal, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()

	table, ok := s.tables[tableID]
	if !ok {
		table = &tableSignals{
			mailboxes: make(map[int]chan Signal),
			lastSeen:  make(map[int]time.Time),
		}
		s.tables[tableID] = table
	}
	// A seat reconnecting replaces its old mailbox; the stale one is closed so
	// the previous stream shuts down rather than lingering.
	if existing, ok := table.mailboxes[seat]; ok {
		close(existing)
	}
	mailbox := make(chan Signal, mailboxDepth)
	table.mailboxes[seat] = mailbox
	table.lastSeen[seat] = time.Now()

	return mailbox, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if current, ok := table.mailboxes[seat]; ok && current == mailbox {
			delete(table.mailboxes, seat)
			delete(table.lastSeen, seat)
			close(mailbox)
		}
		if len(table.mailboxes) == 0 {
			delete(s.tables, tableID)
		}
	}
}

// Send delivers one handshake message to its addressee.
//
// The sender's seat is taken from the caller's authenticated session, not from
// the message, so a player cannot impersonate another seat's negotiation.
func (s *Signalling) Send(tableID int64, from int, signal Signal) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	table, ok := s.tables[tableID]
	if !ok {
		return nil // nobody is listening at that table
	}
	mailbox, ok := table.mailboxes[signal.To]
	if !ok {
		return nil // that seat is not sharing
	}
	signal.From = from

	select {
	case mailbox <- signal:
		return nil
	default:
		return ErrMailboxFull
	}
}

// Peers lists the seats currently connected to the relay at a table.
func (s *Signalling) Peers(tableID int64) []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	table, ok := s.tables[tableID]
	if !ok {
		return nil
	}
	seats := make([]int, 0, len(table.mailboxes))
	for seat := range table.mailboxes {
		seats = append(seats, seat)
	}
	return seats
}
