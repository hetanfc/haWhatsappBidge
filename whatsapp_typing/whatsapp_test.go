package main

import (
	"testing"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
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
