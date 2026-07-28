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
	if s.PausesToday != 2 || s.RestartsToday != 1 {
		t.Fatalf("expected 2 pauses / 1 restart, got %d / %d", s.PausesToday, s.RestartsToday)
	}
	if s.LastSessionPauses != 2 || s.LastSessionRestarts != 1 {
		t.Fatalf("last session summary is wrong: %d pauses / %d restarts",
			s.LastSessionPauses, s.LastSessionRestarts)
	}
}

func TestPresenceOnlineOfflineAndLastSeen(t *testing.T) {
	tr, _ := newTestTracker()
	t0 := time.Now()

	tr.handle(Event{Kind: EvPresence, Online: true, At: t0})
	s := tr.snapshot()
	if !s.PresenceKnown || !s.Online {
		t.Fatal("online presence was not recorded")
	}

	lastSeen := t0.Add(time.Minute)
	tr.handle(Event{Kind: EvPresence, Online: false, LastSeen: lastSeen, At: lastSeen})
	s = tr.snapshot()
	if s.Online || !s.LastSeenAt.Equal(lastSeen) {
		t.Fatalf("offline/last seen was not recorded: online=%v last_seen=%v", s.Online, s.LastSeenAt)
	}
	before := len(s.Timeline)
	tr.handle(Event{Kind: EvPresence, Online: false, LastSeen: lastSeen, At: lastSeen})
	if after := len(tr.snapshot().Timeline); after != before {
		t.Fatalf("presence replay added another timeline entry: %d -> %d", before, after)
	}
}

func TestInteractionEventsReachStateAndTimeline(t *testing.T) {
	tr, _ := newTestTracker()
	t0 := time.Now()

	tr.handle(Event{Kind: EvReaction, At: t0, Emoji: "❤️",
		Target: "foto delle 18:42", Label: "ha reagito con ❤️ a foto delle 18:42"})
	tr.handle(Event{Kind: EvEdit, At: t0.Add(time.Second),
		Label: `ha modificato "prima" in "dopo"`})
	tr.handle(Event{Kind: EvDelete, At: t0.Add(2 * time.Second),
		Label: `ha eliminato "dopo"`})

	s := tr.snapshot()
	if s.LastReactionEmoji != "❤️" || s.LastReactionTarget != "foto delle 18:42" {
		t.Fatalf("reaction detail missing from state: %+v", s)
	}
	if s.LastEditAt.IsZero() || s.LastDeleteAt.IsZero() {
		t.Fatal("edit/delete timestamps missing from state")
	}
	if len(s.Timeline) != 3 || s.Timeline[0].Event != `ha eliminato "dopo"` {
		t.Fatalf("unexpected interaction timeline: %+v", s.Timeline)
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

func TestReceiptLabelsNameTheirTarget(t *testing.T) {
	cases := []struct {
		name string
		ev   Event
		want string
	}{
		{"lettura con soggetto",
			Event{Kind: EvRead, Target: "foto delle 17:02"},
			"ha letto (foto delle 17:02)"},
		{"lettura senza soggetto noto",
			Event{Kind: EvRead},
			ActRead},
		{"prima riproduzione",
			Event{Kind: EvPlayed, Media: "videomessaggio", Target: "videomessaggio delle 21:14", Repeat: 1},
			"ha ascoltato (videomessaggio delle 21:14)"},
		{"video riguardato",
			Event{Kind: EvPlayed, Media: "videomessaggio", Target: "videomessaggio delle 21:14", Repeat: 3},
			"ha riguardato videomessaggio delle 21:14 (3ª volta)"},
		{"vocale riascoltato",
			Event{Kind: EvPlayed, Media: "vocale", Target: "vocale delle 09:30", Repeat: 2},
			"ha riascoltato vocale delle 09:30 (2ª volta)"},
	}
	for _, tc := range cases {
		if got := receiptLabel(tc.ev); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestDescribeTargets(t *testing.T) {
	now := time.Date(2026, 7, 27, 18, 0, 0, 0, time.Local)
	msg := func(h, m int, kind string) sentMessage {
		return sentMessage{At: time.Date(2026, 7, 27, h, m, 0, 0, time.Local), Kind: kind}
	}

	if got := describeTargets([]sentMessage{msg(17, 2, "foto")}, 1, now); got != "foto delle 17:02" {
		t.Errorf("single known message: got %q", got)
	}
	if got := describeTargets(nil, 3, now); got != "3 messaggi" {
		t.Errorf("nothing known: got %q", got)
	}
	if got := describeTargets(nil, 1, now); got != "" {
		t.Errorf("one unknown message should add no detail: got %q", got)
	}
	got := describeTargets([]sentMessage{msg(17, 2, "foto"), msg(16, 0, "testo")}, 2, now)
	if got != "2 messaggi (l'ultimo: foto delle 17:02)" {
		t.Errorf("batch: got %q", got)
	}

	yesterday := sentMessage{At: now.AddDate(0, 0, -1), Kind: "vocale"}
	if got := describeTargets([]sentMessage{yesterday}, 1, now); got != "vocale di ieri alle 18:00" {
		t.Errorf("yesterday: got %q", got)
	}
	old := sentMessage{At: now.AddDate(0, 0, -5), Kind: "video"}
	if got := describeTargets([]sentMessage{old}, 1, now); got != "video del 22/07 alle 18:00" {
		t.Errorf("older: got %q", got)
	}
}

func TestReadTargetReachesTheState(t *testing.T) {
	tr, _ := newTestTracker()
	tr.handle(Event{Kind: EvRead, Target: "foto delle 17:02", At: time.Now()})

	s := tr.snapshot()
	if s.ReadTarget != "foto delle 17:02" {
		t.Fatalf("read target missing from the state, got %q", s.ReadTarget)
	}
	if s.Activity != "ha letto (foto delle 17:02)" {
		t.Fatalf("activity should name the target, got %q", s.Activity)
	}
}

func TestShortDropDoesNotMarkEntitiesUnavailable(t *testing.T) {
	tr, _ := newTestTracker()
	tr.cfg.AvailabilityGrace = 2 * time.Minute

	tr.SetConnected(true)
	tr.SetConnected(false)

	if !tr.snapshot().Available {
		t.Fatal("a blink of the connection must not blank out the entities")
	}
	// Still down once the grace is over: now it is a real outage.
	tr.mu.Lock()
	tr.disconnectedAt = time.Now().Add(-3 * time.Minute)
	tr.mu.Unlock()

	if tr.snapshot().Available {
		t.Fatal("after the grace window the entities must go unavailable")
	}
	if !tr.evaluate(time.Now()) {
		t.Fatal("the expiry of the grace window must trigger a publish")
	}
}

func TestReactionEmojiTellsRemovedFromUnknown(t *testing.T) {
	if got := reactionEmoji(State{}); got != unknown {
		t.Fatalf("no reaction ever seen should be unknown, got %q", got)
	}
	if got := reactionEmoji(State{ReactionSeen: true}); got != "nessuna" {
		t.Fatalf("a removed reaction should read as nessuna, got %q", got)
	}
	if got := reactionEmoji(State{ReactionSeen: true, LastReactionEmoji: "❤️"}); got != "❤️" {
		t.Fatalf("got %q", got)
	}
}

func TestEverySingleEventIsFireable(t *testing.T) {
	tr, _ := newTestTracker()
	t0 := time.Now()

	tr.handle(Event{Kind: EvRead, At: t0})
	first := tr.snapshot()
	// The very same thing happening twice must still be two events: a state
	// value would not change, the sequence has to.
	tr.handle(Event{Kind: EvRead, At: t0.Add(time.Minute)})
	second := tr.snapshot()

	if first.EventSeq == second.EventSeq {
		t.Fatal("two identical events must produce two different sequences")
	}
	if second.LastEventType != "letto" {
		t.Fatalf("expected the letto event type, got %q", second.LastEventType)
	}
	if second.LastEventText == "" {
		t.Fatal("the event must carry its readable label")
	}
}

func TestTypingEventsCarryTheirOwnType(t *testing.T) {
	tr, _ := newTestTracker()
	t0 := time.Now()

	tr.handle(Event{Kind: EvComposing, Media: "audio", At: t0})
	if got := tr.snapshot().LastEventType; got != "registra_vocale" {
		t.Fatalf("recording should have its own event type, got %q", got)
	}
	tr.handle(Event{Kind: EvComposing, Media: "text", At: t0.Add(time.Second)})
	tr.evaluate(t0.Add(time.Minute))
	if got := tr.snapshot().LastEventType; got != "ha_smesso" {
		t.Fatalf("the end of a session should fire ha_smesso, got %q", got)
	}
}

func TestOnlyTheBridgeEntityCanGoUnavailable(t *testing.T) {
	availability := 0
	for _, e := range entities() {
		if e.Availability {
			availability++
			if e.Key != "bridge" {
				t.Fatalf("%s must not carry an availability topic", e.Key)
			}
		}
	}
	if availability != 1 {
		t.Fatalf("exactly one entity should report availability, got %d", availability)
	}
	// With the link down every other entity keeps a real value.
	for _, e := range entities() {
		if v := e.Value(State{}); v == "unavailable" {
			t.Fatalf("%s reports unavailable as a state", e.Key)
		}
	}
}
