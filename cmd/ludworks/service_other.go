//go:build !windows

package main

import (
	"errors"
	"fmt"
	"io"

	"ludworks/internal/config"
)

func isWindowsService() bool { return false }

func runAsService(*config.Config) int { return 1 }

const systemdHelp = `On Linux, Ludworks runs under systemd (installed by the .deb package):
  sudo systemctl enable --now ludworks     start now and at boot
  sudo systemctl stop ludworks             stop
  sudo systemctl status ludworks           status
  sudo journalctl -u ludworks -f           follow logs
Configuration: /etc/ludworks/config.json   Data: /var/lib/ludworks
Admin commands must run as the service user, e.g.:
  sudo -u ludworks ludworks admin create-setup-token
`

func manageService(action string, _ *config.Config, _ config.Flags, out io.Writer) error {
	fmt.Fprint(out, systemdHelp)
	return errors.New("the service command manages the Windows service only")
}
