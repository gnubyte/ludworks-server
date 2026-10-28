package probe

import (
	"context"
	"os/exec"
)

// PCMCommand prepares (does not start) an ffmpeg process that decodes the first audio stream
// of input from startSeconds to raw signed 16-bit little-endian, 2-channel, 44100 Hz
// interleaved PCM on stdout — what AirPlay audio senders stream to receivers in real time.
// Video, subtitle and data streams are dropped. The caller sets cmd.Stdout (or uses
// StdoutPipe), starts it, and cancels ctx to stop it; the process is killed on cancellation.
func PCMCommand(ctx context.Context, ffmpeg, input string, startSeconds float64) (*exec.Cmd, error) {
	if err := checkExe(ffmpeg); err != nil {
		return nil, err
	}
	if err := checkInput(input); err != nil {
		return nil, err
	}
	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error"}
	if startSeconds > 0 {
		args = append(args, "-ss", secondsArg(startSeconds))
	}
	args = append(args, inputArgs(input)...)
	args = append(args, "-map", "0:a:0", "-vn", "-sn", "-dn",
		"-ac", "2", "-ar", "44100", "-c:a", "pcm_s16le", "-f", "s16le", "pipe:1")
	return newCmd(ctx, ffmpeg, args), nil
}
