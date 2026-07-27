package main

import (
	"log/slog"
	"testing"
	"time"
)

type fakePublisher struct{ last State }

func (f *fakePublisher) PublishState(s State) error { f.last = s; return nil }
func (f *fakePublisher) Close()                     {}

func newTestTracker() (*Tracker, *fakePublisher) {
	cfg := Config{
		Name:             "Contatto",
		ComposingTimeout: 20 * time.Second,
		OffDelay:         3 * time.Second,
		Tick:             2 * time.Second,
		ActivitySticky:   30 * time.Second,
	}
	pub := &fakePublisher{}
	tr := NewTracker(cfg, pub, slog.New(slog.DiscardHandler))
	tr.connected = true
	return tr, pub
}

func TestPausedEndsSessionAfterGrace(t *testing.T) {
	tr, _ := newTestTracker()
	t0 := time.Now()

	tr.handle(Event{Kind: EvComposing, Media: "text", At: t0})
	if !tr.isTyping() {
		t.Fatal("composing should turn the sensor on")
	}
	tr.handle(Event{Kind: EvComposing, At: t0.Add(10 * time.Second)})
	tr.handle(Event{Kind: EvPaused, At: t0.Add(12 * time.Second)})

	// Still on during the grace window.
	if tr.evaluate(t0.Add(14 * time.Second)); !tr.isTyping() {
		t.Fatal("sensor should stay on inside the grace window")
	}
	// Grace expired.
	if !tr.evaluate(t0.Add(15*time.Second)) || tr.isTyping() {
		t.Fatal("sensor should turn off once the grace window expires")
	}

	s := tr.snapshot()
	if s.LastDuration != 12 {
		t.Fatalf("duration should stop at the pause (12s), got %ds", s.LastDuration)
	}
	if s.Attributes["last_session_ended_by"] != "paused" {
		t.Fatalf("unexpected end reason: %v", s.Attributes["last_session_ended_by"])
	}
}

func TestShortPauseKeepsOneSession(t *testing.T) {
	tr, _ := newTestTracker()
	t0 := time.Now()

	tr.handle(Event{Kind: EvComposing, At: t0})
	tr.handle(Event{Kind: EvPaused, At: t0.Add(4 * time.Second)})
	tr.evaluate(t0.Add(5 * time.Second)) // inside the grace window
	tr.handle(Event{Kind: EvComposing, At: t0.Add(6 * time.Second)})
	tr.handle(Event{Kind: EvPaused, At: t0.Add(20 * time.Second)})
	tr.evaluate(t0.Add(25 * time.Second))

	s := tr.snapshot()
	if s.SessionsToday != 1 {
		t.Fatalf("a pause shorter than off_delay must not split the session, got %d sessions", s.SessionsToday)
	}
	if s.LastDuration != 20 {
		t.Fatalf("expected a single 20s session, got %ds", s.LastDuration)
	}
}

func TestComposingTimeoutClosesSession(t *testing.T) {
	tr, _ := newTestTracker()
	t0 := time.Now()

	tr.handle(Event{Kind: EvComposing, At: t0})
	// No paused, no refresh: the app was killed or the network dropped.
	if tr.evaluate(t0.Add(19 * time.Second)); !tr.isTyping() {
		t.Fatal("sensor should stay on until the composing timeout")
	}
	if !tr.evaluate(t0.Add(20*time.Second)) || tr.isTyping() {
		t.Fatal("sensor should turn off at the composing timeout")
	}

	s := tr.snapshot()
	if s.LastDuration != 1 {
		t.Fatalf("duration must stop at the last evidence of typing, got %ds", s.LastDuration)
	}
	if s.Attributes["last_session_ended_by"] != "timeout" {
		t.Fatalf("unexpected end reason: %v", s.Attributes["last_session_ended_by"])
	}
}

func TestIncomingMessageEndsSession(t *testing.T) {
	tr, _ := newTestTracker()
	t0 := time.Now()

	tr.handle(Event{Kind: EvComposing, At: t0})
	tr.handle(Event{Kind: EvMessage, At: t0.Add(8 * time.Second)})

	if tr.isTyping() {
		t.Fatal("an incoming message must close the typing session")
	}
	s := tr.snapshot()
	if s.LastDuration != 8 {
		t.Fatalf("expected 8s, got %ds", s.LastDuration)
	}
	if s.Status != "idle" {
		t.Fatalf("expected idle, got %q", s.Status)
	}
}

func TestAudioRecordingStatus(t *testing.T) {
	tr, _ := newTestTracker()
	tr.handle(Event{Kind: EvComposing, Media: "audio", At: time.Now()})
	if s := tr.snapshot(); s.Status != "recording" {
		t.Fatalf("expected recording, got %q", s.Status)
	}
}

func TestDailyTotals(t *testing.T) {
	tr, _ := newTestTracker()
	t0 := time.Now()

	for i := range 3 {
		start := t0.Add(time.Duration(i) * time.Minute)
		tr.handle(Event{Kind: EvComposing, At: start})
		tr.handle(Event{Kind: EvMessage, At: start.Add(5 * time.Second)})
	}
	s := tr.snapshot()
	if s.SessionsToday != 3 || s.SecondsToday != 15 {
		t.Fatalf("expected 3 sessions / 15s, got %d / %ds", s.SessionsToday, s.SecondsToday)
	}
}

func TestDisconnectClosesSession(t *testing.T) {
	tr, pub := newTestTracker()
	tr.handle(Event{Kind: EvComposing, At: time.Now()})
	tr.SetConnected(false)

	if tr.isTyping() {
		t.Fatal("losing the whatsapp connection must clear the typing state")
	}
	if pub.last.Available {
		t.Fatal("entities must be marked unavailable while disconnected")
	}
	if pub.last.Status != "disconnected" {
		t.Fatalf("expected disconnected status, got %q", pub.last.Status)
	}
}

func TestReceiptsTrackReadsAndIgnoreReplays(t *testing.T) {
	tr, _ := newTestTracker()
	t0 := time.Now()

	tr.handle(Event{Kind: EvDelivered, At: t0})
	tr.handle(Event{Kind: EvRead, At: t0.Add(30 * time.Second)})
	tr.handle(Event{Kind: EvPlayed, At: t0.Add(40 * time.Second)})
	// A reconnect replays older receipts: they must not move anything back.
	tr.handle(Event{Kind: EvRead, At: t0.Add(10 * time.Second)})

	s := tr.snapshot()
	if !s.LastReadAt.Equal(t0.Add(30 * time.Second)) {
		t.Fatalf("last read should stay at the newest receipt, got %v", s.LastReadAt)
	}
	if s.ReadsToday != 1 {
		t.Fatalf("a replayed receipt must not count as a new read, got %d", s.ReadsToday)
	}
	if !s.LastDeliveredAt.Equal(t0) || !s.LastPlayedAt.Equal(t0.Add(40*time.Second)) {
		t.Fatal("delivered and played timestamps were not recorded")
	}

	tr.handle(Event{Kind: EvRead, At: t0.Add(2 * time.Minute)})
	if tr.snapshot().ReadsToday != 2 {
		t.Fatal("a newer read receipt must increment the daily counter")
	}
}

func TestReceiptsDoNotTouchTypingState(t *testing.T) {
	tr, _ := newTestTracker()
	t0 := time.Now()

	tr.handle(Event{Kind: EvComposing, At: t0})
	tr.handle(Event{Kind: EvRead, At: t0.Add(time.Second)})

	if !tr.isTyping() {
		t.Fatal("a read receipt must not end a typing session")
	}
	if tr.snapshot().Status != "typing" {
		t.Fatal("status should still report typing")
	}
}

func TestIncomingMessagesAreCounted(t *testing.T) {
	tr, _ := newTestTracker()
	t0 := time.Now()

	tr.handle(Event{Kind: EvMessage, Media: "testo", At: t0})
	tr.handle(Event{Kind: EvMessage, Media: "vocale", At: t0.Add(time.Minute)})

	s := tr.snapshot()
	if s.MessagesToday != 2 {
		t.Fatalf("expected 2 messages today, got %d", s.MessagesToday)
	}
	if !s.LastMessageAt.Equal(t0.Add(time.Minute)) {
		t.Fatalf("last message timestamp not updated, got %v", s.LastMessageAt)
	}
	if s.Attributes["last_message_type"] != "vocale" {
		t.Fatalf("expected the last type to be vocale, got %v", s.Attributes["last_message_type"])
	}
}

func TestActivityTypingWinsOverInstantEvents(t *testing.T) {
	tr, _ := newTestTracker()
	t0 := time.Now()

	tr.handle(Event{Kind: EvComposing, At: t0})
	tr.handle(Event{Kind: EvDelivered, At: t0.Add(2 * time.Second)})

	if got := tr.snapshot().Activity; got != ActTyping {
		t.Fatalf("typing must win over a receipt, got %q", got)
	}
	// The receipt still has to be in the readable history.
	found := false
	for _, e := range tr.snapshot().Timeline {
		if e.Event == ActDelivered {
			found = true
		}
	}
	if !found {
		t.Fatal("the receipt should appear in the timeline even while typing")
	}
}

func TestActivityFallsBackToIdleAfterSticky(t *testing.T) {
	tr, _ := newTestTracker()
	t0 := time.Now()

	tr.handle(Event{Kind: EvRead, At: t0})
	if got := tr.activityAt(t0.Add(10 * time.Second)); got != ActRead {
		t.Fatalf("inside the sticky window the state should hold, got %q", got)
	}
	if got := tr.activityAt(t0.Add(31 * time.Second)); got != ActIdle {
		t.Fatalf("after the sticky window it should be idle, got %q", got)
	}
}

func TestTimelineIsNewestFirstAndCapped(t *testing.T) {
	tr, _ := newTestTracker()
	t0 := time.Now()

	for i := range timelineSize + 10 {
		tr.handle(Event{Kind: EvRead, At: t0.Add(time.Duration(i) * time.Minute)})
	}
	tl := tr.snapshot().Timeline
	if len(tl) != timelineSize {
		t.Fatalf("timeline should be capped at %d, got %d", timelineSize, len(tl))
	}
	if !tl[0].At.After(tl[1].At) {
		t.Fatal("timeline must be newest first")
	}
}

func TestTypingSessionShowsUpInTimeline(t *testing.T) {
	tr, _ := newTestTracker()
	t0 := time.Now()

	tr.handle(Event{Kind: EvComposing, At: t0})
	tr.handle(Event{Kind: EvMessage, Media: "foto", At: t0.Add(12 * time.Second)})

	tl := tr.snapshot().Timeline
	if len(tl) != 2 {
		t.Fatalf("expected the typing start and the message, got %d entries: %+v", len(tl), tl)
	}
	if tl[0].Event != "messaggio ricevuto (foto)" {
		t.Fatalf("newest entry should be the message, got %q", tl[0].Event)
	}
	if tl[1].Event != ActTyping {
		t.Fatalf("oldest entry should be the typing start, got %q", tl[1].Event)
	}
}
