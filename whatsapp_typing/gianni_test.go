package main

import "testing"

func TestContainsMentionAnywhere(t *testing.T) {
	tests := []struct {
		text string
		want bool
	}{
		{"@gianni dove sono?", true},
		{"Che tempo farÃ  domani, @gianni?", true},
		{"Ehi @GIANNI, rispondi", true},
		{"gianni senza chiocciola", false},
		{"un messaggio normale", false},
	}
	for _, tt := range tests {
		if got := containsMention(tt.text, "@gianni"); got != tt.want {
			t.Errorf("containsMention(%q) = %v, want %v", tt.text, got, tt.want)
		}
	}
}
