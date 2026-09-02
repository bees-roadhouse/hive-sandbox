package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/bees-roadhouse/hive-sandbox/internal/chat"
	"github.com/bees-roadhouse/hive-sandbox/internal/httpauth"
	"github.com/bees-roadhouse/hive-sandbox/internal/sse"
	"github.com/bees-roadhouse/hive-sandbox/internal/store"
)

// Stream tuning. RetryHint is what the browser waits before reconnecting;
// KeepAlive holds the connection open through proxies; AuthRecheck bounds how
// long the stream keeps delivering on a credential and a grant nobody has
// re-confirmed. The last one is deliberately its own number rather than a
// reuse of KeepAlive, for the reason the bus gives: somebody will raise the
// keepalive for a proxy that deserves it, and the revocation window must not
// move because of that.
const (
	streamRetryHint   = 2 * time.Second
	streamKeepAlive   = 15 * time.Second
	streamAuthRecheck = 15 * time.Second

	// replayPage is one read of the table, and replayPages bounds a connect.
	// Past the bound the stream simply goes live: frames are how a reply is
	// watched, not the transcript, which is the messages endpoint and is never
	// bounded this way.
	replayPage  = 500
	replayPages = 20
)

// streamCursor is a position in a conversation's run events: the last frame
// the client has, as (request sequence, event sequence). Both start at 1, so
// the zero value is "before everything".
type streamCursor struct {
	req, seq int
}

func (c streamCursor) String() string { return strconv.Itoa(c.req) + ":" + strconv.Itoa(c.seq) }

func (c streamCursor) before(req, seq int) bool {
	return c.req < req || (c.req == req && c.seq < seq)
}

func parseStreamCursor(s string) (streamCursor, error) {
	if s == "" {
		return streamCursor{}, nil
	}
	a, b, ok := strings.Cut(s, ":")
	if !ok {
		return streamCursor{}, errors.New("cursor is not req:seq")
	}
	req, err := strconv.Atoi(a)
	if err != nil || req < 0 {
		return streamCursor{}, errors.New("cursor request sequence is not a number")
	}
	seq, err := strconv.Atoi(b)
	if err != nil || seq < 0 {
		return streamCursor{}, errors.New("cursor event sequence is not a number")
	}
	return streamCursor{req: req, seq: seq}, nil
}

// chatStream serves GET /conversations/{id}/stream.
//
// Frames come off agent_run_events, not the bus. One run is a single writer
// appending in seq order, so a bare (request, seq) pair is a correct cursor
// and every frame can carry one; the reasons the bus needs an overlap window
// and settled watermarks are structurally absent here (see the worker's
// package doc for why the runs are not mirrored onto the bus).
//
// The hub is subscribed BEFORE the connect-time replay, as the bus does: the
// other order leaves a gap between the replay query and joining the hub that
// nothing reports. A frame delivered by both is dropped by the cursor.
func (a *API) chatStream(w http.ResponseWriter, r *http.Request, cred store.Credential) {
	id, ok := conversationID(w, r)
	if !ok {
		return
	}
	cursor, err := parseStreamCursor(lastEventID(r))
	if err != nil {
		fail(w, http.StatusBadRequest, "bad Last-Event-ID")
		return
	}

	ctx := r.Context()
	updates, stop := a.hub.Subscribe(id, 256)
	defer stop()

	// The first read is also the authorization: OpenTurns goes through the
	// predicate and a stranger gets 404 here exactly as on every other route.
	open, err := a.chat.OpenTurns(ctx, cred, id)
	if err != nil {
		chatError(w, err, "stream open turns")
		return
	}
	// A fresh subscriber replays the turn in flight from its start, so a page
	// reloaded mid-answer shows the answer so far rather than only what comes
	// after. A pending turn has no events yet and the same position covers it.
	if cursor == (streamCursor{}) && len(open) > 0 {
		cursor = streamCursor{req: open[0].RequestSeq, seq: 0}
	}

	if err := a.stream(ctx, w, r, cred, id, cursor, open, updates); err != nil && !clientGone(err) {
		slog.Warn("chat stream", "err", err, "conversation", id, "actor", cred.ActorID)
	}
}

func clientGone(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, http.ErrHandlerTimeout)
}

func (a *API) stream(ctx context.Context, w http.ResponseWriter, r *http.Request, cred store.Credential,
	id uuid.UUID, cursor streamCursor, open []store.TurnState, updates <-chan chat.Update) error {

	sw := sse.New(w)
	if err := sw.Retry(streamRetryHint); err != nil {
		return err
	}
	for _, t := range open {
		if err := emitTurn(sw, chat.TurnUpdate{RequestSeq: t.RequestSeq, State: t.State}); err != nil {
			return err
		}
	}

	// --- catch up from the table ---------------------------------------------
	last, err := a.replay(ctx, sw, cred, id, cursor, streamCursor{req: 1 << 30})
	if err != nil {
		return err
	}

	// --- live -----------------------------------------------------------------
	// authorised re-resolves the credential AND the grant on the interval.
	// Both, because both can be revoked under an open stream: "log out
	// everywhere" and "unshare this thread" each have to end delivery within
	// the window, and a stream that only re-checked one would keep going on
	// the other.
	auth := httpauth.Bearer(a.st.Pool())
	confirmed := time.Now()
	authorised := func() bool {
		if time.Since(confirmed) < streamAuthRecheck {
			return true
		}
		fresh, authErr := auth(ctx, r)
		if authErr != nil || fresh != cred {
			return false
		}
		if _, denied := a.st.Guard().Authorize(ctx, cred,
			store.Subject{Kind: store.SubjectConversation, ID: id}, store.AccessRead, "chat.stream"); denied != nil {
			return false
		}
		confirmed = time.Now()
		return true
	}

	keep := time.NewTicker(streamKeepAlive)
	defer keep.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil

		case u, ok := <-updates:
			if !ok {
				return nil
			}
			if !authorised() {
				return nil
			}
			switch {
			case u.Turn != nil:
				if err = emitTurn(sw, *u.Turn); err != nil {
					return err
				}
			case u.Run != nil:
				if !last.before(u.Run.RequestSeq, u.Run.Seq) {
					// Already sent during replay, or an older frame the
					// subscription caught before the replay query ran.
					continue
				}
				// A gap means the hub dropped frames on a full buffer. They
				// are in the table, which is the transport; read them rather
				// than hand the client a hole.
				if gapBefore(last, u.Run) {
					if last, err = a.replay(ctx, sw, cred, id, last,
						streamCursor{req: u.Run.RequestSeq, seq: u.Run.Seq}); err != nil {
						return err
					}
					if !last.before(u.Run.RequestSeq, u.Run.Seq) {
						continue
					}
				}
				if err := emitFrame(sw, *u.Run); err != nil {
					return err
				}
				last = streamCursor{req: u.Run.RequestSeq, seq: u.Run.Seq}
			}

		case <-keep.C:
			if !authorised() {
				return nil
			}
			if err := sw.Comment("keepalive"); err != nil {
				return err
			}
		}
	}
}

// gapBefore reports whether frames sit between the last one sent and this one.
// Within a turn, seq is dense; across turns, the first frame of the next turn
// is seq 1 and anything else of the previous turn is unknowable here, so only
// the in-turn case is a detectable gap.
func gapBefore(last streamCursor, f *chat.Frame) bool {
	return f.RequestSeq == last.req && f.Seq > last.seq+1
}

// replay emits frames from the table after `from` and before or at `until`,
// page by page, and returns the position of the last frame emitted.
func (a *API) replay(ctx context.Context, sw *sse.Writer, cred store.Credential, id uuid.UUID,
	from, until streamCursor) (streamCursor, error) {

	last := from
	for range replayPages {
		events, err := a.chat.TurnEvents(ctx, cred, id, last.req, last.seq, replayPage)
		if err != nil {
			return last, fmt.Errorf("replay: %w", err)
		}
		for _, ev := range events {
			if until.before(ev.RequestSeq, ev.Seq) {
				return last, nil
			}
			if err := emitFrame(sw, chat.FrameOfRecord(ev)); err != nil {
				return last, err
			}
			last = streamCursor{req: ev.RequestSeq, seq: ev.Seq}
		}
		if len(events) < replayPage {
			return last, nil
		}
	}
	return last, nil
}

func emitFrame(sw *sse.Writer, f chat.Frame) error {
	body, err := json.Marshal(f)
	if err != nil {
		return err
	}
	return sw.Event("run", streamCursor{req: f.RequestSeq, seq: f.Seq}.String(), string(body))
}

// emitTurn carries no id: a turn update is not a position in the frame
// sequence, and letting it move the client's cursor would make the next
// reconnect resume from a place that is not a frame.
func emitTurn(sw *sse.Writer, t chat.TurnUpdate) error {
	body, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return sw.Event("turn", "", string(body))
}

func lastEventID(r *http.Request) string {
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		return v
	}
	// EventSource sends the header itself; this is for non-browser clients and
	// for a deliberate restart from a known point.
	return r.URL.Query().Get("last_event_id")
}
