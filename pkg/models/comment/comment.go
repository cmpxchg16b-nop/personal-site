// Package comment models the site's commenting subsystem — user comments
// organized by channel (a blog post, a page, a chat thread, …) — and how
// they are stored and served to the API layer.
//
// Channels are implicit: a channel exists as soon as its first comment is
// stored. Within a channel, comments form a single linear chain: every
// comment records the id of the comment that was the channel's last when it
// was made (Comment.LastCommentId) and carries a Comment.SerialNumber one
// greater than that comment's, starting at 0 for the channel's first
// comment.
package comment

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// MIMETypeTextPlain is the only comment content type supported for now.
const MIMETypeTextPlain = "text/plain"

var (
	// ErrUnsupportedMIMEType is returned (wrapped) by
	// CommentServiceProvider.PutComment when the comment's MIME type is not
	// supported. Only MIMETypeTextPlain is supported for now. Callers match
	// it with errors.Is.
	ErrUnsupportedMIMEType = errors.New("unsupported comment MIME type")

	// ErrStaleLastComment is returned (wrapped) by
	// CommentServiceProvider.PutComment when lastCommentId is not the id of
	// the channel's current last comment — typically because a concurrent
	// PutComment appended first. The caller should re-read the channel and
	// retry with the new last comment's id. Callers match it with errors.Is.
	ErrStaleLastComment = errors.New("last comment id is not the channel's last comment")

	// ErrCommentNotFound is returned (wrapped) by
	// CommentServiceProvider.PutComment when no comment with the id given as
	// lastCommentId exists. Callers match it with errors.Is.
	ErrCommentNotFound = errors.New("comment not found")

	// ErrCommentChannelMismatch is returned (wrapped) by
	// CommentServiceProvider.PutComment when the comment with the id given
	// as lastCommentId belongs to a different channel than the one being
	// commented on. Callers match it with errors.Is.
	ErrCommentChannelMismatch = errors.New("comment belongs to another channel")
)

// Comment is a single comment datum.
//
// The caller supplies only Content and MIMEType; the provider assigns every
// other field when the comment is stored (see
// CommentServiceProvider.PutComment). Once stored, a Comment is immutable:
// it is shared freely across goroutines and must not be modified.
type Comment struct {
	// Id is the comment's globally unique identifier, generated statelessly
	// and independently (a random RFC 4122 version 4 UUID) when the comment
	// is stored.
	Id string

	// CreationTime is when the comment was stored, in seconds since the
	// Unix epoch (UTC).
	CreationTime uint64

	// LastModified is when the comment was last edited, in seconds since
	// the Unix epoch (UTC). Comment edition is not designed yet, so this
	// always equals CreationTime.
	LastModified uint64

	// Content is the comment's text. For text/plain (the only supported
	// type) it is the comment body itself.
	Content string

	// MIMEType is the media type of Content. Only MIMETypeTextPlain is
	// supported for now.
	MIMEType string

	// RichContent is the rendered form of Content. Plain text has no
	// rendering step, so for now it is simply Content encoded as UTF-8.
	RichContent []byte

	// ChannelId identifies the channel (a blog post, a page, a chat
	// thread, …) the comment belongs to. It is opaque to this package.
	ChannelId string

	// UserId identifies the user who created the comment. It is opaque to
	// this package.
	UserId string

	// SerialNumber numbers the comment within its channel: 0 for the
	// channel's first comment, then 1, 2, … in creation order.
	SerialNumber uint64

	// LastCommentId is the id of the comment that was the channel's last
	// when this comment was made — the previous link of the channel's
	// chain. It is empty for the channel's first comment (serial number 0).
	LastCommentId string
}

// CommentDataEvent is one event of the stream returned by
// CommentServiceProvider.GetCommentsByChannelId. Exactly one of its fields
// is set: Comment carries the next comment of the channel, or Err reports a
// failure — an Err event is always the last event before the stream is
// closed.
type CommentDataEvent struct {
	// Comment is the next comment of the channel: the provider's canonical,
	// immutable object.
	Comment *Comment

	// Err is non-nil when streaming failed.
	Err error
}

// CommentServiceProvider stores and serves comments.
type CommentServiceProvider interface {
	// GetCommentsByChannelId streams the comments of channelId in
	// serial-number order and closes the stream when done. requestingUser
	// is the opaque id of the user the stream is served to; providers
	// without per-user visibility rules ignore it. The context is available
	// for implementations backed by a remote store; in-memory
	// implementations may ignore it.
	GetCommentsByChannelId(ctx context.Context, requestingUser string, channelId string) <-chan CommentDataEvent

	// PutComment stores newComment as the next comment of channelId, on
	// behalf of requestingUser.
	//
	// The caller sets only Content and MIMEType (an empty MIMEType defaults
	// to MIMETypeTextPlain); PutComment assigns Id, CreationTime,
	// LastModified, RichContent, ChannelId, UserId, SerialNumber and
	// LastCommentId before storing. After a successful call the comment is
	// immutable and must not be modified. On error the comment was not
	// stored (some of its fields may already have been assigned).
	//
	// lastCommentId is the id of the channel's last comment as the caller
	// knows it: empty when the caller believes the channel has no comments
	// yet. Finding the last comment is the caller's job — read the channel
	// first and take the last comment's id. PutComment fails with an error
	// wrapping ErrStaleLastComment when the channel has moved past
	// lastCommentId, ErrCommentNotFound when no comment has that id,
	// ErrCommentChannelMismatch when that comment belongs to another
	// channel, or ErrUnsupportedMIMEType when the comment's MIME type is not
	// supported.
	PutComment(ctx context.Context, newComment *Comment, lastCommentId string, requestingUser string, channelId string) error
}

// OnMemoryCommentProvider is a CommentServiceProvider keeping all comments
// in process memory; everything is lost on restart.
//
// Two maps hold the state: lastCommentByChannel maps each channel id to the
// id of the channel's last comment, and commentsById maps each globally
// unique comment id to the comment itself. Appending is a compare-and-swap
// by value on the channel's last comment id; readers walk the chain of
// Comment.LastCommentId links, every comment immutable once stored, so no
// locks are taken beyond sync.Map's own bookkeeping.
type OnMemoryCommentProvider struct {
	// lastCommentByChannel maps a channel id to the id of the channel's
	// last comment. A channel is registered with an empty id on first
	// touch, so the append's compare-and-swap always has a value to compare
	// against.
	lastCommentByChannel sync.Map

	// commentsById maps a comment's globally unique id to the comment
	// (*Comment). Comments are immutable once stored.
	commentsById sync.Map
}

var _ CommentServiceProvider = (*OnMemoryCommentProvider)(nil)

// NewOnMemoryCommentProvider constructs an empty OnMemoryCommentProvider.
func NewOnMemoryCommentProvider() *OnMemoryCommentProvider {
	return &OnMemoryCommentProvider{}
}

func (p *OnMemoryCommentProvider) GetCommentsByChannelId(_ context.Context, _ string, channelId string) <-chan CommentDataEvent {
	// Channels are implicit: an unknown channel simply has no comments.
	var comments []*Comment
	var walkErr error
	if v, ok := p.lastCommentByChannel.Load(channelId); ok {
		// Every stored comment is immutable, so the chain reachable from the
		// last comment id loaded above is a consistent snapshot; comments
		// appended concurrently afterwards are not part of it. Walking it
		// yields newest first.
		for id := v.(string); id != ""; {
			v, ok := p.commentsById.Load(id)
			if !ok {
				// Unreachable: an id enters the chain only after its comment
				// is stored, and stored comments are never removed.
				walkErr = fmt.Errorf("comment: channel %q: comment %q missing from the store", channelId, id)
				break
			}
			c := v.(*Comment)
			comments = append(comments, c)
			id = c.LastCommentId
		}
	}
	// The snapshot is fully materialized, so a channel buffered for exactly
	// the events to send can be filled and closed right away, in
	// serial-number order, without spawning a goroutine that would have to
	// be prevented from leaking when the consumer stops reading.
	out := make(chan CommentDataEvent, len(comments)+1)
	for i := len(comments) - 1; i >= 0; i-- {
		out <- CommentDataEvent{Comment: comments[i]}
	}
	if walkErr != nil {
		out <- CommentDataEvent{Err: walkErr}
	}
	close(out)
	return out
}

func (p *OnMemoryCommentProvider) PutComment(_ context.Context, newComment *Comment, lastCommentId string, requestingUser string, channelId string) error {
	if newComment == nil {
		return errors.New("comment: PutComment called with a nil newComment")
	}
	mimeType := newComment.MIMEType
	if mimeType == "" {
		mimeType = MIMETypeTextPlain
	}
	if mimeType != MIMETypeTextPlain {
		return fmt.Errorf("comment: %q: %w (only %q is supported)",
			newComment.MIMEType, ErrUnsupportedMIMEType, MIMETypeTextPlain)
	}

	// The caller's view of the channel's last comment must be a real comment
	// of this channel.
	var prev *Comment
	if lastCommentId != "" {
		v, ok := p.commentsById.Load(lastCommentId)
		if !ok {
			return fmt.Errorf("comment: last comment id %q: %w", lastCommentId, ErrCommentNotFound)
		}
		prev = v.(*Comment)
		if prev.ChannelId != channelId {
			return fmt.Errorf("comment: last comment %q is in channel %q, not %q: %w",
				lastCommentId, prev.ChannelId, channelId, ErrCommentChannelMismatch)
		}
	}

	current, _ := p.lastCommentByChannel.LoadOrStore(channelId, "")
	if current.(string) != lastCommentId {
		return fmt.Errorf("comment: channel %q: %w", channelId, ErrStaleLastComment)
	}

	var serial uint64
	if prev != nil {
		serial = prev.SerialNumber + 1
	}
	now := uint64(time.Now().Unix())
	newComment.Id = uuid.NewString()
	newComment.CreationTime = now
	newComment.LastModified = now
	newComment.MIMEType = mimeType
	newComment.RichContent = []byte(newComment.Content)
	newComment.ChannelId = channelId
	newComment.UserId = requestingUser
	newComment.SerialNumber = serial
	newComment.LastCommentId = lastCommentId

	// All fields must be assigned before the comment is stored: from this
	// point on it is shared with concurrent readers and immutable. The
	// comment must be resolvable by id before its id can enter a channel
	// chain, so store it first, then publish with a compare-and-swap on the
	// channel's last comment id — a value comparison of the ids.
	p.commentsById.Store(newComment.Id, newComment)
	if !p.lastCommentByChannel.CompareAndSwap(channelId, lastCommentId, newComment.Id) {
		// A concurrent PutComment appended first; lastCommentId is no longer
		// the channel's last comment. The stored comment never entered the
		// chain, so remove it again.
		p.commentsById.Delete(newComment.Id)
		return fmt.Errorf("comment: channel %q: %w", channelId, ErrStaleLastComment)
	}
	return nil
}
