package main

import (
	"strings"
	"testing"
)

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

func TestContainsEitherAgentMention(t *testing.T) {
	tests := []struct {
		text string
		want bool
	}{
		{"@gianni spiegami questa cosa", true},
		{"Che ne pensi @gianna?", true},
		{"Ehi @GIANNA, ci sei?", true},
		{"messaggio senza agenti", false},
	}
	for _, tt := range tests {
		if got := containsAnyMention(tt.text, "@gianni", "@gianna"); got != tt.want {
			t.Errorf("containsAnyMention(%q) = %v, want %v", tt.text, got, tt.want)
		}
	}
}

func TestTrustedAgentSender(t *testing.T) {
	for _, sender := range []string{"gianni", "gianna"} {
		if !isTrustedAgentSender(sender) {
			t.Errorf("expected %q to be trusted", sender)
		}
	}
	for _, sender := range []string{"", "proprietario", "Gianna", "attacker"} {
		if isTrustedAgentSender(sender) {
			t.Errorf("expected %q to be rejected", sender)
		}
	}
}

func TestAcknowledgementText(t *testing.T) {
	tests := []struct {
		name  string
		text  string
		state string
		mode  string
		want  string
	}{
		{"gianni", "@gianni ciao", "online", "running", "Avviso Gianni"},
		{"gianna", "@gianna ciao", "online", "running", "Avviso Gianna"},
		{"entrambi", "@gianni @gianna parlate", "online", "running", "metto a confronto"},
		{"pausa", "@gianni ciao", "online", "paused", "bridge è in pausa"},
		{"offline", "@gianni ciao", "offline", "running", "risulta offline"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := acknowledgementText(
				gianniInbound{Text: tt.text}, tt.state, tt.mode, "@gianni", "@gianna",
			)
			if !strings.Contains(got, tt.want) {
				t.Fatalf("acknowledgementText() = %q, want substring %q", got, tt.want)
			}
		})
	}
}
