package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func testArchiveWeb(t *testing.T) (*ArchiveWeb, *messageArchive) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	archive, err := newMessageArchive(context.Background(), db, log, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return NewArchiveWeb(db, "Contatto", log), archive
}

func TestArchiveWebReturnsFullDeletedText(t *testing.T) {
	web, archive := testArchiveWeb(t)
	ctx := context.Background()
	at := time.Now().Add(-time.Minute).Truncate(time.Second)
	longText := strings.Repeat("messaggio molto lungo ", 20)
	archive.add(ctx, archivedMessage{ID: "long", At: at, Kind: "testo", Text: longText})
	archive.delete(ctx, "long", at.Add(10*time.Second))

	req := httptest.NewRequest("GET", "/api/messages?filter=deleted", nil)
	rw := httptest.NewRecorder()
	web.messages(rw, req)
	if rw.Code != 200 {
		t.Fatalf("unexpected status %d: %s", rw.Code, rw.Body.String())
	}
	var response archiveResponse
	if err := json.Unmarshal(rw.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Messages) != 1 || response.Messages[0].Text != longText {
		t.Fatalf("full text was not returned: %+v", response.Messages)
	}
	if response.Messages[0].DeletedAt == nil {
		t.Fatal("deleted timestamp missing")
	}
}

func TestArchiveWebSearchAndEditHistory(t *testing.T) {
	web, archive := testArchiveWeb(t)
	ctx := context.Background()
	at := time.Now().Add(-time.Minute).Truncate(time.Second)
	archive.add(ctx, archivedMessage{ID: "edit", At: at, Kind: "testo", Text: "prima versione"})
	archive.edit(ctx, "edit", archivedMessage{Kind: "testo", Text: "seconda versione speciale"}, at.Add(time.Second))
	archive.add(ctx, archivedMessage{ID: "other", At: at, Kind: "testo", Text: "non deve comparire"})

	rows, err := web.queryMessages(ctx, "edited", "speciale", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Text != "seconda versione speciale" {
		t.Fatalf("unexpected rows: %+v", rows)
	}
	if len(rows[0].Revisions) != 1 || rows[0].Revisions[0].Text != "prima versione" {
		t.Fatalf("edit history missing: %+v", rows[0].Revisions)
	}
}
