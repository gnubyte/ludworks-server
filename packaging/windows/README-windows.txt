Ludworks for Windows
====================

Ludworks is a self-hosted, security-first NAS multitool: files with resumable
uploads, virtual TV channels, notes, an end-to-end-encrypted password vault and
spreadsheets, in one program with a web UI.

Requirements: 64-bit Windows 10/11 or Windows Server 2019 or later.
Documentation: https://ludworks.readthedocs.io


Contents of this archive
------------------------

  ludworks.exe            the server (single self-contained program)
  install-service.ps1     installs or upgrades the Windows service
  uninstall-service.ps1   removes the service and program files (keeps your data)
  config.example.json     example configuration file
  README-windows.txt      this file
  LICENSE                 license terms

Prefer a wizard? Use ludworks_<version>_windows_amd64_setup.exe from the same
release instead; it does the same as install-service.ps1.


Install as a Windows service (recommended)
------------------------------------------

1. Extract the archive, then open PowerShell as administrator
   (right-click > "Run as administrator") in the extracted folder.

2. Run:

     powershell -ExecutionPolicy Bypass -File .\install-service.ps1

   Add -AddFirewallRule to allow other devices on your local network to
   connect (inbound TCP 8443, only from addresses on the same local subnet;
   see "Firewall" below).

   The script copies the program to "C:\Program Files\Ludworks", registers the
   "Ludworks" service (it runs as the least-privilege virtual account
   NT SERVICE\Ludworks, starts automatically and restarts after a crash) and
   starts it. Running it again upgrades an existing installation in place.
   Ludworks always runs from "C:\Program Files\Ludworks", a folder only
   administrators can change; an existing Ludworks service registered for a
   program in any other folder is refused (the message explains how to remove
   it).

3. Read the one-time setup token in the same elevated window:

     Get-Content C:\ProgramData\Ludworks\setup-token.txt

4. Open https://localhost:8443/ in a browser. The server uses a self-signed
   certificate: compare its SHA-256 fingerprint with the one in
   C:\ProgramData\Ludworks\ludworks.log, then create the first administrator
   with the setup token.


Try it in a console instead
---------------------------

  .\ludworks.exe serve

The setup token is printed in the console. Stop the server with Ctrl+C. Use a
different data folder or port with, for example:

  .\ludworks.exe serve --data D:\Ludworks --listen :9443


Data and configuration
----------------------

  C:\ProgramData\Ludworks              data folder (database, files, keys, TLS
                                       certificate, backups); only SYSTEM,
                                       Administrators and the service can read it
  C:\ProgramData\Ludworks\config.json  optional configuration file; start from
                                       config.example.json and restart the service
                                       (Restart-Service Ludworks) after changes
  C:\ProgramData\Ludworks\ludworks.log service log

Back up the data folder regularly; secret.key must be backed up together with
ludworks.db.


Firewall
--------

The rule "Ludworks (HTTPS 8443)" (installer task or -AddFirewallRule) allows
inbound TCP 8443 to ludworks.exe only from the local subnet (the LAN's IPv4
subnet and IPv6 prefixes) on private and domain networks. Upgrades keep it
limited to the local subnet. To allow other networks on purpose, for example
a VPN such as Tailscale (100.64.0.0/10), add a separate rule of your own in an
elevated PowerShell:

  New-NetFirewallRule -DisplayName 'Ludworks (HTTPS 8443, VPN)' `
    -Direction Inbound -Action Allow -Protocol TCP -LocalPort 8443 `
    -Program 'C:\Program Files\Ludworks\ludworks.exe' `
    -RemoteAddress 100.64.0.0/10 -Profile Private, Domain

Upgrades and uninstalls leave rules with other names alone; remove it with
Remove-NetFirewallRule -DisplayName 'Ludworks (HTTPS 8443, VPN)'.


Uninstall
---------

  powershell -ExecutionPolicy Bypass -File "C:\Program Files\Ludworks\uninstall-service.ps1"

This removes the service, the firewall rule and the program files. Your data in
C:\ProgramData\Ludworks is kept; delete that folder yourself to remove it.


Command reference
-----------------

  ludworks.exe serve [--data DIR] [--listen ADDR] [--tls MODE] [--config FILE]
  ludworks.exe version
  ludworks.exe service install|uninstall|start|stop
  ludworks.exe admin reset-password <username>
  ludworks.exe admin create-setup-token

Run "service" and "admin" commands from an elevated prompt.
