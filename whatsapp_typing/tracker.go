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
	EvReaction                   // she added, changed or removed a reaction
	EvEdit                       // she edited a message
	EvDelete                     // she deleted a message for everyone
	EvPresence                   // online/offline and optional last seen
)

type Event struct {
	Kind     EventKind
	Media    string // typing: "text" or "audio". Messages and receipts: the message type
	At       time.Time
	Target   string // receipts: what the receipt refers to, e.g. "foto delle 17:02"
	Repeat   int    // receipts: how many times this message got this receipt
	Label    string // fully formatted label for messages/reactions/edits/deletes
	Emoji    string
	Online   bool
	LastSeen time.Time
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
	Available           bool
	MarkOnline          bool
	Typing              bool
	Status              string // idle | typing | recording | disconnected
	CurrentDuration     int    // seconds in the running session, 0 when idle
	LastDuration        int    // seconds of the last completed session
	LastTypingAt        time.Time
	SessionsToday       int
	SecondsToday        int
	PausesToday         int
	RestartsToday       int
	LastSessionPauses   int
	LastSessionRestarts int

	// Read receipts. Unlike typing, these only move when we send her something:
	// no outgoing messages means no signal at all, not "she wasn't around".
	LastDeliveredAt time.Time
	LastReadAt      time.Time
	LastPlayedAt    time.Time
	ReadsToday      int
	ReadTarget      string // which message the last read receipt was about
	PlayedTarget    string

	// Incoming messages and interaction timestamps.
	LastMessageAt time.Time
	MessagesToday int

	PresenceKnown  bool
	Online         bool
	LastSeenAt     time.Time
	LastPresenceAt time.Time

	LastReactionAt     time.Time
	LastReactionEmoji  string
	LastReactionTarget string
	ReactionSeen       bool
	LastEditAt         time.Time
	LastDeleteAt       time.Time

	// LastEvent is the newest thing that happened, kept verbatim for
	// notifications. EventSeq changes on every single event, including two
	// identical ones in a row, which a state value alone cannot express.
	LastEventType string
	LastEventText string
	LastEventAt   time.Time
	EventSeq      uint64

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

	store *stateStore
	dirty bool // durable fields changed since the last save

	mu              sync.Mutex
	connected       bool
	disconnectedAt  time.Time // when the link dropped, for the availability grace
	lastAvailable   bool      // last published availability, to notice the edge
	typing          bool
	media           string
	sessionStart    time.Time
	lastComposing   time.Time
	pausedAt        time.Time
	lastDuration    time.Duration
	lastTypingAt    time.Time
	endedBy         string
	sessions        int
	secondsToday    time.Duration
	currentPauses   int
	currentRestarts int
	lastPauses      int
	lastRestarts    int
	pausesToday     int
	restartsToday   int
	day             string

	lastDelivered time.Time
	lastRead      time.Time
	lastPlayed    time.Time
	readsToday    int
	readTarget    string // what the last read receipt referred to
	playedTarget  string

	lastMessage     time.Time
	lastMessageType string
	messagesToday   int

	presenceKnown bool
	online        bool
	lastSeen      time.Time
	lastPresence  time.Time

	lastReaction       time.Time
	lastReactionEmoji  string
	lastReactionTarget string
	reactionSeen       bool // a reaction was observed: an empty emoji means "removed", not "unknown"
	lastEdit           time.Time
	lastDelete         time.Time

	// Every timeline entry is also an event: pendingType carries the kind of the
	// event being handled so the emission point does not need it passed down.
	pendingType   string
	lastEventType string
	lastEventText string
	lastEventTime time.Time
	eventSeq      uint64

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
		cfg:    cfg,
		pub:    pub,
		events: make(chan Event, 256),
		log:    log,
		media:  "text",
		day:    time.Now().Format("2006-01-02"),
		// Not connected yet, but not "unavailable" either: the grace window
		// covers the seconds it takes to link up after a restart.
		disconnectedAt: time.Now(),
		lastAvailable:  true,
		activity:       ActIdle,
	}
}

// receiptLabel writes a receipt out in full: what happened, to which message,
// and whether it had already happened to that same message before.
func receiptLabel(ev Event) string {
	base := ActRead
	switch ev.Kind {
	case EvDelivered:
		base = ActDelivered
	case EvPlayed:
		base = ActPlayed
		if ev.Repeat > 1 {
			// Opened again: worth its own wording, it is not a first view.
			verb := "ha riguardato"
			if ev.Media == "vocale" || ev.Media == "audio" {
				verb = "ha riascoltato"
			}
			if ev.Target == "" {
				return fmt.Sprintf("%s (%dª volta)", verb, ev.Repeat)
			}
			return fmt.Sprintf("%s %s (%dª volta)", verb, ev.Target, ev.Repeat)
		}
	}
	if ev.Target == "" {
		return base
	}
	return base + " (" + ev.Target + ")"
}

// eventSlug is the stable identifier Home Assistant automations match on. It
// must not be translated: the labels are for humans, these are for triggers.
func eventSlug(kind EventKind, media string) string {
	switch kind {
	case EvComposing:
		if media == "audio" {
			return "registra_vocale"
		}
		return "sta_scrivendo"
	case EvMessage:
		return "messaggio"
	case EvDelivered:
		return "consegnato"
	case EvRead:
		return "letto"
	case EvPlayed:
		return "riprodotto"
	case EvReaction:
		return "reazione"
	case EvEdit:
		return "modifica"
	case EvDelete:
		return "eliminazione"
	case EvPresence:
		return "presenza"
	default:
		return "altro"
	}
}

// eventTypes lists everything the event entity can fire, for its discovery
// payload. Home Assistant rejects an event_type that was not declared.
func eventTypes() []string {
	return []string{
		"sta_scrivendo", "registra_vocale", "ha_smesso", "messaggio", "consegnato",
		"letto", "riprodotto", "reazione", "modifica", "eliminazione", "presenza", "altro",
	}
}

// note records an instant event: it drives the activity sensor for the sticky
// window and always lands in the timeline, even when typing keeps the state.
// Mutex must be held.
func (t *Tracker) note(at time.Time, label string) {
	t.lastEventLabel = stateSafeLabel(label)
	t.lastEventAt = at
	t.addTimeline(at, label)
}

func stateSafeLabel(s string) string {
	const maxBytes = 240
	if len(s) <= maxBytes {
		return s
	}
	runes := []rune(s)
	for len(runes) > 0 && len(string(runes))+len("…") > maxBytes {
		runes = runes[:len(runes)-1]
	}
	return string(runes) + "…"
}

// addTimeline prepends an entry, newest first, and doubles as the single point
// where an event is emitted: every timeline line is something that happened.
// Mutex must be held.
func (t *Tracker) addTimeline(at time.Time, event string) {
	t.lastEventType = t.pendingType
	if t.lastEventType == "" {
		t.lastEventType = "altro"
	}
	t.lastEventText = event
	t.lastEventTime = at
	t.eventSeq++

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

// available reports whether the entities should still count as alive. The
// WhatsApp socket drops and comes back on its own several times a day, and
// blanking every entity to "unavailable" on each blip buries the history in
// noise, so a gap shorter than AvailabilityGrace is ridden out. Mutex must be
// held.
func (t *Tracker) available(now time.Time) bool {
	if t.connected {
		return true
	}
	if t.disconnectedAt.IsZero() {
		return false
	}
	return now.Sub(t.disconnectedAt) < t.cfg.AvailabilityGrace
}

// activityAt is what the unified sensor should read at that instant; mutex must
// be held. Typing wins over everything: a receipt arriving mid-session goes to
// the timeline but must not blank out "sta scrivendo".
func (t *Tracker) activityAt(now time.Time) string {
	if !t.available(now) {
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

// attachStore wires persistence and restores the last known values. It must be
// called before Run, so the first publish already carries them instead of a
// screenful of "unknown".
func (t *Tracker) attachStore(ctx context.Context, store *stateStore) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.store = store
	st, ok := store.load(ctx)
	if !ok {
		return
	}

	// Counters belong to the day they were counted in: coming back up after
	// midnight must not resurrect yesterday's totals.
	if st.Day == time.Now().Format("2006-01-02") {
		t.day = st.Day
		t.sessions = st.SessionsToday
		t.secondsToday = time.Duration(st.SecondsToday) * time.Second
		t.pausesToday = st.PausesToday
		t.restartsToday = st.RestartsToday
		t.readsToday = st.ReadsToday
		t.messagesToday = st.MessagesToday
	}

	t.lastTypingAt = st.LastTypingAt
	t.lastDuration = time.Duration(st.LastDuration) * time.Second
	t.lastDelivered = st.LastDelivered
	t.lastRead = st.LastRead
	t.lastPlayed = st.LastPlayed
	t.readTarget = st.ReadTarget
	t.playedTarget = st.PlayedTarget
	t.lastMessage = st.LastMessage
	t.lastMessageType = st.LastMessageType
	t.presenceKnown = st.PresenceKnown
	t.online = st.Online
	t.lastSeen = st.LastSeen
	t.lastPresence = st.LastPresence
	t.lastReaction = st.LastReaction
	t.lastReactionEmoji = st.LastReactionEmoji
	t.lastReactionTarget = st.LastReactionTarget
	t.reactionSeen = st.ReactionSeen
	t.lastEdit = st.LastEdit
	t.lastDelete = st.LastDelete
	t.lastEventType = st.LastEventType
	t.lastEventText = st.LastEventText
	t.lastEventTime = st.LastEventTime
	t.timeline = st.Timeline

	t.log.Info("restored last known state", "last_reaction", st.LastReactionEmoji,
		"timeline", len(st.Timeline))
}

// durable builds the snapshot to persist; mutex must be held.
func (t *Tracker) durable() durableState {
	return durableState{
		Day:                t.day,
		LastTypingAt:       t.lastTypingAt,
		LastDuration:       int(t.lastDuration.Seconds()),
		SessionsToday:      t.sessions,
		SecondsToday:       int(t.secondsToday.Seconds()),
		PausesToday:        t.pausesToday,
		RestartsToday:      t.restartsToday,
		LastDelivered:      t.lastDelivered,
		LastRead:           t.lastRead,
		LastPlayed:         t.lastPlayed,
		ReadsToday:         t.readsToday,
		ReadTarget:         t.readTarget,
		PlayedTarget:       t.playedTarget,
		LastMessage:        t.lastMessage,
		LastMessageType:    t.lastMessageType,
		MessagesToday:      t.messagesToday,
		PresenceKnown:      t.presenceKnown,
		Online:             t.online,
		LastSeen:           t.lastSeen,
		LastPresence:       t.lastPresence,
		LastReaction:       t.lastReaction,
		LastReactionEmoji:  t.lastReactionEmoji,
		LastReactionTarget: t.lastReactionTarget,
		ReactionSeen:       t.reactionSeen,
		LastEdit:           t.lastEdit,
		LastDelete:         t.lastDelete,
		LastEventType:      t.lastEventType,
		LastEventText:      t.lastEventText,
		LastEventTime:      t.lastEventTime,
		Timeline:           t.timeline,
	}
}

// persist writes the durable state if anything changed since the last write.
func (t *Tracker) persist(ctx context.Context) {
	t.mu.Lock()
	if t.store == nil || !t.dirty {
		t.mu.Unlock()
		return
	}
	t.dirty = false
	st := t.durable()
	t.mu.Unlock()

	t.store.save(ctx, st)
}

func (t *Tracker) SetConnected(v bool) {
	t.mu.Lock()
	if t.connected == v {
		t.mu.Unlock()
		return
	}
	t.connected = v
	if v {
		t.disconnectedAt = time.Time{}
	} else {
		t.disconnectedAt = time.Now()
	}
	// Keep the edge detector in step, otherwise the tick loop can miss the
	// moment the grace window runs out.
	t.lastAvailable = t.available(time.Now())
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
	// Batched on purpose: events can burst, and the point is surviving a
	// restart, not writing to disk on every single one.
	saveTick := time.NewTicker(10 * time.Second)
	defer saveTick.Stop()

	t.publish()
	lastPublish := time.Now()

	for {
		select {
		case <-ctx.Done():
			// Last write on the way out, so a clean stop keeps what it knows.
			t.persist(context.WithoutCancel(ctx))
			return

		case ev := <-t.events:
			t.handle(ev)
			t.publish()
			lastPublish = time.Now()

		case <-saveTick.C:
			t.persist(ctx)

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
	t.pendingType = eventSlug(ev.Kind, ev.Media)

	switch ev.Kind {
	case EvComposing:
		if ev.Media != "" {
			t.media = ev.Media
		}
		resumed := t.typing && !t.pausedAt.IsZero()
		if resumed {
			t.currentRestarts++
			t.restartsToday++
		}
		t.pausedAt = time.Time{}
		t.lastComposing = ev.At
		if !t.typing {
			t.typing = true
			t.sessionStart = ev.At
			t.currentPauses = 0
			t.currentRestarts = 0
			t.sessions++
			t.endedBy = ""
			label := ActTyping
			if t.media == "audio" {
				label = ActRecording
			}
			t.pendingType = eventSlug(EvComposing, t.media)
			t.addTimeline(ev.At, label)
			t.log.Info("typing started", "contact", t.cfg.Name, "media", t.media)
		}

	case EvPaused:
		if !t.typing || !t.pausedAt.IsZero() {
			return
		}
		t.pausedAt = ev.At
		t.currentPauses++
		t.pausesToday++
		if t.cfg.OffDelay <= 0 {
			t.endSession(ev.At, "paused")
		}

	case EvMessage:
		hadPauses := t.typing && (t.currentPauses > 0 || t.currentRestarts > 0)
		if t.typing {
			t.endSession(ev.At, "message")
		}
		t.lastMessage = ev.At
		t.messagesToday++
		if ev.Media != "" {
			t.lastMessageType = ev.Media
		}
		label := ev.Label
		if label == "" {
			label = "messaggio ricevuto"
			if ev.Media != "" {
				label += " (" + ev.Media + ")"
			}
		}
		if hadPauses {
			label += fmt.Sprintf(" [sessione: %ds, %s, %s]",
				int(t.lastDuration.Seconds()), countWord(t.lastPauses, "pausa", "pause"),
				countWord(t.lastRestarts, "ripresa", "riprese"))
		}
		t.note(ev.At, label)

	// Receipts can arrive out of order or be replayed after a reconnect, so
	// timestamps only ever move forward.
	case EvDelivered:
		if ev.At.After(t.lastDelivered) {
			t.lastDelivered = ev.At
			t.note(ev.At, receiptLabel(ev))
		}

	case EvRead:
		if ev.At.After(t.lastRead) {
			t.lastRead = ev.At
			t.readsToday++
			t.readTarget = ev.Target
			t.note(ev.At, receiptLabel(ev))
			t.log.Info("messages read", "contact", t.cfg.Name, "target", ev.Target,
				"at", ev.At.Format(time.RFC3339))
		}

	// A replay carries an older timestamp, but a genuine second view of the same
	// clip arrives with a fresh one, so this still lets repeats through.
	case EvPlayed:
		if ev.At.After(t.lastPlayed) {
			t.lastPlayed = ev.At
			t.playedTarget = ev.Target
			t.note(ev.At, receiptLabel(ev))
			t.log.Info("media played", "contact", t.cfg.Name, "target", ev.Target,
				"repeat", ev.Repeat, "at", ev.At.Format(time.RFC3339))
		}

	case EvReaction:
		// Replays after a reconnect carry their original timestamp, so this is
		// the whole guard: comparing against the last event of any kind, as it
		// used to, let an old reaction back in as soon as anything else had
		// happened in between.
		if ev.At.After(t.lastReaction) {
			t.lastReaction = ev.At
			t.lastReactionEmoji = ev.Emoji
			t.lastReactionTarget = ev.Target
			t.reactionSeen = true
			t.note(ev.At, ev.Label)
		}

	case EvEdit:
		if ev.At.After(t.lastEdit) {
			t.lastEdit = ev.At
			t.note(ev.At, ev.Label)
		}

	case EvDelete:
		if ev.At.After(t.lastDelete) {
			t.lastDelete = ev.At
			t.note(ev.At, ev.Label)
		}

	case EvPresence:
		stateChanged := !t.presenceKnown || t.online != ev.Online
		lastSeenChanged := !ev.LastSeen.IsZero() && ev.LastSeen.After(t.lastSeen)
		if !stateChanged && !lastSeenChanged {
			break
		}
		t.presenceKnown = true
		t.online = ev.Online
		t.lastPresence = ev.At
		if lastSeenChanged {
			t.lastSeen = ev.LastSeen
		}
		label := "online"
		if !ev.Online {
			label = "offline"
			if !ev.LastSeen.IsZero() {
				label += " — ultimo accesso " + ev.LastSeen.Format("15:04")
			}
		}
		t.note(ev.At, label)
	}

	t.dirty = true
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
	changed := t.refreshActivity(now)
	if avail := t.available(now); avail != t.lastAvailable {
		t.lastAvailable = avail
		changed = true
	}
	return changed
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
	t.lastPauses = t.currentPauses
	t.lastRestarts = t.currentRestarts
	t.lastTypingAt = end
	t.secondsToday += d
	t.endedBy = reason
	if reason != "message" {
		// On "message" the incoming message itself is the timeline entry, and
		// two lines one second apart would just be noise.
		t.pendingType = "ha_smesso"
		label := fmt.Sprintf("ha smesso di scrivere (%ds", int(d.Seconds()))
		if t.lastPauses > 0 || t.lastRestarts > 0 {
			label += ", " + countWord(t.lastPauses, "pausa", "pause") +
				", " + countWord(t.lastRestarts, "ripresa", "riprese")
		}
		t.addTimeline(end, label+")")
	}
	t.dirty = true
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
	t.pausesToday = 0
	t.restartsToday = 0
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
		Available:           t.available(time.Now()),
		MarkOnline:          t.cfg.MarkOnline,
		Typing:              t.typing,
		LastDuration:        int(t.lastDuration.Seconds()),
		LastTypingAt:        t.lastTypingAt,
		SessionsToday:       t.sessions,
		SecondsToday:        int(t.secondsToday.Seconds()),
		PausesToday:         t.pausesToday,
		RestartsToday:       t.restartsToday,
		LastSessionPauses:   t.lastPauses,
		LastSessionRestarts: t.lastRestarts,
		LastDeliveredAt:     t.lastDelivered,
		LastReadAt:          t.lastRead,
		LastPlayedAt:        t.lastPlayed,
		ReadsToday:          t.readsToday,
		ReadTarget:          t.readTarget,
		PlayedTarget:        t.playedTarget,
		LastMessageAt:       t.lastMessage,
		MessagesToday:       t.messagesToday,
		PresenceKnown:       t.presenceKnown,
		Online:              t.online,
		LastSeenAt:          t.lastSeen,
		LastPresenceAt:      t.lastPresence,
		LastReactionAt:      t.lastReaction,
		LastReactionEmoji:   t.lastReactionEmoji,
		ReactionSeen:        t.reactionSeen,
		LastReactionTarget:  t.lastReactionTarget,
		LastEditAt:          t.lastEdit,
		LastDeleteAt:        t.lastDelete,
		LastEventType:       t.lastEventType,
		LastEventText:       t.lastEventText,
		LastEventAt:         t.lastEventTime,
		EventSeq:            t.eventSeq,
		Activity:            t.activityAt(time.Now()),
		ActivitySince:       t.activitySince,
		Timeline:            append([]TimelineEntry(nil), t.timeline...),
	}

	switch {
	case !t.available(time.Now()):
		s.Status = "disconnected"
	case t.typing && t.media == "audio":
		s.Status = "recording"
	case t.typing:
		s.Status = "typing"
	default:
		s.Status = "idle"
	}

	attrs := map[string]any{
		"contact":                  t.cfg.Name,
		"media":                    t.media,
		"connected":                t.connected,
		"last_session_ended_by":    t.endedBy,
		"composing_timeout":        int(t.cfg.ComposingTimeout.Seconds()),
		"off_delay":                int(t.cfg.OffDelay.Seconds()),
		"current_session_pauses":   t.currentPauses,
		"current_session_restarts": t.currentRestarts,
		"last_session_pauses":      t.lastPauses,
		"last_session_restarts":    t.lastRestarts,
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

func countWord(n int, singular, plural string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, singular)
	}
	return fmt.Sprintf("%d %s", n, plural)
}

func (t *Tracker) publish() {
	if err := t.pub.PublishState(t.snapshot()); err != nil {
		t.log.Error("publish failed", "err", err)
	}
}
