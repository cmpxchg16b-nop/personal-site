package sipbot

import "testing"

// A ringing local end must mark ended in the same critical section that
// decides no ACK wait is needed. Otherwise a late activate could start an
// inbound answer after the decision and put the subsequent Hangup in pre-ACK.
func TestPeerCallEndLocalRingingPreventsLateActivate(t *testing.T) {
	c := &peerCall{
		accepted:      make(chan struct{}),
		answerDone:    make(chan struct{}),
		pendingCancel: func() {},
	}
	if _, ok := c.endLocal(); !ok {
		t.Fatal("the first local end did not win")
	}
	if c.activate() {
		t.Fatal("a ringing call activated after its local end")
	}
	if _, ok := c.endLocal(); ok {
		t.Fatal("the local end ran more than once")
	}
}

// Once the inbound answer is done, an active call ends without consulting the
// dialog or waiting. This also exercises the answered half of endLocal.
func TestPeerCallEndLocalAfterAnswer(t *testing.T) {
	done := make(chan struct{})
	close(done)
	c := &peerCall{
		accepted:      make(chan struct{}),
		answerDone:    done,
		pendingCancel: func() {},
		phase:         phaseActive,
	}
	if _, ok := c.endLocal(); !ok {
		t.Fatal("the answered local end did not win")
	}
	if _, ok := c.end(); ok {
		t.Fatal("end ran again after endLocal")
	}
}
