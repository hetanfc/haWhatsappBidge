package main

import (
	"context"
	"fmt"
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

// Activity labels, in Italian because they land straight in the Home Assistant
// history and logbook.
const (
	ActIdle      = "inattivo"
	ActTyping    = "sta scrivendo"
	ActRecording = "registra vocale"
	ActDelivered = "consegnato"
	ActRead      = "ha letto"
	ActPlayed    = "ha ascoltato"
)

const timelineSize = 50

// TimelineEntry is one line of the readable history.
type TimelineEntry struct {
	At    time.Time `json:"-"`
	Time  string    `json:"time"`  // HH:MM, for cards
	Stamp string    `json:"at"`    // RFC3339, for templates
	Event string    `json:"event"` // what happened, already in words
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

	// Activity is the single unified state: what is happening right now, or the
	// last thing that happened if it is still recent.
	Activity      string
	ActivitySince time.Time
	Timeline      []TimelineEntry

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

	// Activity: the label of the last instant event and when it happened, kept
	// only for as long as ActivitySticky. Typing always wins over these.
	lastEventLabel string
	lastEventAt    time.Time
	activity       string
	activitySince  time.Time
	timeline       []TimelineEntry
}

func NewTracker(cfg Config, pub Publisher, log *slog.Logger) *Tracker {
	return &Tracker{
		cfg:      cfg,
		pub:      pub,
		events:   make(chan Event, 64),
		log:      log,
		media:    "text",
		day:      time.Now().Format("2006-01-02"),
		activity: ActIdle,
	}
}

// note records an instant event: it drives the activity sensor for the sticky
// window and always lands in the timeline, even when typing keeps the state.
// Mutex must be held.
func (t *Tracker) note(at time.Time, label string) {
	t.lastEventLabel = label
	t.lastEventAt = at
	t.addTimeline(at, label)
}

// addTimeline prepends an entry, newest first; mutex must be held.
func (t *Tracker) addTimeline(at time.Time, event string) {
	entry := TimelineEntry{
		At:    at,
		Time:  at.Format("15:04"),
		Stamp: at.Format(time.RFC3339),
		Event: event,
	}
	t.timeline = append([]TimelineEntry{entry}, t.timeline...)
	if len(t.timeline) > timelineSize {
		t.timeline = t.timeline[:timelineSize]
	}
}

// activityAt is what the unified sensor should read at that instant; mutex must
// be held. Typing wins over everything: a receipt arriving mid-session goes to
// the timeline but must not blank out "sta scrivendo".
func (t *Tracker) activityAt(now time.Time) string {
	if !t.connected {
		return ActIdle
	}
	if t.typing {
		if t.media == "audio" {
			return ActRecording
		}
		return ActTyping
	}
	if !t.lastEventAt.IsZero() && now.Sub(t.lastEventAt) < t.cfg.ActivitySticky {
		return t.lastEventLabel
	}
	return ActIdle
}

// refreshActivity keeps the published label in sync; reports whether it moved.
// Mutex must be held.
func (t *Tracker) refreshActivity(now time.Time) bool {
	label := t.activityAt(now)
	if label == t.activity {
		return false
	}
	t.activity = label
	t.activitySince = now
	return true
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
			label := ActTyping
			if t.media == "audio" {
				label = ActRecording
			}
			t.addTimeline(ev.At, label)
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
		label := "messaggio ricevuto"
		if ev.Media != "" {
			label += " (" + ev.Media + ")"
		}
		t.note(ev.At, label)

	// Receipts can arrive out of order or be replayed after a reconnect, so
	// timestamps only ever move forward.
	case EvDelivered:
		if ev.At.After(t.lastDelivered) {
			t.lastDelivered = ev.At
			t.note(ev.At, ActDelivered)
		}

	case EvRead:
		if ev.At.After(t.lastRead) {
			t.lastRead = ev.At
			t.readsToday++
			t.note(ev.At, ActRead)
			t.log.Info("messages read", "contact", t.cfg.Name, "at", ev.At.Format(time.RFC3339))
		}

	case EvPlayed:
		if ev.At.After(t.lastPlayed) {
			t.lastPlayed = ev.At
			t.note(ev.At, ActPlayed)
			t.log.Info("voice note played", "contact", t.cfg.Name, "at", ev.At.Format(time.RFC3339))
		}
	}

	t.refreshActivity(time.Now())
}

// evaluate applies the timeouts; it reports whether the state changed.
func (t *Tracker) evaluate(now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rollover(now)

	if t.typing {
		switch {
		case !t.pausedAt.IsZero() && now.Sub(t.pausedAt) >= t.cfg.OffDelay:
			t.endSession(t.pausedAt, "paused")
		case now.Sub(t.lastComposing) >= t.cfg.ComposingTimeout:
			t.endSession(t.lastComposing, "timeout")
		}
	}
	// Always re-evaluated, so the activity sensor falls back to idle when the
	// sticky window of an instant event expires.
	return t.refreshActivity(now)
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
	if reason != "message" {
		// On "message" the incoming message itself is the timeline entry, and
		// two lines one second apart would just be noise.
		t.addTimeline(end, fmt.Sprintf("ha smesso di scrivere (%ds)", int(d.Seconds())))
	}
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
		Activity:        t.activityAt(time.Now()),
		ActivitySince:   t.activitySince,
		Timeline:        append([]TimelineEntry(nil), t.timeline...),
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
