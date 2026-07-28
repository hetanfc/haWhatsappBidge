package main

import (
	"context"
	"database/sql"
	"log/slog"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func newTestStore(t *testing.T) *stateStore {
	t.Helper()
	db, err := sql.Open("sqlite", "file:state?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })

	store, err := newStateStore(context.Background(), db, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestRestartKeepsTheLastKnownValuesAndRejectsReplays(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	t0 := time.Now().Add(-time.Hour)

	// First run: she reacts, then the add-on is stopped.
	first, _ := newTestTracker()
	first.attachStore(ctx, store)
	first.handle(Event{Kind: EvReaction, At: t0, Emoji: "😂", Target: "foto delle 17:02",
		Label: "ha reagito con 😂 a foto delle 17:02"})
	first.persist(ctx)

	// Second run: the saved state comes back instead of a blank tracker.
	second, pub := newTestTracker()
	second.attachStore(ctx, store)
	second.publish()

	if pub.last.LastReactionEmoji != "😂" {
		t.Fatalf("the reaction should survive the restart, got %q", pub.last.LastReactionEmoji)
	}
	if !pub.last.ReactionSeen {
		t.Fatal("the restored state must remember that a reaction was seen")
	}

	// WhatsApp replays the same reaction on reconnect: it must be ignored, and
	// it must stay ignored even after some unrelated event.
	before := len(second.snapshot().Timeline)
	second.handle(Event{Kind: EvRead, At: time.Now()})
	second.handle(Event{Kind: EvReaction, At: t0, Emoji: "😂", Target: "foto delle 17:02",
		Label: "ha reagito con 😂 a foto delle 17:02"})

	tl := second.snapshot().Timeline
	seen := 0
	for _, e := range tl {
		if e.Event == "ha reagito con 😂 a foto delle 17:02" {
			seen++
		}
	}
	if seen != 1 {
		t.Fatalf("the reaction should appear once, from the first run, got %d", seen)
	}
	if len(tl) != before+1 {
		t.Fatalf("only the read receipt should have been added, got %d new entries", len(tl)-before)
	}
}

func TestCountersOnlyComeBackOnTheSameDay(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	store.save(ctx, durableState{Day: "1999-01-01", ReadsToday: 7, MessagesToday: 3})
	tr, _ := newTestTracker()
	tr.attachStore(ctx, store)

	if s := tr.snapshot(); s.ReadsToday != 0 || s.MessagesToday != 0 {
		t.Fatalf("yesterday's counters must not be restored, got %d reads / %d messages",
			s.ReadsToday, s.MessagesToday)
	}
}
