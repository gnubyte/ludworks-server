//go:build !windows

package cast

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ludworks/internal/cast/renderer"
	"ludworks/internal/files"
	"ludworks/internal/music"
)

// pathMedia reports a fixed ffmpeg binary.
type pathMedia struct {
	mediaService
	path string
}

func (p pathMedia) FFmpegPath(context.Context) string { return p.path }

// fakeFFmpeg prints its arguments then a PCM marker, then waits (killed by Close).
const fakeFFmpeg = `#!/bin/sh
printf 'ARGS:%s\n' "$*"
printf 'PCMDATA'
exec sleep 30
`

func TestPCMSources(t *testing.T) {
	v := newEnv(t)
	ctx := context.Background()
	p := v.principal(v.adminID)
	space, err := v.files.HomeSpaceID(ctx, v.adminID)
	if err != nil {
		t.Fatal(err)
	}
	add := func(name string, content []byte) string {
		e, err := v.files.CreateFromReader(ctx, p, space, "", name, strings.NewReader(string(content)), files.CreateOpts{})
		if err != nil {
			t.Fatal(err)
		}
		return e.ID
	}
	samples := make([]byte, 4*pcmRate)
	for i := range samples {
		samples[i] = byte(i % 251)
	}
	wavID := add("tone.wav", wav(1, 2, 44100, 16, samples))
	mp3ID := add("song.mp3", []byte("ID3 not really an mp3"))
	s := &session{m: v.m, owner: owner{UserID: v.adminID, Scopes: []string{"*"}}, ctx: ctx}

	// Without ffmpeg: ready-to-play WAV is read natively, other formats cannot be decoded.
	if src := v.m.pcmSource(s, mp3ID, &music.TrackInfo{Mime: "audio/mpeg", Codec: "mp3", Container: "mp3"}); src != nil {
		t.Fatal("PCM source without a decoder")
	}
	src := v.m.pcmSource(s, wavID, &music.TrackInfo{Mime: "audio/wav", Container: "wav"})
	if src == nil {
		t.Fatal("no native WAV source")
	}
	rc, err := src(ctx, 0.5)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if len(b) != len(samples)/2 || b[0] != samples[len(samples)/2] {
		t.Fatalf("native WAV from 0.5 s: %d bytes", len(b))
	}

	// With ffmpeg: decoded by ffmpeg from the start position; Close stops the process.
	script := filepath.Join(t.TempDir(), "ffmpeg")
	if err := os.WriteFile(script, []byte(fakeFFmpeg), 0o755); err != nil {
		t.Fatal(err)
	}
	v.m.media = pathMedia{v.m.media, script}
	src = v.m.pcmSource(s, mp3ID, &music.TrackInfo{Mime: "audio/mpeg", Codec: "mp3", Container: "mp3"})
	if src == nil {
		t.Fatal("no ffmpeg source")
	}
	rc, err = src(ctx, 12.5)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4096)
	var got strings.Builder
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(got.String(), "PCMDATA") && time.Now().Before(deadline) {
		n, err := rc.Read(buf)
		got.Write(buf[:n])
		if err != nil {
			break
		}
	}
	out := got.String()
	for _, want := range []string{"-ss 12.500", "-f s16le", "-ar 44100", "-ac 2", "-protocol_whitelist file", "PCMDATA"} {
		if !strings.Contains(out, want) {
			t.Errorf("ffmpeg output %q lacks %q", out, want)
		}
	}
	done := make(chan struct{})
	go func() { rc.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not stop ffmpeg")
	}

	// An owner who is gone cannot open the source any more.
	gone := &session{m: v.m, owner: owner{UserID: "nosuchuser00000000000"}, ctx: ctx}
	if _, err := v.m.pcmSource(gone, wavID, &music.TrackInfo{Container: "wav"})(ctx, 0); !errors.Is(err, errItemGone) {
		t.Fatalf("source for a missing owner: %v", err)
	}
	// Without ffmpeg and a WAV in another format: unsupported.
	v.m.media = pathMedia{v.m.media, ""}
	monoID := add("mono.wav", wav(1, 1, 44100, 16, samples))
	if _, err := v.m.pcmSource(s, monoID, &music.TrackInfo{Container: "wav"})(ctx, 0); !errors.Is(err, renderer.ErrUnsupported) {
		t.Fatalf("mono WAV without ffmpeg: %v", err)
	}
}
