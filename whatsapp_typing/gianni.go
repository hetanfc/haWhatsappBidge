package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

type gianniInbound struct {
	Version          int                   `json:"version"`
	MessageID        string                `json:"message_id"`
	ChatID           string                `json:"chat_id"`
	SenderID         string                `json:"sender_id"`
	SenderRole       string                `json:"sender_role"`
	SenderName       string                `json:"sender_name"`
	FromMe           bool                  `json:"from_me"`
	BotGenerated     bool                  `json:"bot_generated"`
	Text             string                `json:"text"`
	Timestamp        string                `json:"timestamp"`
	ReplyToMessageID string                `json:"reply_to_message_id,omitempty"`
	RecentMessages   []gianniRecentMessage `json:"recent_messages,omitempty"`
}

type gianniRecentMessage struct {
	MessageID        string `json:"message_id"`
	SenderRole       string `json:"sender_role"`
	SenderName       string `json:"sender_name"`
	Text             string `json:"text"`
	Timestamp        string `json:"timestamp"`
	ReplyToMessageID string `json:"reply_to_message_id,omitempty"`
}

type gianniOutbound struct {
	Version      int     `json:"version"`
	ResponseID   string  `json:"response_id"`
	ChatID       string  `json:"chat_id"`
	Sender       string  `json:"sender"`
	BotGenerated bool    `json:"bot_generated"`
	Type         string  `json:"type"`
	Text         string  `json:"text"`
	Source       string  `json:"source"`
	Caption      string  `json:"caption"`
	Latitude     float64 `json:"latitude"`
	Longitude    float64 `json:"longitude"`
	Name         string  `json:"name"`
}

type GianniRelay struct {
	cfg         Config
	wa          *WhatsApp
	mqtt        *MQTTPublisher
	log         *slog.Logger
	outbox      chan []byte
	sendMu      sync.Mutex
	sentMu      sync.Mutex
	sentByBot   map[string]time.Time
	statusMu    sync.RWMutex
	bridgeState string
	bridgeMode  string
	http        *http.Client
}

func NewGianniRelay(cfg Config, wa *WhatsApp, mqttPub *MQTTPublisher, log *slog.Logger) *GianniRelay {
	return &GianniRelay{
		cfg:         cfg,
		wa:          wa,
		mqtt:        mqttPub,
		log:         log,
		outbox:      make(chan []byte, 32),
		sentByBot:   make(map[string]time.Time),
		bridgeState: "unknown",
		bridgeMode:  "unknown",
		http: &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return fmt.Errorf("too many redirects")
				}
				return nil
			},
		},
	}
}

func (g *GianniRelay) Start(ctx context.Context) error {
	subscribe := func(cli mqtt.Client) {
		tok := cli.Subscribe(g.cfg.GianniTopicOut, 1, func(_ mqtt.Client, msg mqtt.Message) {
			payload := append([]byte(nil), msg.Payload()...)
			select {
			case g.outbox <- payload:
			case <-ctx.Done():
			}
		})
		if !tok.WaitTimeout(10 * time.Second) {
			g.log.Error("Gianni MQTT subscription timed out", "topic", g.cfg.GianniTopicOut)
		} else if err := tok.Error(); err != nil {
			g.log.Error("Gianni MQTT subscription failed", "topic", g.cfg.GianniTopicOut, "err", err)
		} else {
			g.log.Info("Gianni relay listening", "topic", g.cfg.GianniTopicOut)
		}
		statusTok := cli.Subscribe(g.cfg.GianniTopicStatus, 1, func(_ mqtt.Client, msg mqtt.Message) {
			g.updateBridgeStatus(msg.Payload())
		})
		if !statusTok.WaitTimeout(10*time.Second) || statusTok.Error() != nil {
			g.log.Warn("Gianni status subscription failed", "topic", g.cfg.GianniTopicStatus)
		}
	}
	g.mqtt.OnConnect(subscribe)
	tok := g.mqtt.cli.Subscribe(g.cfg.GianniTopicOut, 1, func(_ mqtt.Client, msg mqtt.Message) {
		payload := append([]byte(nil), msg.Payload()...)
		select {
		case g.outbox <- payload:
		case <-ctx.Done():
		}
	})
	if !tok.WaitTimeout(10 * time.Second) {
		return fmt.Errorf("timeout subscribing to %s", g.cfg.GianniTopicOut)
	}
	if err := tok.Error(); err != nil {
		return fmt.Errorf("subscribe to %s: %w", g.cfg.GianniTopicOut, err)
	}
	statusTok := g.mqtt.cli.Subscribe(g.cfg.GianniTopicStatus, 1, func(_ mqtt.Client, msg mqtt.Message) {
		g.updateBridgeStatus(msg.Payload())
	})
	if !statusTok.WaitTimeout(10 * time.Second) {
		return fmt.Errorf("timeout subscribing to %s", g.cfg.GianniTopicStatus)
	}
	if err := statusTok.Error(); err != nil {
		return fmt.Errorf("subscribe to %s: %w", g.cfg.GianniTopicStatus, err)
	}
	go g.run(ctx)
	g.log.Info("AI relay enabled", "gianni_mention", g.cfg.GianniMention,
		"gianna_mention", g.cfg.GiannaMention,
		"inbox", g.cfg.GianniTopicIn, "outbox", g.cfg.GianniTopicOut)
	return nil
}

func (g *GianniRelay) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case payload := <-g.outbox:
			if err := g.deliver(ctx, payload); err != nil {
				g.log.Error("Gianni WhatsApp delivery failed", "err", err)
			}
		}
	}
}

func (g *GianniRelay) Forward(evt *events.Message, text string, at time.Time) {
	if evt == nil {
		return
	}

	hasMention := containsAnyMention(text, g.cfg.GianniMention, g.cfg.GiannaMention)
	senderID := evt.Info.Sender.String()
	senderRole := "contact"
	senderName := g.cfg.Name
	if evt.Info.IsFromMe {
		senderRole = "owner"
		senderName = "Proprietario"
		if g.wa.cli.Store.ID != nil {
			senderID = g.wa.cli.Store.ID.String()
		}
	} else if senderID == "" {
		senderID = g.wa.target.String()
	}

	in := gianniInbound{
		Version:          1,
		MessageID:        string(evt.Info.ID),
		ChatID:           g.wa.target.String(),
		SenderID:         senderID,
		SenderRole:       senderRole,
		SenderName:       senderName,
		FromMe:           evt.Info.IsFromMe,
		BotGenerated:     g.wasSentByBot(string(evt.Info.ID)),
		Text:             text,
		Timestamp:        at.Format(time.RFC3339),
		ReplyToMessageID: messageContext(evt.Message).GetStanzaID(),
	}
	if in.BotGenerated {
		return
	}
	if !hasMention {
		return
	}
	if g.cfg.GianniContextEnabled && g.wa.archive != nil {
		recent := g.wa.archive.recentBefore(
			context.Background(),
			in.MessageID,
			at,
			time.Duration(g.cfg.GianniContextHours)*time.Hour,
			g.cfg.GianniContextMaxMessages,
			64_000,
		)
		in.RecentMessages = make([]gianniRecentMessage, 0, len(recent))
		for _, message := range recent {
			role := "contact"
			name := g.cfg.Name
			if message.AgentSender == "gianni" {
				role, name = "agent", "Gianni"
			} else if message.AgentSender == "gianna" {
				role, name = "agent", "Gianna"
			} else if message.FromMe {
				role, name = "owner", "Proprietario"
			}
			in.RecentMessages = append(in.RecentMessages, gianniRecentMessage{
				MessageID:        message.ID,
				SenderRole:       role,
				SenderName:       name,
				Text:             message.Text,
				Timestamp:        message.At.Format(time.RFC3339),
				ReplyToMessageID: message.QuotedID,
			})
		}
	}
	body, err := json.Marshal(in)
	if err != nil {
		g.log.Error("Gianni inbound encoding failed", "err", err)
		return
	}
	tok := g.mqtt.cli.Publish(g.cfg.GianniTopicIn, 1, false, body)
	if !tok.WaitTimeout(10 * time.Second) {
		g.log.Error("Gianni inbound publish timed out", "message_id", in.MessageID)
		return
	}
	if err := tok.Error(); err != nil {
		g.log.Error("Gianni inbound publish failed", "message_id", in.MessageID, "err", err)
		return
	}
	g.log.Info("message forwarded to AI bridge", "message_id", in.MessageID, "sender", senderRole)
	if g.cfg.GianniAckEnabled {
		go g.sendAcknowledgement(in)
	}
}

func (g *GianniRelay) updateBridgeStatus(payload []byte) {
	var status struct {
		State string `json:"state"`
		Mode  string `json:"mode"`
	}
	if err := json.Unmarshal(payload, &status); err != nil {
		g.log.Warn("invalid Gianni status", "err", err)
		return
	}
	g.statusMu.Lock()
	g.bridgeState = strings.ToLower(strings.TrimSpace(status.State))
	g.bridgeMode = strings.ToLower(strings.TrimSpace(status.Mode))
	g.statusMu.Unlock()
}

func acknowledgementText(in gianniInbound, state, mode, gianniMention, giannaMention string) string {
	if state == "offline" {
		return "👽 _Ricevuto, ma GianniBridge risulta offline._"
	}
	if mode == "paused" {
		return "👽 _Ricevuto, ma il bridge è in pausa._"
	}
	hasGianni := containsMention(in.Text, gianniMention)
	hasGianna := containsMention(in.Text, giannaMention)
	if hasGianni && hasGianna {
		return "👽 _Ricevuto. Li metto a confronto._"
	}
	if hasGianna {
		return "👽 _Ricevuto. Avviso Gianna._"
	}
	return "👽 _Ricevuto. Avviso Gianni._"
}

func (g *GianniRelay) sendAcknowledgement(in gianniInbound) {
	g.statusMu.RLock()
	state, mode := g.bridgeState, g.bridgeMode
	g.statusMu.RUnlock()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	g.sendMu.Lock()
	defer g.sendMu.Unlock()
	text := acknowledgementText(
		in, state, mode, g.cfg.GianniMention, g.cfg.GiannaMention,
	)
	if err := g.send(
		ctx,
		&waE2E.Message{Conversation: proto.String(text)},
		"ricevuta AI",
		text,
		"ack",
	); err != nil {
		g.log.Warn("Gianni acknowledgement failed", "err", err)
	}
}

func (g *GianniRelay) deliver(ctx context.Context, payload []byte) error {
	var out gianniOutbound
	if err := json.Unmarshal(payload, &out); err != nil {
		return fmt.Errorf("invalid Gianni JSON: %w", err)
	}
	if out.Version != 1 || !isTrustedAgentSender(out.Sender) || !out.BotGenerated {
		return fmt.Errorf("untrusted AI response")
	}
	if out.ResponseID == "" || out.ChatID != g.wa.target.String() {
		return fmt.Errorf("Gianni response is for another chat")
	}
	if !g.wa.cli.IsConnected() || !g.wa.cli.IsLoggedIn() {
		return fmt.Errorf("WhatsApp is not connected")
	}

	g.sendMu.Lock()
	defer g.sendMu.Unlock()

	switch out.Type {
	case "text":
		if strings.TrimSpace(out.Text) == "" {
			return fmt.Errorf("empty text response")
		}
		return g.send(
			ctx,
			&waE2E.Message{Conversation: proto.String(out.Text)},
			"testo",
			out.Text,
			out.Sender,
		)
	case "location":
		if out.Latitude < -90 || out.Latitude > 90 || out.Longitude < -180 || out.Longitude > 180 {
			return fmt.Errorf("invalid location coordinates")
		}
		msg := &waE2E.LocationMessage{
			DegreesLatitude:  proto.Float64(out.Latitude),
			DegreesLongitude: proto.Float64(out.Longitude),
			Name:             proto.String(out.Name),
			Comment:          proto.String(out.Text),
		}
		return g.send(
			ctx,
			&waE2E.Message{LocationMessage: msg},
			"posizione",
			strings.TrimSpace(strings.Join([]string{out.Name, out.Text}, " ")),
			out.Sender,
		)
	case "image":
		return g.sendImage(ctx, out.Source, out.Caption, out.Sender)
	default:
		return fmt.Errorf("unsupported Gianni response type %q", out.Type)
	}
}

func (g *GianniRelay) send(
	ctx context.Context,
	msg *waE2E.Message,
	kind string,
	contextText string,
	agentSender string,
) error {
	resp, err := g.wa.cli.SendMessage(ctx, g.wa.target, msg)
	if err != nil {
		return err
	}
	id := string(resp.ID)
	g.markSentByBot(id)
	g.wa.sent.add(ctx, id, resp.Timestamp, kind)
	if g.wa.archive != nil {
		g.wa.archive.markAgentMessage(
			ctx,
			id,
			resp.Timestamp,
			kind,
			contextText,
			agentSender,
		)
	}
	g.log.Info("Gianni response sent to WhatsApp", "response_message_id", id, "kind", kind)
	return nil
}

func (g *GianniRelay) sendImage(ctx context.Context, source, caption, agentSender string) error {
	parsed, err := url.Parse(source)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("Gianni image source must be an HTTP(S) URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return err
	}
	resp, err := g.http.Do(req)
	if err != nil {
		return fmt.Errorf("download image: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("download image: HTTP %d", resp.StatusCode)
	}
	maxBytes := int64(g.cfg.GianniImageMaxMB) * 1024 * 1024
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return fmt.Errorf("read image: %w", err)
	}
	if int64(len(data)) > maxBytes {
		return fmt.Errorf("image exceeds %d MB", g.cfg.GianniImageMaxMB)
	}
	mime := resp.Header.Get("Content-Type")
	if mime == "" || mime == "application/octet-stream" {
		mime = http.DetectContentType(data)
	}
	mime = strings.TrimSpace(strings.Split(mime, ";")[0])
	if !strings.HasPrefix(mime, "image/") {
		return fmt.Errorf("source is not an image (%s)", mime)
	}
	uploaded, err := g.wa.cli.Upload(ctx, data, whatsmeow.MediaImage)
	if err != nil {
		return fmt.Errorf("upload image to WhatsApp: %w", err)
	}
	msg := &waE2E.ImageMessage{
		URL:           proto.String(uploaded.URL),
		DirectPath:    proto.String(uploaded.DirectPath),
		MediaKey:      uploaded.MediaKey,
		Mimetype:      proto.String(mime),
		FileEncSHA256: uploaded.FileEncSHA256,
		FileSHA256:    uploaded.FileSHA256,
		FileLength:    proto.Uint64(uploaded.FileLength),
		Caption:       proto.String(caption),
	}
	return g.send(ctx, &waE2E.Message{ImageMessage: msg}, "foto", caption, agentSender)
}

func containsMention(text, mention string) bool {
	return strings.Contains(strings.ToLower(text), strings.ToLower(mention))
}

func containsAnyMention(text string, mentions ...string) bool {
	for _, mention := range mentions {
		if containsMention(text, mention) {
			return true
		}
	}
	return false
}

func isTrustedAgentSender(sender string) bool {
	return sender == "gianni" || sender == "gianna"
}

func (g *GianniRelay) markSentByBot(id string) {
	g.sentMu.Lock()
	defer g.sentMu.Unlock()
	now := time.Now()
	for key, at := range g.sentByBot {
		if now.Sub(at) > 24*time.Hour {
			delete(g.sentByBot, key)
		}
	}
	g.sentByBot[id] = now
}

func (g *GianniRelay) wasSentByBot(id string) bool {
	g.sentMu.Lock()
	defer g.sentMu.Unlock()
	_, ok := g.sentByBot[id]
	return ok
}
