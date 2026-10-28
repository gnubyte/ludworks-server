package airplay

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"testing"

	"ludworks/internal/cast/renderer"
)

func ep(service string, txt map[string]string) renderer.Endpoint {
	return renderer.Endpoint{Service: service, Host: "127.0.0.1", Port: 7000, TXT: txt}
}

func TestDescribe(t *testing.T) {
	homePodAP := map[string]string{"features": "0x4A7FCA00,0x3C356BD0", "flags": "0x18644", "model": "AudioAccessory5,1"}
	cases := []struct {
		name string
		eps  []renderer.Endpoint
		want renderer.Capabilities
	}{
		{"HomePod mini", []renderer.Endpoint{ep(renderer.ServiceRAOP, homePodTXT), ep(renderer.ServiceAirPlay, homePodAP)},
			renderer.Capabilities{Audio: true}},
		{"Apple TV 4K", []renderer.Endpoint{ep(renderer.ServiceRAOP, map[string]string{"et": "0,3,5", "cn": "0,1,2,3", "md": "0,1,2", "sf": "0x30644", "am": "AppleTV6,2"}),
			ep(renderer.ServiceAirPlay, appleTV4KTXT)},
			renderer.Capabilities{Audio: true, Video: true, RequiresPairing: true, PairingSupported: true}},
		{"Apple TV allowing everyone", []renderer.Endpoint{ep(renderer.ServiceAirPlay, openAppleTVTXT)},
			renderer.Capabilities{Video: true, PairingSupported: true}},
		{"Apple TV 3", []renderer.Endpoint{ep(renderer.ServiceAirPlay, legacyATVTXT)}, renderer.Capabilities{Video: true}},
		{"AirPort Express", []renderer.Endpoint{ep(renderer.ServiceRAOP, map[string]string{"et": "0,1", "ek": "1", "cn": "0,1", "tp": "TCP,UDP"})},
			renderer.Capabilities{Audio: true}},
		{"shairport-sync", []renderer.Endpoint{ep(renderer.ServiceRAOP, map[string]string{"et": "0,1", "cn": "0,1", "md": "0,1,2", "am": "ShairportSync", "sr": "44100", "ss": "16", "ch": "2"})},
			renderer.Capabilities{Audio: true}},
		{"AAC only", []renderer.Endpoint{ep(renderer.ServiceRAOP, map[string]string{"et": "0", "cn": "2,3"})}, renderer.Capabilities{}},
		{"48 kHz only", []renderer.Endpoint{ep(renderer.ServiceRAOP, map[string]string{"et": "0", "sr": "48000"})}, renderer.Capabilities{}},
		{"TCP only", []renderer.Endpoint{ep(renderer.ServiceRAOP, map[string]string{"et": "0", "tp": "TCP"})}, renderer.Capabilities{}},
		{"password TV", []renderer.Endpoint{ep(renderer.ServiceAirPlay, map[string]string{"features": "0x4A7FFFF7,0x3C155FDE", "flags": "0x80", "pw": "true"})},
			renderer.Capabilities{Video: true}},
		{"Home members only", []renderer.Endpoint{ep(renderer.ServiceAirPlay, map[string]string{"features": "0x4A7FFFF7,0x3C155FDE", "flags": "0x44", "acl": "1"})},
			renderer.Capabilities{Video: true}},
		{"no endpoints", nil, renderer.Capabilities{}},
	}
	d := New(Options{})
	for _, c := range cases {
		dev := &renderer.Device{Name: c.name, Endpoints: c.eps}
		if got := d.Describe(dev); got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
	if d.Kind() != renderer.KindAirPlay {
		t.Error("kind")
	}
}

func TestDeviceInfoParsing(t *testing.T) {
	for in, want := range map[string]uint64{
		"0x5A7FFFF7,0x1E": 0x1E_5A7FFFF7, "0x445F8A00,0x1C340": 0x1C340_445F8A00, "0x77": 0x77, "0X5A7FFFF7, 0XE": 0xE_5A7FFFF7,
	} {
		if got, ok := parseFeatures(in); !ok || got != want {
			t.Errorf("parseFeatures(%q) = %#x %v", in, got, ok)
		}
	}
	for _, bad := range []string{"", "0xZZ", "0x1,", "0x123456789"} {
		if _, ok := parseFeatures(bad); ok {
			t.Errorf("parseFeatures(%q) accepted", bad)
		}
	}
	cases := []struct {
		txt     map[string]string
		rsa, ok bool
	}{
		{map[string]string{}, false, true},
		{map[string]string{"et": "0,3,5"}, false, true},
		{map[string]string{"et": "0,1"}, false, true},
		{map[string]string{"et": "0,1", "ek": "1"}, true, true},
		{map[string]string{"et": "1"}, true, true},
		{map[string]string{"et": "3,5"}, false, false},
		{map[string]string{"et": "4"}, false, false},
	}
	for _, c := range cases {
		info := parseDevice(&renderer.Device{Endpoints: []renderer.Endpoint{ep(renderer.ServiceRAOP, c.txt)}})
		if rsa, ok := info.raopEncryption(); rsa != c.rsa || ok != c.ok {
			t.Errorf("%v: rsa %v ok %v", c.txt, rsa, ok)
		}
	}
	info := parseDevice(&renderer.Device{Endpoints: []renderer.Endpoint{ep(renderer.ServiceRAOP, map[string]string{"et": "0,4", "am": "AirPort10,115", "md": "0,2"})}})
	if !info.authSetupFirst() || !info.mdText || info.mdArt || !info.mdProgress {
		t.Errorf("AirPort Express info %+v", info)
	}
}

func TestConnectChecks(t *testing.T) {
	d := New(Options{})
	ctx := context.Background()
	cases := []struct {
		name string
		eps  []renderer.Endpoint
		want error
	}{
		{"nothing playable", []renderer.Endpoint{ep(renderer.ServiceRAOP, map[string]string{"cn": "2"})}, renderer.ErrUnsupported},
		{"password speaker", []renderer.Endpoint{ep(renderer.ServiceRAOP, map[string]string{"et": "0", "pw": "true"})}, renderer.ErrPairingUnsupported},
		{"FairPlay only", []renderer.Endpoint{ep(renderer.ServiceRAOP, map[string]string{"et": "3,5"})}, renderer.ErrPairingUnsupported},
	}
	for _, c := range cases {
		if _, err := d.Connect(ctx, &renderer.Device{Name: c.name, Endpoints: c.eps}, nil); !errors.Is(err, c.want) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	// Unreachable device.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	dev := &renderer.Device{Endpoints: []renderer.Endpoint{{Service: renderer.ServiceRAOP, Host: "127.0.0.1", Port: port, TXT: homePodTXT}}}
	if _, err := d.Connect(ctx, dev, nil); err == nil {
		t.Error("connected to a closed port")
	}
	// Pairing a HomePod (no screen for a PIN) is not offered.
	hp := &renderer.Device{Endpoints: []renderer.Endpoint{ep(renderer.ServiceAirPlay, map[string]string{"features": "0x4A7FCA00,0x3C356BD0"})}}
	if _, err := d.StartPairing(ctx, hp); !errors.Is(err, renderer.ErrPairingUnsupported) {
		t.Errorf("StartPairing on a HomePod: %v", err)
	}
}

func TestCredentialsEncoding(t *testing.T) {
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	devPK, _, _ := ed25519.GenerateKey(rand.Reader)
	c := &credentials{id: []byte("6A4C3F5E-0C8B-4A0F-9E1B-8D2B3C4D5E6F"), key: key, deviceID: []byte("AA:BB:CC:DD:EE:FF"), devicePK: devPK}
	b := c.marshal()
	got, err := parseCredentials(b)
	if err != nil || string(got.id) != string(c.id) || !got.key.Equal(c.key) || string(got.deviceID) != string(c.deviceID) {
		t.Fatalf("round trip: %v", err)
	}
	for i := range len(b) - 1 {
		if _, err := parseCredentials(b[:i]); err == nil {
			t.Fatalf("truncated to %d bytes accepted", i)
		}
	}
	bad := append([]byte{}, b...)
	bad[0] = 2
	if _, err := parseCredentials(bad); err == nil {
		t.Error("unknown version accepted")
	}
	if _, err := parseCredentials(append(b, 0)); err == nil {
		t.Error("trailing byte accepted")
	}
}
