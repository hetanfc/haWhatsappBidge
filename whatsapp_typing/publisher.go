package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

type Publisher interface {
	PublishState(s State) error
	Close()
}

// entity describes one Home Assistant entity, once, for both publishers:
// MQTT uses Template against the JSON state topic, the REST publisher uses Value.
type entity struct {
	Key         string
	Kind        string // binary_sensor | sensor
	Name        string
	Icon        string
	DeviceClass string
	StateClass  string
	Unit        string
	Attrs       bool // carries the shared attributes of the typing sensor
	Timeline    bool // carries the readable history, published only when it changes
	Template    string
	Value       func(s State) string
}

const unknown = "unknown"

func entities() []entity {
	return []entity{
		{
			Key: "activity", Kind: "sensor", Name: "Attività",
			Icon: "mdi:timeline-text-outline", Timeline: true,
			Template: "{{ value_json.activity }}",
			Value:    func(s State) string { return s.Activity },
		},
		{
			Key: "typing", Kind: "binary_sensor", Name: "Sta scrivendo",
			Icon: "mdi:pencil", Attrs: true,
			Template: "{{ value_json.typing }}",
			Value: func(s State) string {
				if s.Typing {
					return "on"
				}
				return "off"
			},
		},
		{
			Key: "online", Kind: "binary_sensor", Name: "Online",
			Icon:     "mdi:account-circle",
			Template: "{{ value_json.online }}",
			Value: func(s State) string {
				if !s.PresenceKnown {
					return unknown
				}
				if s.Online {
					return "on"
				}
				return "off"
			},
		},
		{
			Key: "last_seen", Kind: "sensor", Name: "Ultimo accesso",
			Icon: "mdi:account-clock-outline", DeviceClass: "timestamp",
			Template: "{{ value_json.last_seen }}",
			Value:    func(s State) string { return timestamp(s.LastSeenAt) },
		},
		{
			Key: "last_presence", Kind: "sensor", Name: "Ultimo cambio presenza",
			Icon: "mdi:account-sync-outline", DeviceClass: "timestamp",
			Template: "{{ value_json.last_presence }}",
			Value:    func(s State) string { return timestamp(s.LastPresenceAt) },
		},
		{
			Key: "status", Kind: "sensor", Name: "Stato",
			Icon:     "mdi:message-text-clock",
			Template: "{{ value_json.status }}",
			Value:    func(s State) string { return s.Status },
		},
		{
			Key: "current_duration", Kind: "sensor", Name: "Durata sessione",
			Icon: "mdi:timer-play-outline", DeviceClass: "duration", StateClass: "measurement", Unit: "s",
			Template: "{{ value_json.current_duration }}",
			Value:    func(s State) string { return strconv.Itoa(s.CurrentDuration) },
		},
		{
			Key: "last_duration", Kind: "sensor", Name: "Ultima durata",
			Icon: "mdi:timer-outline", DeviceClass: "duration", StateClass: "measurement", Unit: "s",
			Template: "{{ value_json.last_duration }}",
			Value:    func(s State) string { return strconv.Itoa(s.LastDuration) },
		},
		{
			Key: "last_typing", Kind: "sensor", Name: "Ultima volta",
			DeviceClass: "timestamp",
			Template:    "{{ value_json.last_typing }}",
			Value: func(s State) string {
				if s.LastTypingAt.IsZero() {
					return unknown
				}
				return s.LastTypingAt.Format(time.RFC3339)
			},
		},
		{
			Key: "sessions_today", Kind: "sensor", Name: "Sessioni oggi",
			Icon: "mdi:counter", StateClass: "total_increasing",
			Template: "{{ value_json.sessions_today }}",
			Value:    func(s State) string { return strconv.Itoa(s.SessionsToday) },
		},
		{
			Key: "seconds_today", Kind: "sensor", Name: "Tempo totale oggi",
			Icon: "mdi:clock-outline", DeviceClass: "duration", StateClass: "total_increasing", Unit: "s",
			Template: "{{ value_json.seconds_today }}",
			Value:    func(s State) string { return strconv.Itoa(s.SecondsToday) },
		},
		{
			Key: "pauses_today", Kind: "sensor", Name: "Pause di scrittura oggi",
			Icon: "mdi:pause-circle-outline", StateClass: "total_increasing",
			Template: "{{ value_json.pauses_today }}",
			Value:    func(s State) string { return strconv.Itoa(s.PausesToday) },
		},
		{
			Key: "restarts_today", Kind: "sensor", Name: "Riprese di scrittura oggi",
			Icon: "mdi:restart", StateClass: "total_increasing",
			Template: "{{ value_json.restarts_today }}",
			Value:    func(s State) string { return strconv.Itoa(s.RestartsToday) },
		},
		{
			Key: "last_read", Kind: "sensor", Name: "Ultima lettura",
			Icon: "mdi:check-all", DeviceClass: "timestamp",
			Template: "{{ value_json.last_read }}",
			Value:    func(s State) string { return timestamp(s.LastReadAt) },
		},
		{
			Key: "last_delivered", Kind: "sensor", Name: "Ultima consegna",
			Icon: "mdi:check", DeviceClass: "timestamp",
			Template: "{{ value_json.last_delivered }}",
			Value:    func(s State) string { return timestamp(s.LastDeliveredAt) },
		},
		{
			Key: "last_played", Kind: "sensor", Name: "Ultima riproduzione",
			Icon: "mdi:play-circle-outline", DeviceClass: "timestamp",
			Template: "{{ value_json.last_played }}",
			Value:    func(s State) string { return timestamp(s.LastPlayedAt) },
		},
		{
			Key: "reads_today", Kind: "sensor", Name: "Letture oggi",
			Icon: "mdi:eye-check-outline", StateClass: "total_increasing",
			Template: "{{ value_json.reads_today }}",
			Value:    func(s State) string { return strconv.Itoa(s.ReadsToday) },
		},
		{
			Key: "last_read_target", Kind: "sensor", Name: "Cosa ha letto",
			Icon:     "mdi:file-eye-outline",
			Template: "{{ value_json.last_read_target }}",
			Value:    func(s State) string { return orUnknown(s.ReadTarget) },
		},
		{
			Key: "last_played_target", Kind: "sensor", Name: "Cosa ha riprodotto",
			Icon:     "mdi:motion-play-outline",
			Template: "{{ value_json.last_played_target }}",
			Value:    func(s State) string { return orUnknown(s.PlayedTarget) },
		},
		{
			Key: "last_message", Kind: "sensor", Name: "Ultimo messaggio ricevuto",
			Icon: "mdi:message-arrow-left-outline", DeviceClass: "timestamp",
			Template: "{{ value_json.last_message }}",
			Value:    func(s State) string { return timestamp(s.LastMessageAt) },
		},
		{
			Key: "messages_today", Kind: "sensor", Name: "Messaggi ricevuti oggi",
			Icon: "mdi:message-badge-outline", StateClass: "total_increasing",
			Template: "{{ value_json.messages_today }}",
			Value:    func(s State) string { return strconv.Itoa(s.MessagesToday) },
		},
		{
			Key: "last_reaction", Kind: "sensor", Name: "Ultima reazione",
			Icon: "mdi:emoticon-outline", DeviceClass: "timestamp",
			Template: "{{ value_json.last_reaction }}",
			Value:    func(s State) string { return timestamp(s.LastReactionAt) },
		},
		{
			Key: "last_reaction_emoji", Kind: "sensor", Name: "Emoji ultima reazione",
			Icon:     "mdi:emoticon-happy-outline",
			Template: "{{ value_json.last_reaction_emoji }}",
			Value:    func(s State) string { return orUnknown(s.LastReactionEmoji) },
		},
		{
			Key: "last_reaction_target", Kind: "sensor", Name: "Messaggio ultima reazione",
			Icon:     "mdi:message-reply-text-outline",
			Template: "{{ value_json.last_reaction_target }}",
			Value:    func(s State) string { return orUnknown(s.LastReactionTarget) },
		},
		{
			Key: "last_edit", Kind: "sensor", Name: "Ultima modifica",
			Icon: "mdi:message-draw", DeviceClass: "timestamp",
			Template: "{{ value_json.last_edit }}",
			Value:    func(s State) string { return timestamp(s.LastEditAt) },
		},
		{
			Key: "last_delete", Kind: "sensor", Name: "Ultima eliminazione",
			Icon: "mdi:message-off-outline", DeviceClass: "timestamp",
			Template: "{{ value_json.last_delete }}",
			Value:    func(s State) string { return timestamp(s.LastDeleteAt) },
		},
	}
}

// orUnknown keeps a sensor valid when a receipt refers to something we never
// saw, which is normal for messages sent before this add-on was running.
func orUnknown(s string) string {
	if s == "" {
		return unknown
	}
	return s
}

// timestamp renders a time for a timestamp sensor: Home Assistant rejects an
// empty string, so a receipt we have never seen is reported as unknown.
func timestamp(t time.Time) string {
	if t.IsZero() {
		return unknown
	}
	return t.Format(time.RFC3339)
}

// timelineAttrs is the readable history attached to the activity sensor. It is
// published on its own topic and only when it changes: the state topic is
// refreshed every couple of seconds while she types, and dragging 50 entries
// along every time would bloat the Home Assistant recorder for nothing.
func timelineAttrs(s State) map[string]any {
	entries := s.Timeline
	if entries == nil {
		entries = []TimelineEntry{}
	}
	attrs := map[string]any{
		"timeline":   entries,
		"last_event": s.Activity,
	}
	if !s.ActivitySince.IsZero() {
		attrs["since"] = s.ActivitySince.Format(time.RFC3339)
	}
	return attrs
}

// statePayload is what lands on the single MQTT state topic.
func statePayload(s State) ([]byte, error) {
	typing := "OFF"
	if s.Typing {
		typing = "ON"
	}
	online := "UNKNOWN"
	if s.PresenceKnown {
		online = "OFF"
		if s.Online {
			online = "ON"
		}
	}
	return json.Marshal(map[string]any{
		"typing":               typing,
		"online":               online,
		"last_seen":            timestamp(s.LastSeenAt),
		"last_presence":        timestamp(s.LastPresenceAt),
		"status":               s.Status,
		"activity":             s.Activity,
		"current_duration":     s.CurrentDuration,
		"last_duration":        s.LastDuration,
		"last_typing":          timestamp(s.LastTypingAt),
		"sessions_today":       s.SessionsToday,
		"seconds_today":        s.SecondsToday,
		"pauses_today":         s.PausesToday,
		"restarts_today":       s.RestartsToday,
		"last_read":            timestamp(s.LastReadAt),
		"last_delivered":       timestamp(s.LastDeliveredAt),
		"last_played":          timestamp(s.LastPlayedAt),
		"reads_today":          s.ReadsToday,
		"last_message":         timestamp(s.LastMessageAt),
		"messages_today":       s.MessagesToday,
		"last_reaction":        timestamp(s.LastReactionAt),
		"last_reaction_emoji":  orUnknown(s.LastReactionEmoji),
		"last_reaction_target": orUnknown(s.LastReactionTarget),
		"last_edit":            timestamp(s.LastEditAt),
		"last_delete":          timestamp(s.LastDeleteAt),
		"last_read_target":     orUnknown(s.ReadTarget),
		"last_played_target":   orUnknown(s.PlayedTarget),
		"attributes":           s.Attributes,
	})
}

// NewPublisher builds the publisher selected in the config.
func NewPublisher(cfg Config, log *slog.Logger) (Publisher, error) {
	if cfg.Publisher == "ha" {
		return NewHAPublisher(cfg, log), nil
	}
	return NewMQTTPublisher(cfg, log)
}

// HAPublisher writes straight into Home Assistant's REST API. Simpler than MQTT
// (no broker) but the entities are runtime-only: they exist as long as this
// service keeps pushing, and they are not restored across HA restarts until the
// next update. MQTT discovery is the better option when a broker is available.
type HAPublisher struct {
	cfg  Config
	log  *slog.Logger
	http *http.Client

	lastPost map[string]string // per entity, to skip identical writes
	lastFull time.Time         // when we last wrote everything regardless
}

func NewHAPublisher(cfg Config, log *slog.Logger) *HAPublisher {
	return &HAPublisher{
		cfg:      cfg,
		log:      log,
		http:     &http.Client{Timeout: 10 * time.Second},
		lastPost: map[string]string{},
	}
}

// fullResync is how often every entity is rewritten even when nothing changed,
// so a Home Assistant restart doesn't leave the entities missing for long.
const fullResync = 2 * time.Minute

func (p *HAPublisher) PublishState(s State) error {
	full := time.Since(p.lastFull) >= fullResync
	if full {
		p.lastFull = time.Now()
	}

	for _, e := range entities() {
		id := fmt.Sprintf("%s.%s_%s", e.Kind, p.cfg.Slug, e.Key)
		state := e.Value(s)
		if !s.Available {
			state = "unavailable"
		}
		attrs := map[string]any{
			"friendly_name": fmt.Sprintf("%s %s", p.cfg.Name, e.Name),
		}
		if e.Icon != "" {
			attrs["icon"] = e.Icon
		}
		if e.DeviceClass != "" {
			attrs["device_class"] = e.DeviceClass
		}
		if e.StateClass != "" {
			attrs["state_class"] = e.StateClass
		}
		if e.Unit != "" {
			attrs["unit_of_measurement"] = e.Unit
		}
		if e.Attrs {
			for k, v := range s.Attributes {
				attrs[k] = v
			}
		}
		if e.Timeline {
			for k, v := range timelineAttrs(s) {
				attrs[k] = v
			}
		}
		body, err := json.Marshal(map[string]any{"state": state, "attributes": attrs})
		if err != nil {
			return err
		}
		// Nothing moved for this entity: writing it again would only add rows to
		// the recorder and traffic to the API.
		if !full && p.lastPost[id] == string(body) {
			continue
		}
		req, err := http.NewRequest(http.MethodPost, p.cfg.HAURL+"/api/states/"+id, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+p.cfg.HAToken)
		req.Header.Set("Content-Type", "application/json")
		resp, err := p.http.Do(req)
		if err != nil {
			return err
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			return fmt.Errorf("home assistant returned %s for %s", resp.Status, id)
		}
		p.lastPost[id] = string(body)
	}
	return nil
}

func (p *HAPublisher) Close() {}
