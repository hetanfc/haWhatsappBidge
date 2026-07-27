package main

import (
	"testing"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func TestReceiptFromContact(t *testing.T) {
	tests := []struct {
		name   string
		evt    *events.Receipt
		wanted bool
	}{
		{
			name: "contact read receipt",
			evt: &events.Receipt{
				MessageSource: types.MessageSource{IsFromMe: false},
				Type:          types.ReceiptTypeRead,
			},
			wanted: true,
		},
		{
			name: "own device regular read receipt",
			evt: &events.Receipt{
				MessageSource: types.MessageSource{IsFromMe: true},
				Type:          types.ReceiptTypeRead,
			},
			wanted: false,
		},
		{
			name: "own device explicit read-self receipt",
			evt: &events.Receipt{
				MessageSource: types.MessageSource{IsFromMe: true},
				Type:          types.ReceiptTypeReadSelf,
			},
			wanted: false,
		},
		{
			name: "read-self is never attributed to contact",
			evt: &events.Receipt{
				MessageSource: types.MessageSource{IsFromMe: false},
				Type:          types.ReceiptTypeReadSelf,
			},
			wanted: false,
		},
		{name: "nil receipt", wanted: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := receiptFromContact(test.evt); got != test.wanted {
				t.Fatalf("receiptFromContact() = %v, wanted %v", got, test.wanted)
			}
		})
	}
}

func TestMessageMetadataExtraction(t *testing.T) {
	msg := &waE2E.Message{
		ImageMessage: &waE2E.ImageMessage{
			Caption: proto.String("una foto"),
			ContextInfo: &waE2E.ContextInfo{
				StanzaID:    proto.String("quoted-id"),
				IsForwarded: proto.Bool(true),
			},
		},
	}
	if got := messageKind(msg); got != "foto" {
		t.Fatalf("messageKind() = %q", got)
	}
	if got := messageText(msg); got != "una foto" {
		t.Fatalf("messageText() = %q", got)
	}
	if got := messageContext(msg).GetStanzaID(); got != "quoted-id" {
		t.Fatalf("quoted id = %q", got)
	}
	if !messageContext(msg).GetIsForwarded() {
		t.Fatal("forwarded flag was not extracted")
	}
}

func TestIncomingMessageLabels(t *testing.T) {
	m := archivedMessage{Kind: "testo", Text: "ciao", ViewOnce: true, Forwarded: true}
	got := incomingMessageLabel(m, "foto delle 18:42")
	want := `ha risposto a foto delle 18:42: "ciao" [visualizzazione singola, inoltrato]`
	if got != want {
		t.Fatalf("incomingMessageLabel() = %q, want %q", got, want)
	}
}
