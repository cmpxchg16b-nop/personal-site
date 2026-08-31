package echobot

// This file is the echo bot's running-hash feature, decoupled from the
// message handler proper: it computes every inbound file transfer's
// running sha256 as the chunks arrive and reports it over the session's
// messaging channel — a "sha256:<hex>" chat message opened by the first
// accepted chunk (the ResponseWriter threads it on the transfer's
// announcement when one was seen), amended via chat control every
// hashReportChunkInterval chunks that moved the hash, with the final
// chunk always amending, so the report ends on the file's complete
// digest. How such a feature keeps its state is the message-handler
// implementer's own affair; this one is below.

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"log/slog"
	"sync"

	"personal-site/pkg/models/ss"
	"personal-site/pkg/rtc/msg_handler"
)

// hashReportChunkInterval throttles the running-hash amends on the
// messaging channel to one per this many accepted chunks: an amend per
// 16 KiB chunk is needlessly chatty (the frontend throttles its own
// status amends to 4/s). The running hash itself still absorbs every
// chunk, and the first and the final chunk always report, so the report
// opens immediately and ends on the file's complete digest.
const hashReportChunkInterval = 500

// hashReport is one inbound transfer's report state: the running hash
// absorbing every accepted chunk, and the msg id of the bot's own
// "sha256:..." chat message reporting it (empty until the first chunk's
// report is on the wire).
type hashReport struct {
	hash        hash.Hash
	reportMsgId ss.MsgId
}

// fileReportKey identifies one inbound transfer's report: (peer, file).
type fileReportKey struct {
	peer   ss.SubscriberId
	fileId string
}

// hashReporter tracks every in-flight inbound transfer's hashReport,
// keyed by (peer, file id). One transfer's chunks arrive serialized on
// its binary channel — the Server guarantees it — so a report's fields
// are only ever touched by one goroutine at a time; the map alone is
// shared across peers' channels, hence a sync.Map and no lock of ours.
// Entries are dropped when their transfer completes; a transfer the
// Server drops mid-stream (corrupt) or tears down with its session leaves
// its entry behind — bounded by the number of aborted transfers, and
// acceptable for an echo-purpose bot.
type hashReporter struct {
	logger  *slog.Logger
	reports sync.Map // fileReportKey → *hashReport
}

// chunk folds one accepted chunk into its transfer's running hash and
// reports it on the messaging channel: the first chunk opens the report;
// later chunks amend it, throttled; the final chunk always amends.
func (r *hashReporter) chunk(chunk *msg_handler.FileChunk, w msg_handler.ResponseWriter) {
	key := fileReportKey{peer: chunk.Peer, fileId: chunk.FileId}
	v, _ := r.reports.LoadOrStore(key, &hashReport{hash: sha256.New()})
	report := v.(*hashReport)
	hashMoved := len(chunk.Payload) > 0
	if hashMoved {
		_, _ = report.hash.Write(chunk.Payload)
	}
	text := "sha256:" + hex.EncodeToString(report.hash.Sum(nil))
	if chunk.Last {
		r.reports.Delete(key)
	}
	if report.reportMsgId == "" {
		// The first accepted chunk opens the report. The msg id is
		// recorded only once the message is on the wire, so a failed send
		// retries with the next chunk.
		id, err := w.Reply(text)
		if err != nil {
			r.logger.Warn("echobot: hash report not sent",
				"peer", chunk.Peer, "fileId", chunk.FileId, "err", err)
			return
		}
		report.reportMsgId = id
		return
	}
	// The report is open: amend it only when the hash moved, and no more
	// often than hashReportChunkInterval — unless this chunk completes the
	// transfer, which always reports.
	if !hashMoved || (!chunk.Last && (chunk.Seq+1)%hashReportChunkInterval != 0) {
		return
	}
	if err := w.Amend(report.reportMsgId, text); err != nil {
		r.logger.Warn("echobot: hash amend not sent",
			"peer", chunk.Peer, "fileId", chunk.FileId, "err", err)
	}
}
