// Command ludworks is the Ludworks server.
//
//	ludworks serve [--data DIR] [--listen ADDR] [--tls MODE] [--config FILE] [--log-level LEVEL]
//	ludworks version
//	ludworks admin reset-password <username> [config flags]   (new password read from stdin)
//	ludworks admin create-setup-token [config flags]
//	ludworks service install|uninstall|start|stop [config flags]   (Windows)
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"

	"ludworks/internal/app"
	"ludworks/internal/auth"
	"ludworks/internal/config"
	"ludworks/internal/version"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

const usage = `Usage:
  ludworks serve [flags]                      run the server
  ludworks version                            print the version
  ludworks admin reset-password <username>    set a user's password (read from stdin)
  ludworks admin create-setup-token           create a new one-time setup token
  ludworks service install|uninstall|start|stop   manage the Windows service

Flags (serve, admin, service):
  --data DIR        data directory
  --listen ADDR     listen address (default :8443)
  --tls MODE        self-signed | files | acme | off
  --config FILE     JSON config file
  --log-level LVL   debug | info | warn | error
`

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "serve":
		return cmdServe(args[1:], stderr)
	case "version", "--version", "-version":
		fmt.Fprintf(stdout, "ludworks %s (%s %s/%s)\n", version.Version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
		return 0
	case "admin":
		return cmdAdmin(args[1:], stdout, stderr)
	case "service":
		return cmdService(args[1:], stdout, stderr)
	case "help", "-h", "--help", "-help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

// parseFlags parses config flags that may appear before or after positional arguments
// ("admin reset-password --data X bob" and "admin reset-password bob --data X").
func parseFlags(name string, args []string, stderr io.Writer) (config.Flags, []string, error) {
	var f config.Flags
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	f.Register(fs)
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return f, nil, err
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
	return f, positional, nil
}

func loadConfig(name string, args []string, stderr io.Writer) (*config.Config, config.Flags, []string, bool) {
	f, pos, err := parseFlags(name, args, stderr)
	if err != nil {
		return nil, f, nil, false
	}
	cfg, err := config.Load(f)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return nil, f, nil, false
	}
	return cfg, f, pos, true
}

func newLogger(cfg *config.Config, w io.Writer) *slog.Logger {
	var lvl slog.Level
	_ = lvl.UnmarshalText([]byte(cfg.Log.Level))
	opts := &slog.HandlerOptions{Level: lvl}
	var h slog.Handler = slog.NewTextHandler(w, opts)
	if cfg.Log.Format == "json" {
		h = slog.NewJSONHandler(w, opts)
	}
	l := slog.New(h)
	slog.SetDefault(l)
	return l
}

func cmdServe(args []string, stderr io.Writer) int {
	cfg, _, pos, ok := loadConfig("serve", args, stderr)
	if !ok {
		return 2
	}
	if len(pos) > 0 {
		fmt.Fprintf(stderr, "serve: unexpected argument %q\n", pos[0])
		return 2
	}
	if isWindowsService() {
		return runAsService(cfg)
	}
	log := newLogger(cfg, stderr)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := serve(ctx, cfg, log); err != nil {
		log.Error("server stopped with error", "err", err)
		return 1
	}
	return 0
}

// serve builds and runs the application until ctx is cancelled.
func serve(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	a, err := app.New(ctx, cfg, log)
	if err != nil {
		return err
	}
	return a.Run(ctx)
}

func cmdAdmin(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	sub := args[0]
	cfg, _, pos, ok := loadConfig("admin "+sub, args[1:], stderr)
	if !ok {
		return 2
	}
	log := newLogger(cfg, stderr)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch sub {
	case "reset-password":
		if len(pos) != 1 {
			fmt.Fprintln(stderr, "usage: ludworks admin reset-password <username>")
			return 2
		}
		pw, err := readNewPassword(os.Stdin, stderr)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		d, closeFn, err := app.OpenCLI(ctx, cfg, log)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		defer closeFn()
		if err := auth.ResetPasswordCLI(ctx, d, pos[0], pw); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		fmt.Fprintf(stdout, "Password for %s updated; all of their sessions were revoked.\n", pos[0])
		return 0
	case "create-setup-token":
		if len(pos) != 0 {
			fmt.Fprintln(stderr, "usage: ludworks admin create-setup-token")
			return 2
		}
		d, closeFn, err := app.OpenCLI(ctx, cfg, log)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		defer closeFn()
		tok, err := auth.CreateSetupTokenCLI(ctx, d)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		fmt.Fprintln(stdout, tok)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown admin command %q\n\n%s", sub, usage)
		return 2
	}
}

// maxPasswordBytes matches the server-side limit (ARCHITECTURE.md §2.3).
const maxPasswordBytes = 1024

var errPasswordMismatch = errors.New("passwords do not match")

// readNewPassword prompts twice without echo on a terminal, otherwise reads one line from
// stdin (for scripted use: printf '%s\n' "$PW" | ludworks admin reset-password bob).
func readNewPassword(in *os.File, prompt io.Writer) (string, error) {
	if isTerminal(in) {
		fmt.Fprint(prompt, "New password: ")
		p1, err := readPasswordNoEcho(in)
		fmt.Fprintln(prompt)
		if err != nil {
			return "", err
		}
		fmt.Fprint(prompt, "Repeat new password: ")
		p2, err := readPasswordNoEcho(in)
		fmt.Fprintln(prompt)
		if err != nil {
			return "", err
		}
		if p1 != p2 {
			return "", errPasswordMismatch
		}
		return checkPassword(p1)
	}
	line, err := readLine(in)
	if err != nil {
		return "", err
	}
	return checkPassword(line)
}

func checkPassword(p string) (string, error) {
	if p == "" {
		return "", errors.New("empty password")
	}
	if len(p) > maxPasswordBytes {
		return "", fmt.Errorf("password longer than %d bytes", maxPasswordBytes)
	}
	return p, nil
}

// readLine reads a single line (without the trailing CR/LF) byte by byte, so nothing beyond
// the line is consumed and the length is bounded.
func readLine(r io.Reader) (string, error) {
	var b strings.Builder
	buf := make([]byte, 1)
	for b.Len() <= maxPasswordBytes+2 {
		n, err := r.Read(buf)
		if n == 1 {
			if buf[0] == '\n' {
				break
			}
			b.WriteByte(buf[0])
			continue
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
	}
	return strings.TrimSuffix(b.String(), "\r"), nil
}

func cmdService(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	sub := args[0]
	cfg, f, pos, ok := loadConfig("service "+sub, args[1:], stderr)
	if !ok {
		return 2
	}
	if len(pos) != 0 {
		fmt.Fprintf(stderr, "service %s: unexpected argument %q\n", sub, pos[0])
		return 2
	}
	if err := manageService(sub, cfg, f, stdout); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	return 0
}
