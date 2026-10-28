package cast

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"ludworks/internal/cast/renderer"
	"ludworks/internal/settings"
)

// Settings is the "cast" settings namespace (API.md §3, §14).
type Settings struct {
	Enabled       bool           `json:"enabled"`
	Discovery     bool           `json:"discovery"`
	ManualDevices []ManualDevice `json:"manual_devices"`
	MediaPort     int            `json:"media_port"`
	MediaBaseURL  string         `json:"media_base_url"`
}

// ManualDevice is an admin-configured device (for networks where mDNS does not reach it).
// Port 0 selects the protocol's default port.
type ManualDevice struct {
	Kind string `json:"kind"` // renderer.KindChromecast | renderer.KindAirPlay
	Host string `json:"host"` // host name or IP address
	Port int    `json:"port,omitempty"`
	Name string `json:"name,omitempty"`
}

// DefaultSettings are the namespace defaults: casting off, discovery on once enabled.
var DefaultSettings = Settings{Discovery: true, ManualDevices: []ManualDevice{}}

// Validation limits.
const (
	maxManualDevices = 50
	maxDeviceName    = 100
	// Default ports of manual devices.
	defaultChromecastPort = 8009
	defaultAirPlayPort    = 7000
)

// defaultPort is the protocol port used for a manual device without one.
func defaultPort(kind string) int {
	if kind == renderer.KindAirPlay {
		return defaultAirPlayPort
	}
	return defaultChromecastPort
}

// listenPorts returns the TCP ports the server's own listeners use (the main listener and
// the HTTP→HTTPS redirect listener), which media_port must not collide with.
func listenPorts(addrs ...string) map[int]bool {
	out := map[int]bool{}
	for _, a := range addrs {
		if a == "" {
			continue
		}
		if _, p, err := net.SplitHostPort(a); err == nil {
			if n, err := strconv.Atoi(p); err == nil && n > 0 {
				out[n] = true
			}
		}
	}
	return out
}

// validateSettings normalises s in place (kinds and hosts lower-cased, default ports filled
// in, trailing slashes dropped) and checks every field. reserved are the server's own ports.
func validateSettings(s *Settings, reserved map[int]bool) error {
	errs := settings.FieldErrors{}
	if s.ManualDevices == nil {
		s.ManualDevices = []ManualDevice{}
	}
	if len(s.ManualDevices) > maxManualDevices {
		errs["manual_devices"] = fmt.Sprintf("at most %d devices", maxManualDevices)
	}
	seen := map[string]bool{}
	for i := range s.ManualDevices {
		d := &s.ManualDevices[i]
		key := fmt.Sprintf("manual_devices.%d", i)
		d.Kind = strings.ToLower(strings.TrimSpace(d.Kind))
		d.Host = strings.ToLower(strings.TrimSuffix(strings.Trim(strings.TrimSpace(d.Host), "[]"), "."))
		d.Name = strings.TrimSpace(d.Name)
		switch {
		case d.Kind != renderer.KindChromecast && d.Kind != renderer.KindAirPlay:
			errs[key+".kind"] = "must be chromecast or airplay"
		case d.Host == "" || !settings.ValidHost(d.Host):
			errs[key+".host"] = "must be a host name or IP address"
		case d.Port < 0 || d.Port > 65535:
			errs[key+".port"] = "must be between 1 and 65535 (or empty for the default)"
		case !utf8.ValidString(d.Name) || settings.HasControl(d.Name) || utf8.RuneCountInString(d.Name) > maxDeviceName:
			errs[key+".name"] = fmt.Sprintf("must be at most %d characters without control characters", maxDeviceName)
		}
		if d.Port == 0 {
			d.Port = defaultPort(d.Kind)
		}
		id := d.Host + "|" + strconv.Itoa(d.Port)
		if seen[id] {
			errs[key+".host"] = "this device is listed twice"
		}
		seen[id] = true
	}
	switch {
	case s.MediaPort == 0:
	case s.MediaPort < 1024 || s.MediaPort > 65535:
		errs["media_port"] = "must be 0 (off) or between 1024 and 65535"
	case reserved[s.MediaPort]:
		errs["media_port"] = "is already used by the server's own listener"
	}
	s.MediaBaseURL = strings.TrimRight(strings.TrimSpace(s.MediaBaseURL), "/")
	if s.MediaBaseURL != "" {
		u, err := url.Parse(s.MediaBaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
			u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || settings.HasControl(s.MediaBaseURL) ||
			len(s.MediaBaseURL) > 2048 {
			errs["media_base_url"] = "must be an absolute http(s) URL without path, query or fragment (e.g. https://nas.example.com)"
		}
	}
	return errs.Err()
}

// settings returns the effective cast settings (defaults when they cannot be read).
func (m *Module) settings(ctx context.Context) Settings {
	st := DefaultSettings
	if err := m.d.Settings.Get(ctx, "cast", &st); err != nil {
		m.log.Warn("cast: reading settings failed", "err", err)
		return DefaultSettings
	}
	return st
}
