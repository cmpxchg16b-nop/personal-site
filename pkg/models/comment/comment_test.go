package comment

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

// lastCommentId returns the id of the last comment of channelId, or "" when
// the channel has no comments.
func lastCommentId(t *testing.T, p *OnMemoryCommentProvider, channelId string) string {
	t.Helper()
	var last string
	for ev := range p.GetCommentsByChannelId(context.Background(), "reader", channelId) {
		if ev.Err != nil {
			t.Fatalf("GetCommentsByChannelId(%q): unexpected event error: %v", channelId, ev.Err)
		}
		last = ev.Comment.Id
	}
	return last
}

// collectComments drains the comment stream of channelId into a slice, in
// the order the stream delivered it.
func collectComments(t *testing.T, p *OnMemoryCommentProvider, channelId string) []*Comment {
	t.Helper()
	var comments []*Comment
	for ev := range p.GetCommentsByChannelId(context.Background(), "reader", channelId) {
		if ev.Err != nil {
			t.Fatalf("GetCommentsByChannelId(%q): unexpected event error: %v", channelId, ev.Err)
		}
		comments = append(comments, ev.Comment)
	}
	return comments
}

func TestOnMemoryCommentProvider_FirstComment(t *testing.T) {
	p := NewOnMemoryCommentProvider()
	c := &Comment{Content: "hello, world", MIMEType: MIMETypeTextPlain}

	if err := p.PutComment(context.Background(), c, "", "user-1", "post-1"); err != nil {
		t.Fatalf("PutComment: %v", err)
	}

	if c.Id == "" {
		t.Error("Id was not assigned")
	}
	if c.CreationTime == 0 {
		t.Error("CreationTime was not assigned")
	}
	if c.LastModified != c.CreationTime {
		t.Errorf("LastModified = %d, want CreationTime %d", c.LastModified, c.CreationTime)
	}
	if c.MIMEType != MIMETypeTextPlain {
		t.Errorf("MIMEType = %q, want %q", c.MIMEType, MIMETypeTextPlain)
	}
	if got, want := string(c.RichContent), c.Content; got != want {
		t.Errorf("RichContent = %q, want Content %q", got, want)
	}
	if c.ChannelId != "post-1" {
		t.Errorf("ChannelId = %q, want %q", c.ChannelId, "post-1")
	}
	if c.UserId != "user-1" {
		t.Errorf("UserId = %q, want %q", c.UserId, "user-1")
	}
	if c.SerialNumber != 0 {
		t.Errorf("SerialNumber = %d, want 0", c.SerialNumber)
	}
	if c.LastCommentId != "" {
		t.Errorf("LastCommentId = %q, want empty", c.LastCommentId)
	}
}

func TestOnMemoryCommentProvider_Chain(t *testing.T) {
	p := NewOnMemoryCommentProvider()

	lastId := ""
	for i := range 3 {
		c := &Comment{Content: fmt.Sprintf("comment %d", i)}
		if err := p.PutComment(context.Background(), c, lastId, "user-1", "post-1"); err != nil {
			t.Fatalf("PutComment %d: %v", i, err)
		}
		if c.SerialNumber != uint64(i) {
			t.Errorf("comment %d: SerialNumber = %d, want %d", i, c.SerialNumber, i)
		}
		if c.LastCommentId != lastId {
			t.Errorf("comment %d: LastCommentId = %q, want %q", i, c.LastCommentId, lastId)
		}
		lastId = c.Id
	}
}

func TestOnMemoryCommentProvider_EmptyMIMETypeDefaultsToTextPlain(t *testing.T) {
	p := NewOnMemoryCommentProvider()
	c := &Comment{Content: "no type given"}

	if err := p.PutComment(context.Background(), c, "", "user-1", "post-1"); err != nil {
		t.Fatalf("PutComment: %v", err)
	}
	if c.MIMEType != MIMETypeTextPlain {
		t.Errorf("MIMEType = %q, want default %q", c.MIMEType, MIMETypeTextPlain)
	}
}

func TestOnMemoryCommentProvider_UnsupportedMIMEType(t *testing.T) {
	p := NewOnMemoryCommentProvider()
	c := &Comment{Content: "<b>hi</b>", MIMEType: "text/html"}

	err := p.PutComment(context.Background(), c, "", "user-1", "post-1")
	if !errors.Is(err, ErrUnsupportedMIMEType) {
		t.Fatalf("PutComment: got %v, want ErrUnsupportedMIMEType", err)
	}
	// The channel stays empty: the rejected comment must not be visible.
	if got := collectComments(t, p, "post-1"); len(got) != 0 {
		t.Errorf("channel has %d comments, want 0", len(got))
	}
}

func TestOnMemoryCommentProvider_NilNewComment(t *testing.T) {
	p := NewOnMemoryCommentProvider()
	if err := p.PutComment(context.Background(), nil, "", "user-1", "post-1"); err == nil {
		t.Fatal("PutComment with nil newComment: got nil error")
	}
}

func TestOnMemoryCommentProvider_UnknownLastComment(t *testing.T) {
	p := NewOnMemoryCommentProvider()
	err := p.PutComment(context.Background(), &Comment{Content: "orphan"}, "no-such-comment", "user-1", "post-1")
	if !errors.Is(err, ErrCommentNotFound) {
		t.Fatalf("PutComment with unknown last comment id: got %v, want ErrCommentNotFound", err)
	}
}

func TestOnMemoryCommentProvider_StaleLastComment(t *testing.T) {
	p := NewOnMemoryCommentProvider()
	first := &Comment{Content: "first"}
	if err := p.PutComment(context.Background(), first, "", "user-1", "post-1"); err != nil {
		t.Fatalf("PutComment first: %v", err)
	}

	second := &Comment{Content: "second"}
	if err := p.PutComment(context.Background(), second, first.Id, "user-1", "post-1"); err != nil {
		t.Fatalf("PutComment second: %v", err)
	}

	// first is no longer the channel's last comment.
	if err := p.PutComment(context.Background(), &Comment{Content: "third"}, first.Id, "user-1", "post-1"); !errors.Is(err, ErrStaleLastComment) {
		t.Errorf("PutComment with stale last comment id: got %v, want ErrStaleLastComment", err)
	}
	// An empty last comment id on a non-empty channel is stale too.
	if err := p.PutComment(context.Background(), &Comment{Content: "third"}, "", "user-1", "post-1"); !errors.Is(err, ErrStaleLastComment) {
		t.Errorf("PutComment with empty last comment id on non-empty channel: got %v, want ErrStaleLastComment", err)
	}
}

func TestOnMemoryCommentProvider_LastCommentFromAnotherChannel(t *testing.T) {
	p := NewOnMemoryCommentProvider()
	a := &Comment{Content: "in channel a"}
	if err := p.PutComment(context.Background(), a, "", "user-1", "channel-a"); err != nil {
		t.Fatalf("PutComment: %v", err)
	}

	err := p.PutComment(context.Background(), &Comment{Content: "in channel b"}, a.Id, "user-1", "channel-b")
	if !errors.Is(err, ErrCommentChannelMismatch) {
		t.Fatalf("PutComment with last comment from another channel: got %v, want ErrCommentChannelMismatch", err)
	}
}

func TestOnMemoryCommentProvider_GetUnknownChannel(t *testing.T) {
	p := NewOnMemoryCommentProvider()
	if got := collectComments(t, p, "no-such-channel"); len(got) != 0 {
		t.Errorf("unknown channel streamed %d comments, want 0", len(got))
	}
}

func TestOnMemoryCommentProvider_GetCommentsInSerialOrder(t *testing.T) {
	p := NewOnMemoryCommentProvider()
	var stored []*Comment
	for i := range 4 {
		c := &Comment{Content: fmt.Sprintf("comment %d", i)}
		lastId := ""
		if len(stored) > 0 {
			lastId = stored[len(stored)-1].Id
		}
		if err := p.PutComment(context.Background(), c, lastId, "user-1", "post-1"); err != nil {
			t.Fatalf("PutComment %d: %v", i, err)
		}
		stored = append(stored, c)
	}

	got := collectComments(t, p, "post-1")
	if len(got) != len(stored) {
		t.Fatalf("streamed %d comments, want %d", len(got), len(stored))
	}
	for i, c := range got {
		if c.SerialNumber != uint64(i) {
			t.Errorf("event %d: SerialNumber = %d, want %d", i, c.SerialNumber, i)
		}
		// Events carry the provider's canonical objects: the same pointers
		// PutComment stored.
		if c != stored[i] {
			t.Errorf("event %d: Comment = %p, want the stored %p", i, c, stored[i])
		}
		wantLast := ""
		if i > 0 {
			wantLast = stored[i-1].Id
		}
		if c.LastCommentId != wantLast {
			t.Errorf("event %d: LastCommentId = %q, want %q", i, c.LastCommentId, wantLast)
		}
	}
}

func TestOnMemoryCommentProvider_GetSnapshot(t *testing.T) {
	p := NewOnMemoryCommentProvider()
	first := &Comment{Content: "first"}
	if err := p.PutComment(context.Background(), first, "", "user-1", "post-1"); err != nil {
		t.Fatalf("PutComment first: %v", err)
	}

	// The stream is a snapshot: comments stored after the call are not
	// part of it, even if they land before the consumer drains the stream.
	stream := p.GetCommentsByChannelId(context.Background(), "reader", "post-1")
	second := &Comment{Content: "second"}
	if err := p.PutComment(context.Background(), second, first.Id, "user-1", "post-1"); err != nil {
		t.Fatalf("PutComment second: %v", err)
	}

	n := 0
	for ev := range stream {
		if ev.Err != nil {
			t.Fatalf("unexpected event error: %v", ev.Err)
		}
		n++
	}
	if n != 1 {
		t.Errorf("snapshot streamed %d comments, want 1", n)
	}
}

func TestOnMemoryCommentProvider_IdsAreUnique(t *testing.T) {
	p := NewOnMemoryCommentProvider()
	ids := map[string]bool{}
	for i := range 100 {
		channelId := fmt.Sprintf("channel-%d", i%10)
		c := &Comment{Content: fmt.Sprintf("comment %d", i)}
		if err := p.PutComment(context.Background(), c, lastCommentId(t, p, channelId), "user-1", channelId); err != nil {
			t.Fatalf("PutComment %d: %v", i, err)
		}
		if ids[c.Id] {
			t.Fatalf("comment %d: duplicate id %q", i, c.Id)
		}
		ids[c.Id] = true
	}
}

// TestOnMemoryCommentProvider_ConcurrentPuts hammers one channel from many
// goroutines, retrying on ErrStaleLastComment as a caller of the CAS-style
// API is expected to, and verifies the result is a well-formed chain: serial
// numbers 0..n-1 each exactly once, each comment linked to its predecessor.
// Run with -race to exercise the concurrency.
func TestOnMemoryCommentProvider_ConcurrentPuts(t *testing.T) {
	p := NewOnMemoryCommentProvider()
	const n = 64

	// lastIdOf reads the channel's current last comment id; the in-memory
	// provider never emits error events. No *testing.T here: it runs on
	// spawned goroutines.
	lastIdOf := func() string {
		var last string
		for ev := range p.GetCommentsByChannelId(context.Background(), "reader", "hot") {
			last = ev.Comment.Id
		}
		return last
	}

	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := &Comment{Content: fmt.Sprintf("comment %d", i)}
			for {
				err := p.PutComment(context.Background(), c, lastIdOf(), "user-1", "hot")
				if errors.Is(err, ErrStaleLastComment) {
					continue
				}
				if err != nil {
					t.Errorf("PutComment: %v", err)
				}
				return
			}
		}()
	}
	wg.Wait()

	got := collectComments(t, p, "hot")
	if len(got) != n {
		t.Fatalf("channel has %d comments, want %d", len(got), n)
	}
	seenSerials := map[uint64]bool{}
	seenIds := map[string]bool{}
	lastId := ""
	for _, c := range got {
		if seenSerials[c.SerialNumber] {
			t.Errorf("duplicate SerialNumber %d", c.SerialNumber)
		}
		seenSerials[c.SerialNumber] = true
		if seenIds[c.Id] {
			t.Errorf("duplicate Id %q", c.Id)
		}
		seenIds[c.Id] = true
		if c.LastCommentId != lastId {
			t.Errorf("comment %q (serial %d): broken chain, LastCommentId = %q, want %q",
				c.Id, c.SerialNumber, c.LastCommentId, lastId)
		}
		lastId = c.Id
	}
	for s := range uint64(n) {
		if !seenSerials[s] {
			t.Errorf("SerialNumber %d missing", s)
		}
	}
}
