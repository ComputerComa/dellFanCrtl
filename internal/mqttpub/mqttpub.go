// Package mqttpub publishes sensor/fan state to an MQTT broker with Home
// Assistant MQTT discovery, so a running dellfanctl shows up as a device
// with per-sensor entities automatically, no HA-side configuration needed.
//
// Everything here is designed to never block the control loop: Connect
// uses paho's ConnectRetry/AutoReconnect so a slow or unreachable broker
// never delays startup or a tick, and PublishSnapshot is meant to be called
// via `go` by the caller (see internal/control) so even a momentarily slow
// Publish call can't stall sensor polling or fan control.
package mqttpub

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"dellfanctl/internal/model"
	"dellfanctl/internal/version"
)

// SensorReading is one sensor's value at publish time.
type SensorReading struct {
	ID       string
	Value    float64
	HasValue bool
	Stale    bool
}

// GroupReading is one group's aggregated value at publish time.
type GroupReading struct {
	Name     string
	Value    float64
	HasValue bool
}

// Snapshot is everything PublishSnapshot needs for one tick. It's a plain
// value with no references into mutable controller state, so it's safe to
// hand to the publisher goroutine without synchronization.
type Snapshot struct {
	Time          time.Time
	Sensors       []SensorReading
	Groups        []GroupReading
	FanPercent    float64
	HasFanPercent bool
	AvgTemp       float64
	HasAvgTemp    bool
	Mode          string // "manual" or "fallback"
	Fallback      bool   // mirrors Mode == "fallback", as a proper bool for the binary_sensor below
	// FallbackReason is why safetyTrip() fired, e.g. "required sensor
	// \"Disk 9 Temp\" unreadable for 6 ticks". Empty when Fallback is
	// false.
	FallbackReason string
}

type topicPair struct{ state, attrs string }

// Publisher owns the MQTT connection and topic layout.
type Publisher struct {
	cfg model.MQTT
	log *slog.Logger

	node string
	base string

	client mqtt.Client

	mu           sync.RWMutex
	sensorTopics map[string]topicPair
	groupTopics  map[string]topicPair
}

var slugRe = regexp.MustCompile(`[^a-z0-9_-]+`)

func slug(s string) string {
	s = slugRe.ReplaceAllString(strings.ToLower(s), "_")
	if s == "" {
		return "x"
	}
	return s
}

// New builds a Publisher from config. It does not connect; call Start for
// that.
func New(cfg model.MQTT, log *slog.Logger) *Publisher {
	if log == nil {
		log = slog.Default()
	}
	node := cfg.NodeID
	if node == "" {
		if h, err := os.Hostname(); err == nil && h != "" {
			node = h
		} else {
			node = "dellfanctl"
		}
	}
	node = slug(node)
	base := cfg.BaseTopic
	if base == "" {
		base = "dellfanctl/" + node
	}
	return &Publisher{cfg: cfg, log: log, node: node, base: base}
}

func (p *Publisher) qos() byte                 { return p.cfg.QoS }
func (p *Publisher) availabilityTopic() string { return p.base + "/status" }

// Start connects to the broker. Connection (and reconnection) happens in
// the background via paho's ConnectRetry/AutoReconnect regardless of
// whether the initial attempt succeeds within the short window this waits
// for, so a broker that's slow or briefly down never blocks the caller
// (see package docs).
func (p *Publisher) Start() error {
	opts := mqtt.NewClientOptions()
	opts.AddBroker(p.cfg.Broker)
	clientID := p.cfg.ClientID
	if clientID == "" {
		clientID = "dellfanctl"
	}
	opts.SetClientID(clientID + "-" + p.node)
	if p.cfg.Username != "" {
		opts.SetUsername(p.cfg.Username)
	}
	password := p.cfg.Password
	if p.cfg.PasswordEnv != "" {
		if v := os.Getenv(p.cfg.PasswordEnv); v != "" {
			password = v
		}
	}
	if password != "" {
		opts.SetPassword(password)
	}
	opts.SetOrderMatters(false)
	opts.SetAutoReconnect(true)
	opts.SetConnectRetry(true)
	opts.SetConnectRetryInterval(10 * time.Second)
	opts.SetConnectTimeout(5 * time.Second)
	opts.SetKeepAlive(30 * time.Second)
	opts.SetWill(p.availabilityTopic(), "offline", p.qos(), true)
	opts.SetOnConnectHandler(func(mqtt.Client) {
		p.log.Info("mqtt connected", "broker", p.cfg.Broker)
		p.publishRetained(p.availabilityTopic(), "online")
	})
	opts.SetConnectionLostHandler(func(_ mqtt.Client, err error) {
		p.log.Warn("mqtt connection lost; will reconnect automatically", "error", err)
	})
	opts.SetReconnectingHandler(func(mqtt.Client, *mqtt.ClientOptions) {
		p.log.Debug("mqtt reconnecting")
	})

	p.client = mqtt.NewClient(opts)
	token := p.client.Connect()
	// ConnectRetry means Connect() itself won't error out and keep trying
	// forever in the background; this brief wait is only so a healthy
	// broker's discovery publish (right after Start returns) doesn't race
	// a connection that completes a few milliseconds later.
	if !token.WaitTimeout(5 * time.Second) {
		p.log.Warn("mqtt broker not reachable yet; continuing to connect in the background", "broker", p.cfg.Broker)
	} else if err := token.Error(); err != nil {
		p.log.Warn("mqtt initial connect failed; continuing to retry in the background", "broker", p.cfg.Broker, "error", err)
	}
	return nil
}

// Close publishes a retained "offline" availability message (so Home
// Assistant reflects shutdown immediately rather than waiting for the
// broker to notice the dropped connection) and disconnects. Callers should
// have already waited for any in-flight PublishSnapshot/PublishDiscovery
// call to return (see Controller.Run) so this doesn't cut off publishes
// that are still queued.
func (p *Publisher) Close() {
	if p.client == nil {
		return
	}
	if p.client.IsConnectionOpen() {
		tok := p.client.Publish(p.availabilityTopic(), p.qos(), true, "offline")
		tok.WaitTimeout(2 * time.Second)
	}
	p.client.Disconnect(250)
}

func (p *Publisher) publishRetained(topic, payload string) mqtt.Token {
	if p.client == nil {
		return nil
	}
	return p.client.Publish(topic, p.qos(), true, payload)
}

func (p *Publisher) publishRetainedJSON(topic string, v interface{}) mqtt.Token {
	b, err := json.Marshal(v)
	if err != nil {
		p.log.Warn("mqtt: failed to encode json payload", "topic", topic, "error", err)
		return nil
	}
	return p.publishRetained(topic, string(b))
}

// waitAll waits for every token to complete (publish acknowledged, or at
// least handed to the network layer), up to an overall deadline shared
// across all of them. Publish() itself already queues messages without
// blocking; this wait only happens in callers that can afford it (a
// one-time startup discovery publish, or a per-tick publish already
// running in its own goroutine off the control loop's critical path) so
// that a shutdown right after doesn't disconnect mid-flush and silently
// drop retained messages.
func waitAll(tokens []mqtt.Token, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for _, t := range tokens {
		if t == nil {
			continue
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return
		}
		t.WaitTimeout(remaining)
	}
}

// classInfo maps a sensor class to its Home Assistant unit/device_class and
// whether it's a temperature (for the overall-average-temperature summary).
func classInfo(class model.SensorClass) (unit, deviceClass, entityCategory string, isTemp bool) {
	switch class {
	case model.ClassCPU, model.ClassInlet, model.ClassExhaust, model.ClassBoard, model.ClassDisk, model.ClassMemory:
		return "°C", "temperature", "", true
	case model.ClassFanRPM:
		return "RPM", "", "diagnostic", false
	case model.ClassPSU:
		return "", "", "diagnostic", false
	case model.ClassUtilization:
		return "%", "", "diagnostic", false
	case model.ClassPower:
		return "W", "power", "diagnostic", false
	default:
		return "", "", "", false
	}
}

type haDevice struct {
	Identifiers  []string `json:"identifiers"`
	Name         string   `json:"name"`
	Manufacturer string   `json:"manufacturer"`
	Model        string   `json:"model"`
	SWVersion    string   `json:"sw_version"`
}

type haDiscovery struct {
	Name                string   `json:"name"`
	UniqueID            string   `json:"unique_id"`
	StateTopic          string   `json:"state_topic"`
	AvailabilityTopic   string   `json:"availability_topic"`
	PayloadAvailable    string   `json:"payload_available"`
	PayloadNotAvailable string   `json:"payload_not_available"`
	UnitOfMeasurement   string   `json:"unit_of_measurement,omitempty"`
	DeviceClass         string   `json:"device_class,omitempty"`
	StateClass          string   `json:"state_class,omitempty"`
	JSONAttributesTopic string   `json:"json_attributes_topic,omitempty"`
	EntityCategory      string   `json:"entity_category,omitempty"`
	Icon                string   `json:"icon,omitempty"`
	PayloadOn           string   `json:"payload_on,omitempty"`
	PayloadOff          string   `json:"payload_off,omitempty"`
	Device              haDevice `json:"device"`
}

func (p *Publisher) device() haDevice {
	return haDevice{
		Identifiers:  []string{"dellfanctl_" + p.node},
		Name:         "Dell Fan Control (" + p.node + ")",
		Manufacturer: "Dell",
		Model:        "PowerEdge (dellfanctl)",
		SWVersion:    version.Version,
	}
}

func (p *Publisher) discoveryTopic(component, objectID string) string {
	prefix := p.cfg.DiscoveryPrefix
	if prefix == "" {
		prefix = "homeassistant"
	}
	return fmt.Sprintf("%s/%s/%s/%s/config", prefix, component, p.node, objectID)
}

// PublishDiscovery publishes one retained Home Assistant discovery config
// per sensor, per enabled group, and for the fan-percent/avg-temp/mode/
// last-update summary entities. It also records each entity's state/
// attributes topics so PublishSnapshot can find them later without
// recomputing or risking drift from what was announced here.
func (p *Publisher) PublishDiscovery(cfg *model.Config) {
	sensorTopics := make(map[string]topicPair)
	groupTopics := make(map[string]topicPair)
	dev := p.device()
	avail := p.availabilityTopic()
	var tokens []mqtt.Token

	publish := func(component, objectID string, d haDiscovery) {
		d.AvailabilityTopic = avail
		d.PayloadAvailable = "online"
		d.PayloadNotAvailable = "offline"
		d.Device = dev
		tokens = append(tokens, p.publishRetainedJSON(p.discoveryTopic(component, objectID), d))
	}

	for _, s := range cfg.Sensors {
		if s.Disabled {
			continue
		}
		unit, deviceClass, entityCategory, _ := classInfo(s.Class)
		stateTopic := fmt.Sprintf("%s/sensors/%s/%s/state", p.base, s.Class, s.ID)
		attrsTopic := fmt.Sprintf("%s/sensors/%s/%s/attributes", p.base, s.Class, s.ID)
		sensorTopics[s.ID] = topicPair{state: stateTopic, attrs: attrsTopic}
		publish("sensor", s.ID, haDiscovery{
			Name:                s.Name,
			UniqueID:            p.node + "_" + s.ID,
			StateTopic:          stateTopic,
			UnitOfMeasurement:   unit,
			DeviceClass:         deviceClass,
			StateClass:          stateClassFor(unit),
			JSONAttributesTopic: attrsTopic,
			EntityCategory:      entityCategory,
		})
	}

	for _, g := range cfg.Groups {
		if !g.Enabled {
			continue
		}
		key := "group_" + slug(g.Name)
		stateTopic := fmt.Sprintf("%s/groups/%s/state", p.base, slug(g.Name))
		attrsTopic := fmt.Sprintf("%s/groups/%s/attributes", p.base, slug(g.Name))
		groupTopics[g.Name] = topicPair{state: stateTopic, attrs: attrsTopic}
		publish("sensor", key, haDiscovery{
			Name:                fmt.Sprintf("%s Group (%s)", titleCase(g.Name), g.Aggregation),
			UniqueID:            p.node + "_" + key,
			StateTopic:          stateTopic,
			UnitOfMeasurement:   "°C",
			DeviceClass:         "temperature",
			StateClass:          "measurement",
			JSONAttributesTopic: attrsTopic,
		})
	}

	publish("sensor", "summary_fan_percent", haDiscovery{
		Name:              "Overall Fan Speed",
		UniqueID:          p.node + "_summary_fan_percent",
		StateTopic:        p.base + "/summary/fan_percent/state",
		UnitOfMeasurement: "%",
		StateClass:        "measurement",
		Icon:              "mdi:fan",
	})
	publish("sensor", "summary_avg_temp", haDiscovery{
		Name:              "Overall Average Temperature",
		UniqueID:          p.node + "_summary_avg_temp",
		StateTopic:        p.base + "/summary/avg_temp/state",
		UnitOfMeasurement: "°C",
		DeviceClass:       "temperature",
		StateClass:        "measurement",
	})
	publish("sensor", "summary_mode", haDiscovery{
		Name:           "Fan Control Mode",
		UniqueID:       p.node + "_summary_mode",
		StateTopic:     p.base + "/summary/mode/state",
		EntityCategory: "diagnostic",
		Icon:           "mdi:state-machine",
	})
	// A dedicated problem entity, not just the text summary_mode above:
	// device_class "problem" is what lets Home Assistant offer a one-click
	// "notify me" automation ("Device became problem") instead of someone
	// having to build their own template trigger off summary_mode's raw
	// "fallback" string - the entire point being that you find out *before*
	// you notice the fans, not after (see internal/control's
	// on_fallback_cmd for a non-HA notification path too).
	publish("binary_sensor", "summary_fallback", haDiscovery{
		Name:                "Safety Fallback",
		UniqueID:            p.node + "_summary_fallback",
		StateTopic:          p.base + "/summary/fallback/state",
		JSONAttributesTopic: p.base + "/summary/fallback/attributes",
		DeviceClass:         "problem",
		PayloadOn:           "ON",
		PayloadOff:          "OFF",
		Icon:                "mdi:fan-alert",
	})
	publish("sensor", "summary_last_update", haDiscovery{
		Name:           "Last Update",
		UniqueID:       p.node + "_summary_last_update",
		StateTopic:     p.base + "/summary/last_update/state",
		DeviceClass:    "timestamp",
		EntityCategory: "diagnostic",
	})

	p.mu.Lock()
	p.sensorTopics = sensorTopics
	p.groupTopics = groupTopics
	p.mu.Unlock()

	// This is a one-time startup call (before the ticker even starts), so
	// waiting here doesn't touch the control loop's cadence; it exists so
	// a config reload or quick restart right after doesn't race a
	// disconnect against still-queued discovery messages.
	waitAll(tokens, 5*time.Second)
}

func stateClassFor(unit string) string {
	if unit == "" {
		return ""
	}
	return "measurement"
}

func titleCase(s string) string {
	if s == "" {
		return s
	}
	b := []byte(s)
	if b[0] >= 'a' && b[0] <= 'z' {
		b[0] -= 'a' - 'A'
	}
	return string(b)
}

// PublishSnapshot publishes retained state (and a retained JSON attributes
// payload carrying last_updated/stale) for every reading in snap, plus the
// summary entities. Callers (see internal/control) invoke this via `go` so
// a slow/unreachable broker can never delay the control loop; it's safe for
// this to then wait (bounded) for its own publishes to actually reach the
// broker, which matters if the process exits shortly after (see
// Controller.Run, which waits for this to return before disconnecting).
func (p *Publisher) PublishSnapshot(snap Snapshot) {
	p.mu.RLock()
	sensorTopics := p.sensorTopics
	groupTopics := p.groupTopics
	p.mu.RUnlock()

	ts := snap.Time.Format(time.RFC3339)
	var tokens []mqtt.Token

	for _, s := range snap.Sensors {
		tp, ok := sensorTopics[s.ID]
		if !ok || !s.HasValue {
			continue
		}
		tokens = append(tokens, p.publishRetained(tp.state, strconv.FormatFloat(s.Value, 'f', 2, 64)))
		tokens = append(tokens, p.publishRetainedJSON(tp.attrs, map[string]interface{}{
			"last_updated": ts,
			"stale":        s.Stale,
		}))
	}
	for _, g := range snap.Groups {
		tp, ok := groupTopics[g.Name]
		if !ok || !g.HasValue {
			continue
		}
		tokens = append(tokens, p.publishRetained(tp.state, strconv.FormatFloat(g.Value, 'f', 2, 64)))
		tokens = append(tokens, p.publishRetainedJSON(tp.attrs, map[string]interface{}{"last_updated": ts}))
	}
	if snap.HasFanPercent {
		tokens = append(tokens, p.publishRetained(p.base+"/summary/fan_percent/state", strconv.FormatFloat(snap.FanPercent, 'f', 1, 64)))
	}
	if snap.HasAvgTemp {
		tokens = append(tokens, p.publishRetained(p.base+"/summary/avg_temp/state", strconv.FormatFloat(snap.AvgTemp, 'f', 2, 64)))
	}
	if snap.Mode != "" {
		tokens = append(tokens, p.publishRetained(p.base+"/summary/mode/state", snap.Mode))
	}
	fallbackPayload := "OFF"
	if snap.Fallback {
		fallbackPayload = "ON"
	}
	tokens = append(tokens, p.publishRetained(p.base+"/summary/fallback/state", fallbackPayload))
	tokens = append(tokens, p.publishRetainedJSON(p.base+"/summary/fallback/attributes", map[string]interface{}{
		"last_updated": ts,
		"reason":       snap.FallbackReason,
	}))
	tokens = append(tokens, p.publishRetained(p.base+"/summary/last_update/state", ts))

	waitAll(tokens, 3*time.Second)
}
