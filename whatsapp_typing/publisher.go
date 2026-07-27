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
	Attrs       bool
	Template    string
	Value       func(s State) string
}

const unknown = "unknown"

func entities() []entity {
	return []entity{
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
	}
}

// statePayload is what lands on the single MQTT state topic.
func statePayload(s State) ([]byte, error) {
	typing := "OFF"
	if s.Typing {
		typing = "ON"
	}
	last := unknown
	if !s.LastTypingAt.IsZero() {
		last = s.LastTypingAt.Format(time.RFC3339)
	}
	return json.Marshal(map[string]any{
		"typing":           typing,
		"status":           s.Status,
		"current_duration": s.CurrentDuration,
		"last_duration":    s.LastDuration,
		"last_typing":      last,
		"sessions_today":   s.SessionsToday,
		"seconds_today":    s.SecondsToday,
		"attributes":       s.Attributes,
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
}

func NewHAPublisher(cfg Config, log *slog.Logger) *HAPublisher {
	return &HAPublisher{cfg: cfg, log: log, http: &http.Client{Timeout: 10 * time.Second}}
}

func (p *HAPublisher) PublishState(s State) error {
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
		body, err := json.Marshal(map[string]any{"state": state, "attributes": attrs})
		if err != nil {
			return err
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
	}
	return nil
}

func (p *HAPublisher) Close() {}
