//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"ludworks/internal/config"
)

const (
	serviceName    = "Ludworks"
	serviceAccount = `NT SERVICE\` + serviceName // per-service virtual account (least privilege)
)

func isWindowsService() bool {
	ok, err := svc.IsWindowsService()
	return err == nil && ok
}

// runAsService runs `serve` under the service control manager. Services have no console, so
// logs go to <data>\ludworks.log (rotated once at 10 MiB on start).
func runAsService(cfg *config.Config) int {
	if err := cfg.EnsureDirs(); err != nil {
		return 1
	}
	logPath := filepath.Join(cfg.DataDir, "ludworks.log")
	if st, err := os.Stat(logPath); err == nil && st.Size() > 10<<20 {
		_ = os.Rename(logPath, logPath+".1")
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	var w io.Writer = io.Discard
	if err == nil {
		defer f.Close()
		w = f
	}
	log := newLogger(cfg, w)
	if err := svc.Run(serviceName, &serviceHandler{cfg: cfg, log: log}); err != nil {
		log.Error("service failed", "err", err)
		return 1
	}
	return 0
}

type serviceHandler struct {
	cfg *config.Config
	log *slog.Logger
}

func (h *serviceHandler) Execute(_ []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serve(ctx, h.cfg, h.log) }()
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending, WaitHint: 30_000}
				cancel()
				if err := <-done; err != nil {
					h.log.Error("server stopped with error", "err", err)
					return true, 1
				}
				return false, 0
			}
		case err := <-done:
			if err != nil {
				h.log.Error("server stopped with error", "err", err)
				return true, 1
			}
			return false, 0
		}
	}
}

func manageService(action string, cfg *config.Config, f config.Flags, out io.Writer) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to service manager (run as Administrator): %w", err)
	}
	defer m.Disconnect()
	switch action {
	case "install":
		return installService(m, cfg, f, out)
	case "uninstall":
		s, err := m.OpenService(serviceName)
		if err != nil {
			return fmt.Errorf("service %s is not installed", serviceName)
		}
		defer s.Close()
		_ = stopService(s)
		if err := s.Delete(); err != nil {
			return err
		}
		fmt.Fprintf(out, "Service %s removed. The data directory was left untouched.\n", serviceName)
		return nil
	case "start":
		s, err := m.OpenService(serviceName)
		if err != nil {
			return fmt.Errorf("service %s is not installed", serviceName)
		}
		defer s.Close()
		if err := s.Start(); err != nil {
			return err
		}
		fmt.Fprintf(out, "Service %s started.\n", serviceName)
		return nil
	case "stop":
		s, err := m.OpenService(serviceName)
		if err != nil {
			return fmt.Errorf("service %s is not installed", serviceName)
		}
		defer s.Close()
		if err := stopService(s); err != nil {
			return err
		}
		fmt.Fprintf(out, "Service %s stopped.\n", serviceName)
		return nil
	default:
		return fmt.Errorf("unknown service action %q (install, uninstall, start, stop)", action)
	}
}

func installService(m *mgr.Mgr, cfg *config.Config, f config.Flags, out io.Writer) error {
	if s, err := m.OpenService(serviceName); err == nil {
		s.Close()
		return fmt.Errorf("service %s is already installed", serviceName)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.Abs(exe); err != nil {
		return err
	}
	args := []string{"serve", "--data", cfg.DataDir}
	if f.ConfigFile != "" {
		abs, err := filepath.Abs(f.ConfigFile)
		if err != nil {
			return err
		}
		args = append(args, "--config", abs)
	}
	if f.Listen != "" {
		args = append(args, "--listen", f.Listen)
	}
	if f.TLSMode != "" {
		args = append(args, "--tls", f.TLSMode)
	}
	s, err := m.CreateService(serviceName, exe, mgr.Config{
		DisplayName:      "Ludworks",
		Description:      "Ludworks self-hosted NAS server (files, TV, notes, vault, sheets).",
		StartType:        mgr.StartAutomatic,
		DelayedAutoStart: true,
		ServiceStartName: serviceAccount,
	}, args...)
	if err != nil {
		return fmt.Errorf("create service: %w", err)
	}
	defer s.Close()
	_ = s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 2 * time.Minute},
	}, 24*60*60)

	// The virtual account exists once the service does; give it (and only it, SYSTEM and
	// Administrators) access to the data directory.
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return err
	}
	sid, _, _, err := windows.LookupSID("", serviceAccount)
	if err != nil {
		_ = s.Delete()
		return fmt.Errorf("look up %s: %w", serviceAccount, err)
	}
	if err := config.RestrictDirTo(cfg.DataDir, sid.String()); err != nil {
		_ = s.Delete()
		return fmt.Errorf("set data directory permissions: %w", err)
	}
	fmt.Fprintf(out, "Service %s installed (account %s, data %s).\nStart it with: ludworks service start\n",
		serviceName, serviceAccount, cfg.DataDir)
	return nil
}

func stopService(s *mgr.Service) error {
	st, err := s.Query()
	if err != nil {
		return err
	}
	if st.State == svc.Stopped {
		return nil
	}
	if _, err := s.Control(svc.Stop); err != nil {
		return err
	}
	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(300 * time.Millisecond)
		if st, err = s.Query(); err != nil {
			return err
		}
		if st.State == svc.Stopped {
			return nil
		}
	}
	return errors.New("timed out waiting for the service to stop")
}
