package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

type MQTTPublisher struct {
	cfg  Config
	log  *slog.Logger
	cli  mqtt.Client
	base string
}

func NewMQTTPublisher(cfg Config, log *slog.Logger) (*MQTTPublisher, error) {
	p := &MQTTPublisher{
		cfg:  cfg,
		log:  log,
		base: "whatsapp_typing/" + cfg.Slug,
	}

	scheme := "tcp"
	if cfg.MQTT.TLS {
		scheme = "ssl"
	}
	opts := mqtt.NewClientOptions().
		AddBroker(fmt.Sprintf("%s://%s:%d", scheme, cfg.MQTT.Host, cfg.MQTT.Port)).
		SetClientID(cfg.MQTT.ClientID).
		SetUsername(cfg.MQTT.User).
		SetPassword(cfg.MQTT.Password).
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetConnectRetryInterval(10*time.Second).
		SetConnectTimeout(10*time.Second).
		SetKeepAlive(30*time.Second).
		SetOrderMatters(false).
		SetWill(p.availabilityTopic(), "offline", 1, true)

	// Discovery is re-published on every (re)connect so the entities survive a
	// broker restart that wiped retained messages.
	opts.SetOnConnectHandler(func(mqtt.Client) {
		p.log.Info("mqtt connected", "broker", cfg.MQTT.Host)
		if err := p.publishDiscovery(); err != nil {
			p.log.Error("discovery publish failed", "err", err)
		}
	})
	opts.SetConnectionLostHandler(func(_ mqtt.Client, err error) {
		p.log.Warn("mqtt connection lost", "err", err)
	})

	p.cli = mqtt.NewClient(opts)
	tok := p.cli.Connect()
	if !tok.WaitTimeout(15 * time.Second) {
		return nil, fmt.Errorf("timeout connecting to mqtt broker %s:%d", cfg.MQTT.Host, cfg.MQTT.Port)
	}
	if err := tok.Error(); err != nil {
		return nil, fmt.Errorf("mqtt connect: %w", err)
	}
	return p, nil
}

func (p *MQTTPublisher) availabilityTopic() string { return p.base + "/availability" }
func (p *MQTTPublisher) stateTopic() string        { return p.base + "/state" }

func (p *MQTTPublisher) device() map[string]any {
	return map[string]any{
		"identifiers":  []string{"whatsapp_typing_" + p.cfg.Slug},
		"name":         fmt.Sprintf("WhatsApp %s", p.cfg.Name),
		"manufacturer": "whatsmeow",
		"model":        "WhatsApp typing sensor",
		"sw_version":   version,
	}
}

func (p *MQTTPublisher) publishDiscovery() error {
	for _, e := range entities() {
		uid := fmt.Sprintf("whatsapp_typing_%s_%s", p.cfg.Slug, e.Key)
		cfgTopic := fmt.Sprintf("%s/%s/whatsapp_typing_%s/%s/config",
			p.cfg.DiscoveryPrefix, e.Kind, p.cfg.Slug, e.Key)

		payload := map[string]any{
			"name":                  e.Name,
			"unique_id":             uid,
			"object_id":             fmt.Sprintf("%s_%s", p.cfg.Slug, e.Key),
			"state_topic":           p.stateTopic(),
			"value_template":        e.Template,
			"availability_topic":    p.availabilityTopic(),
			"payload_available":     "online",
			"payload_not_available": "offline",
			"device":                p.device(),
		}
		if e.Kind == "binary_sensor" {
			payload["payload_on"] = "ON"
			payload["payload_off"] = "OFF"
		}
		if e.Icon != "" {
			payload["icon"] = e.Icon
		}
		if e.DeviceClass != "" {
			payload["device_class"] = e.DeviceClass
		}
		if e.StateClass != "" {
			payload["state_class"] = e.StateClass
		}
		if e.Unit != "" {
			payload["unit_of_measurement"] = e.Unit
		}
		if e.Attrs {
			payload["json_attributes_topic"] = p.stateTopic()
			payload["json_attributes_template"] = "{{ value_json.attributes | tojson }}"
		}

		body, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		if err := p.publish(cfgTopic, body, true); err != nil {
			return err
		}
	}
	p.log.Info("mqtt discovery published", "entities", len(entities()), "prefix", p.cfg.DiscoveryPrefix)
	return nil
}

func (p *MQTTPublisher) PublishState(s State) error {
	body, err := statePayload(s)
	if err != nil {
		return err
	}
	if err := p.publish(p.stateTopic(), body, true); err != nil {
		return err
	}
	avail := "offline"
	if s.Available {
		avail = "online"
	}
	return p.publish(p.availabilityTopic(), []byte(avail), true)
}

func (p *MQTTPublisher) publish(topic string, payload []byte, retain bool) error {
	tok := p.cli.Publish(topic, 1, retain, payload)
	if !tok.WaitTimeout(10 * time.Second) {
		return fmt.Errorf("timeout publishing to %s", topic)
	}
	return tok.Error()
}

func (p *MQTTPublisher) Close() {
	// Best effort: tell HA we're gone before dropping the connection, otherwise
	// the retained LWT would only fire on an unclean disconnect.
	_ = p.publish(p.availabilityTopic(), []byte("offline"), true)
	p.cli.Disconnect(500)
}
