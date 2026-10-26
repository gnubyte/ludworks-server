// Command fakeimap runs the mailtest fake IMAP server for manual end-to-end tests of the
// admin IMAP test and mail-in against a real Ludworks server. It is a development tool and
// is never part of a release build.
//
//	fakeimap -listen 127.0.0.1:18186 -user inbox -pass secret -dir ./messages
//
// Every *.eml file in -dir (sorted by name) becomes an unseen message in INBOX. The state
// (which messages are \Seen) is printed when the process is interrupted.
package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"ludworks/internal/mail/mailtest"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:18186", "listen address (loopback only)")
	user := flag.String("user", "inbox", "IMAP username")
	pass := flag.String("pass", "", "IMAP password (required)")
	dir := flag.String("dir", "", "directory of .eml files to serve as unseen INBOX messages")
	starttls := flag.Bool("starttls", false, "advertise STARTTLS")
	implicit := flag.Bool("tls", false, "implicit TLS (self-signed certificate)")
	authPlain := flag.Bool("auth-plain", true, "advertise AUTH=PLAIN")
	boxes := flag.String("mailboxes", "Archive,Notes", "extra mailboxes (comma separated)")
	flag.Parse()
	if *pass == "" {
		fmt.Fprintln(os.Stderr, "fakeimap: -pass is required")
		os.Exit(2)
	}
	if host, _, err := net.SplitHostPort(*listen); err != nil || !net.ParseIP(host).IsLoopback() {
		fmt.Fprintln(os.Stderr, "fakeimap: -listen must be a loopback address")
		os.Exit(2)
	}
	var extra []string
	for _, b := range strings.Split(*boxes, ",") {
		if b = strings.TrimSpace(b); b != "" {
			extra = append(extra, b)
		}
	}
	s, err := mailtest.NewIMAP(mailtest.IMAPOptions{
		Addr: *listen, User: *user, Pass: *pass, STARTTLS: *starttls, ImplicitTLS: *implicit,
		AuthPlain: *authPlain, Mailboxes: extra,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "fakeimap:", err)
		os.Exit(1)
	}
	var uids []uint32
	if *dir != "" {
		names, err := filepath.Glob(filepath.Join(*dir, "*.eml"))
		if err != nil {
			fmt.Fprintln(os.Stderr, "fakeimap:", err)
			os.Exit(1)
		}
		slices.Sort(names)
		for _, n := range names {
			b, err := os.ReadFile(n)
			if err != nil {
				fmt.Fprintln(os.Stderr, "fakeimap:", err)
				os.Exit(1)
			}
			uid := s.Add("INBOX", b)
			uids = append(uids, uid)
			fmt.Printf("uid %d: %s (%d bytes)\n", uid, filepath.Base(n), len(b))
		}
	}
	fmt.Printf("fake IMAP listening on %s:%d\n", s.Host, s.Port)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	for _, u := range uids {
		fmt.Printf("uid %d seen=%v\n", u, s.Seen("INBOX", u))
	}
	for _, c := range s.Commands() {
		fmt.Println("cmd:", c.Line)
	}
	s.Close()
}
