package pokerhouse

import (
	"errors"
	"testing"
	"time"
)

func TestSignalReachesItsAddressee(t *testing.T) {
	relay := NewSignalling()
	inbox, leave := relay.Join(1, 3)
	defer leave()

	if err := relay.Send(1, 0, Signal{To: 3, Kind: "offer", Payload: "sdp"}); err != nil {
		t.Fatal(err)
	}
	select {
	case signal := <-inbox:
		if signal.From != 0 || signal.Kind != "offer" || signal.Payload != "sdp" {
			t.Errorf("received %+v", signal)
		}
	case <-time.After(time.Second):
		t.Fatal("the signal was not delivered")
	}
}

func TestSenderCannotForgeTheirSeat(t *testing.T) {
	// The From field is overwritten with the authenticated seat, so a player
	// cannot pretend to be somebody else's peer.
	relay := NewSignalling()
	inbox, leave := relay.Join(1, 3)
	defer leave()

	if err := relay.Send(1, 2, Signal{From: 99, To: 3, Kind: "offer"}); err != nil {
		t.Fatal(err)
	}
	signal := <-inbox
	if signal.From != 2 {
		t.Errorf("From = %d, want the authenticated seat 2", signal.From)
	}
}

func TestSignalToAnAbsentSeatIsDropped(t *testing.T) {
	relay := NewSignalling()
	if err := relay.Send(1, 0, Signal{To: 4, Kind: "offer"}); err != nil {
		t.Errorf("sending to a seat that is not sharing gave %v, want it to be ignored", err)
	}
}

func TestMailboxIsBounded(t *testing.T) {
	// A peer that stops reading must not be able to grow the server's memory.
	relay := NewSignalling()
	_, leave := relay.Join(1, 3)
	defer leave()

	var err error
	for i := 0; i < mailboxDepth*2; i++ {
		err = relay.Send(1, 0, Signal{To: 3, Kind: "ice", Payload: "candidate"})
		if err != nil {
			break
		}
	}
	if !errors.Is(err, ErrMailboxFull) {
		t.Errorf("got %v after flooding, want ErrMailboxFull", err)
	}
}

func TestLeavingDropsUndeliveredSignals(t *testing.T) {
	relay := NewSignalling()
	inbox, leave := relay.Join(1, 3)
	if err := relay.Send(1, 0, Signal{To: 3, Kind: "offer"}); err != nil {
		t.Fatal(err)
	}
	leave()

	// The channel is closed, so a read returns the zero value immediately
	// rather than blocking or leaking the queued message to a later session.
	for range inbox {
	}
	if peers := relay.Peers(1); len(peers) != 0 {
		t.Errorf("peers = %v after leaving, want none", peers)
	}
}

func TestReconnectReplacesTheOldMailbox(t *testing.T) {
	relay := NewSignalling()
	first, _ := relay.Join(1, 3)
	second, leave := relay.Join(1, 3)
	defer leave()

	// The first stream is shut down rather than left listening.
	select {
	case _, open := <-first:
		if open {
			t.Error("the replaced mailbox is still delivering")
		}
	case <-time.After(time.Second):
		t.Error("the replaced mailbox was not closed")
	}

	if err := relay.Send(1, 0, Signal{To: 3, Kind: "offer"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-second:
	case <-time.After(time.Second):
		t.Fatal("the new mailbox did not receive")
	}
}

func TestPeersListsConnectedSeats(t *testing.T) {
	relay := NewSignalling()
	_, leaveA := relay.Join(7, 0)
	defer leaveA()
	_, leaveB := relay.Join(7, 4)
	defer leaveB()

	if peers := relay.Peers(7); len(peers) != 2 {
		t.Errorf("peers = %v, want two seats", peers)
	}
	if peers := relay.Peers(8); len(peers) != 0 {
		t.Errorf("peers at an empty table = %v", peers)
	}
}
