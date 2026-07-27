package main

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

type EventKind int

const (
	EvComposing EventKind = iota // "sta scrivendo" / "sta registrando"
	EvPaused                     // explicit stop from WhatsApp
	EvMessage                    // a message arrived: she was typing, now she sent it
	EvDelivered                  // our message reached her device (two grey ticks)
	EvRead                       // she opened the chat and read it (blue ticks)
	EvPlayed                     // she played a voice note we sent
)

type Event struct {
	Kind  EventKind
	Media string // "text" or "audio"
	At    time.Time
}

// State is the snapshot handed to the publisher.
type State struct {
	Available       bool
	Typing          bool
	Status          string // idle | typing | recording | disconnected
	CurrentDuration int    // seconds in the running session, 0 when idle
	LastDuration    int    // seconds of the last completed session
	LastTypingAt    time.Time
	SessionsToday   int
	SecondsToday    int

	// Read receipts. Unlike typing, these only move when we send her something:
	// no outgoing messages means no signal at all, not "she wasn't around".
	LastDeliveredAt time.Time
	LastReadAt      time.Time
	LastPlayedAt    time.Time
	ReadsToday      int

	// Incoming messages: only when they arrive and what type they are, never
	// their content.
	LastMessageAt time.Time
	MessagesToday int

	Attributes map[string]any
}

// Tracker turns the raw composing/paused stream into a debounced ON/OFF state
// with session accounting.
//
// WhatsApp re-sends "composing" every few seconds while someone keeps typing and
// sends "paused" when they stop, but both can get lost (bad network, app killed,
// message sent). So the session ends on whichever comes first:
//   - "paused" plus OffDelay of grace (so micro-pauses between words don't chop
//     the session into pieces),
//   - an incoming message from her,
//   - ComposingTimeout without any refresh (the safety net).
//
// The recorded duration always stops at the last real evidence of typing, never
// at the end of the grace period, so the numbers stay honest.
type Tracker struct {
	cfg    Config
	pub    Publisher
	events chan Event
	log    *slog.Logger

	mu            sync.Mutex
	connected     bool
	typing        bool
	media         string
	sessionStart  time.Time
	lastComposing time.Time
	pausedAt      time.Time
	lastDuration  time.Duration
	lastTypingAt  time.Time
	endedBy       string
	sessions      int
	secondsToday  time.Duration
	day           string

	lastDelivered time.Time
	lastRead      time.Time
	lastPlayed    time.Time
	readsToday    int

	lastMessage     time.Time
	lastMessageType string
	messagesToday   int
}

func NewTracker(cfg Config, pub Publisher, log *slog.Logger) *Tracker {
	return &Tracker{
		cfg:    cfg,
		pub:    pub,
		events: make(chan Event, 64),
		log:    log,
		media:  "text",
		day:    time.Now().Format("2006-01-02"),
	}
}

func (t *Tracker) Send(ev Event) {
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	select {
	case t.events <- ev:
	default:
		t.log.Warn("tracker queue full, dropping event")
	}
}

func (t *Tracker) SetConnected(v bool) {
	t.mu.Lock()
	if t.connected == v {
		t.mu.Unlock()
		return
	}
	t.connected = v
	if !v && t.typing {
		// Connection died mid-session: close it at the last known evidence.
		t.endSession(t.lastComposing, "disconnected")
	}
	t.mu.Unlock()
	t.publish()
}

func (t *Tracker) Run(ctx context.Context) {
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()

	t.publish()
	lastPublish := time.Now()

	for {
		select {
		case <-ctx.Done():
			return

		case ev := <-t.events:
			t.handle(ev)
			t.publish()
			lastPublish = time.Now()

		case now := <-tick.C:
			changed := t.evaluate(now)
			if changed || (t.isTyping() && now.Sub(lastPublish) >= t.cfg.Tick) {
				t.publish()
				lastPublish = now
			}
		}
	}
}

func (t *Tracker) handle(ev Event) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rollover(ev.At)

	switch ev.Kind {
	case EvComposing:
		if ev.Media != "" {
			t.media = ev.Media
		}
		t.pausedAt = time.Time{}
		t.lastComposing = ev.At
		if !t.typing {
			t.typing = true
			t.sessionStart = ev.At
			t.sessions++
			t.endedBy = ""
			t.log.Info("typing started", "contact", t.cfg.Name, "media", t.media)
		}

	case EvPaused:
		if !t.typing {
			return
		}
		t.pausedAt = ev.At
		if t.cfg.OffDelay <= 0 {
			t.endSession(ev.At, "paused")
		}

	case EvMessage:
		if t.typing {
			t.endSession(ev.At, "message")
		}
		t.lastMessage = ev.At
		t.messagesToday++
		if ev.Media != "" {
			t.lastMessageType = ev.Media
		}

	// Receipts can arrive out of order or be replayed after a reconnect, so
	// timestamps only ever move forward.
	case EvDelivered:
		if ev.At.After(t.lastDelivered) {
			t.lastDelivered = ev.At
		}

	case EvRead:
		if ev.At.After(t.lastRead) {
			t.lastRead = ev.At
			t.readsToday++
			t.log.Info("messages read", "contact", t.cfg.Name, "at", ev.At.Format(time.RFC3339))
		}

	case EvPlayed:
		if ev.At.After(t.lastPlayed) {
			t.lastPlayed = ev.At
			t.log.Info("voice note played", "contact", t.cfg.Name, "at", ev.At.Format(time.RFC3339))
		}
	}
}

// evaluate applies the timeouts; it reports whether the state changed.
func (t *Tracker) evaluate(now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rollover(now)

	if !t.typing {
		return false
	}
	if !t.pausedAt.IsZero() && now.Sub(t.pausedAt) >= t.cfg.OffDelay {
		t.endSession(t.pausedAt, "paused")
		return true
	}
	if now.Sub(t.lastComposing) >= t.cfg.ComposingTimeout {
		t.endSession(t.lastComposing, "timeout")
		return true
	}
	return false
}

// endSession must be called with the mutex held.
func (t *Tracker) endSession(end time.Time, reason string) {
	if !t.typing {
		return
	}
	if end.Before(t.sessionStart) {
		end = t.sessionStart
	}
	d := end.Sub(t.sessionStart)
	if d < time.Second {
		d = time.Second
	}
	t.typing = false
	t.pausedAt = time.Time{}
	t.lastDuration = d
	t.lastTypingAt = end
	t.secondsToday += d
	t.endedBy = reason
	t.log.Info("typing stopped", "contact", t.cfg.Name, "seconds", int(d.Seconds()), "reason", reason)
}

// rollover resets the daily counters at local midnight; mutex must be held.
func (t *Tracker) rollover(now time.Time) {
	day := now.Format("2006-01-02")
	if day == t.day {
		return
	}
	t.day = day
	t.sessions = 0
	t.secondsToday = 0
	t.readsToday = 0
	t.messagesToday = 0
	if t.typing {
		// A session spanning midnight keeps running but counts from now on.
		t.sessions = 1
	}
}

func (t *Tracker) isTyping() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.typing
}

func (t *Tracker) snapshot() State {
	t.mu.Lock()
	defer t.mu.Unlock()

	s := State{
		Available:       t.connected,
		Typing:          t.typing,
		LastDuration:    int(t.lastDuration.Seconds()),
		LastTypingAt:    t.lastTypingAt,
		SessionsToday:   t.sessions,
		SecondsToday:    int(t.secondsToday.Seconds()),
		LastDeliveredAt: t.lastDelivered,
		LastReadAt:      t.lastRead,
		LastPlayedAt:    t.lastPlayed,
		ReadsToday:      t.readsToday,
		LastMessageAt:   t.lastMessage,
		MessagesToday:   t.messagesToday,
	}

	switch {
	case !t.connected:
		s.Status = "disconnected"
	case t.typing && t.media == "audio":
		s.Status = "recording"
	case t.typing:
		s.Status = "typing"
	default:
		s.Status = "idle"
	}

	attrs := map[string]any{
		"contact":               t.cfg.Name,
		"media":                 t.media,
		"connected":             t.connected,
		"last_session_ended_by": t.endedBy,
		"composing_timeout":     int(t.cfg.ComposingTimeout.Seconds()),
		"off_delay":             int(t.cfg.OffDelay.Seconds()),
	}
	if !t.lastPlayed.IsZero() {
		attrs["last_played"] = t.lastPlayed.Format(time.RFC3339)
	}
	if t.lastMessageType != "" {
		attrs["last_message_type"] = t.lastMessageType
	}
	if t.typing {
		cur := time.Since(t.sessionStart)
		if cur < 0 {
			cur = 0
		}
		s.CurrentDuration = int(cur.Seconds())
		attrs["session_start"] = t.sessionStart.Format(time.RFC3339)
		if !t.pausedAt.IsZero() {
			attrs["in_grace_period"] = true
		}
	}
	s.Attributes = attrs
	return s
}

func (t *Tracker) publish() {
	if err := t.pub.PublishState(t.snapshot()); err != nil {
		t.log.Error("publish failed", "err", err)
	}
}
