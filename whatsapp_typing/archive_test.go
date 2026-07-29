package main

import (
	"context"
	"database/sql"
	"log/slog"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func newTestArchive(t *testing.T) *messageArchive {
	t.Helper()
	db, err := sql.Open("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	a, err := newMessageArchive(context.Background(), db,
		slog.New(slog.DiscardHandler), 60*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestArchiveEditKeepsPreviousContent(t *testing.T) {
	a := newTestArchive(t)
	ctx := context.Background()
	at := time.Now()
	a.add(ctx, archivedMessage{ID: "one", At: at, Kind: "testo", Text: "prima"})

	old, found, changed := a.edit(ctx, "one",
		archivedMessage{Kind: "testo", Text: "dopo"}, at.Add(time.Minute))
	if !found || !changed || old.Text != "prima" {
		t.Fatalf("unexpected edit result: found=%v changed=%v old=%+v", found, changed, old)
	}
	current, ok := a.get(ctx, "one")
	if !ok || current.Text != "dopo" {
		t.Fatalf("edited content was not stored: %+v", current)
	}
	_, _, changed = a.edit(ctx, "one",
		archivedMessage{Kind: "testo", Text: "dopo"}, at.Add(2*time.Minute))
	if changed {
		t.Fatal("a replay of the same edit must be ignored")
	}
}

func TestArchiveMigratesPreviousSentMessageLabels(t *testing.T) {
	db, err := sql.Open("sqlite", "file:migration?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	ctx := context.Background()
	log := slog.New(slog.DiscardHandler)
	sent, err := newSentLog(ctx, db, log)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	sent.add(ctx, "legacy", at, "foto")

	a, err := newMessageArchive(ctx, db, log, 60*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	m, ok := a.get(ctx, "legacy")
	if !ok || !m.FromMe || m.Kind != "foto" {
		t.Fatalf("legacy sent message was not migrated: %+v, found=%v", m, ok)
	}
}

func TestArchiveDeleteReturnsOriginalOnce(t *testing.T) {
	a := newTestArchive(t)
	ctx := context.Background()
	at := time.Now()
	a.add(ctx, archivedMessage{ID: "photo", At: at, Kind: "foto", Text: "didascalia"})

	old, found, changed := a.delete(ctx, "photo", at.Add(time.Minute))
	if !found || !changed || old.Kind != "foto" || old.Text != "didascalia" {
		t.Fatalf("unexpected deletion result: found=%v changed=%v old=%+v", found, changed, old)
	}
	_, _, changed = a.delete(ctx, "photo", at.Add(2*time.Minute))
	if changed {
		t.Fatal("a replayed revocation must not create another event")
	}
}

func TestArchiveUnknownDeleteCreatesDeduplicationTombstone(t *testing.T) {
	a := newTestArchive(t)
	ctx := context.Background()
	at := time.Now()

	_, found, changed := a.delete(ctx, "unknown", at)
	if found || !changed {
		t.Fatalf("first unknown deletion: found=%v changed=%v", found, changed)
	}
	_, found, changed = a.delete(ctx, "unknown", at.Add(time.Minute))
	if !found || changed {
		t.Fatalf("replayed unknown deletion: found=%v changed=%v", found, changed)
	}
}

func TestArchiveTracksReactionChangesAndRemoval(t *testing.T) {
	a := newTestArchive(t)
	ctx := context.Background()
	at := time.Now()

	if old, changed := a.reaction(ctx, "one", "❤️", at); old != "" || !changed {
		t.Fatalf("first reaction: old=%q changed=%v", old, changed)
	}
	if old, changed := a.reaction(ctx, "one", "😂", at.Add(time.Second)); old != "❤️" || !changed {
		t.Fatalf("changed reaction: old=%q changed=%v", old, changed)
	}
	if old, changed := a.reaction(ctx, "one", "", at.Add(2*time.Second)); old != "😂" || !changed {
		t.Fatalf("removed reaction: old=%q changed=%v", old, changed)
	}
	if old, changed := a.reaction(ctx, "one", "❤️", at.Add(time.Second)); old != "" || changed {
		t.Fatalf("an older replay resurrected a removed reaction: old=%q changed=%v", old, changed)
	}
	if _, changed := a.reaction(ctx, "one", "", at.Add(3*time.Second)); changed {
		t.Fatal("replayed removal must be ignored")
	}
}

func TestArchiveReturnsRecentConversationInChronologicalOrder(t *testing.T) {
	a := newTestArchive(t)
	ctx := context.Background()
	at := time.Now()
	a.add(ctx, archivedMessage{
		ID: "one", At: at.Add(-3 * time.Minute), Kind: "testo",
		Text: "prima", FromMe: false,
	})
	a.add(ctx, archivedMessage{
		ID: "two", At: at.Add(-2 * time.Minute), Kind: "testo",
		Text: "dopo", FromMe: true,
	})
	a.markAgentMessage(
		ctx, "agent", at.Add(-time.Minute), "testo", "risposta di Gianna", "gianna",
	)
	a.markAgentMessage(
		ctx, "ack", at.Add(-30*time.Second), "ricevuta AI", "Ricevuto", "ack",
	)
	a.add(ctx, archivedMessage{
		ID: "current", At: at, Kind: "testo", Text: "@gianna perché?",
	})

	got := a.recentBefore(ctx, "current", at, time.Hour, 20, 10_000)
	if len(got) != 3 {
		t.Fatalf("expected 3 context messages, got %d: %+v", len(got), got)
	}
	if got[0].ID != "one" || got[1].ID != "two" || got[2].ID != "agent" {
		t.Fatalf("messages are not chronological: %+v", got)
	}
	if got[2].AgentSender != "gianna" {
		t.Fatalf("agent attribution was lost: %+v", got[2])
	}
}

func TestArchiveRecentConversationRespectsCharacterBudget(t *testing.T) {
	a := newTestArchive(t)
	ctx := context.Background()
	at := time.Now()
	a.add(ctx, archivedMessage{
		ID: "long", At: at.Add(-time.Minute), Kind: "testo",
		Text: "abcdefghij",
	})

	got := a.recentBefore(ctx, "current", at, time.Hour, 10, 5)
	if len(got) != 1 || got[0].Text != "abcde" {
		t.Fatalf("unexpected capped context: %+v", got)
	}
}

func TestExcerptFlattensAndLimitsText(t *testing.T) {
	got := excerpt("  una\n frase   con spazi ")
	if got != "una frase con spazi" {
		t.Fatalf("unexpected excerpt %q", got)
	}
	long := ""
	for range 120 {
		long += "a"
	}
	if got := []rune(excerpt(long)); len(got) != 100 || got[len(got)-1] != '…' {
		t.Fatalf("long excerpt was not capped: %q (%d runes)", string(got), len(got))
	}
}
