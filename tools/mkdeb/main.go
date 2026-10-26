// Command mkdeb builds Debian binary packages (.deb) in pure Go, without dpkg-deb, and
// inspects them.
//
//	mkdeb build -spec packaging/deb/ludworks.json -version 0.1.0 -arch amd64 \
//	      -var BINARY=/abs/path/to/ludworks -out dist/ludworks_0.1.0_amd64.deb
//	mkdeb inspect dist/ludworks_0.1.0_amd64.deb
//
// The package is an ar archive holding debian-binary ("2.0\n"), control.tar.gz and
// data.tar.gz, in that order. Everything is owned by root:root (uid/gid 0), file modes come
// from the spec (never from the build host's file system), and every timestamp is
// SOURCE_DATE_EPOCH (or -mtime), so two builds of the same inputs are byte-identical.
// control.tar.gz carries control (with a computed Installed-Size), md5sums (all regular
// files except conffiles, as dh_md5sums does), conffiles and the maintainer scripts.
//
// The spec is a small JSON document; relative source paths are resolved against the
// spec's directory, and ${NAME} variables (VERSION, ARCH and any -var NAME=value) are
// expanded in the control template and in source paths:
//
//	{
//	  "control": "control",
//	  "files": [
//	    {"src": "${BINARY}", "dst": "/usr/bin/ludworks", "mode": "0755"},
//	    {"src": "config.json", "dst": "/etc/ludworks/config.json", "mode": "0640", "conffile": true}
//	  ],
//	  "dirs": [{"dst": "/etc/ludworks", "mode": "0755"}],
//	  "scripts": {"postinst": "postinst", "prerm": "prerm", "postrm": "postrm"}
//	}
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

const usage = `Usage:
  mkdeb build -spec FILE -version VERSION -arch ARCH [-var NAME=VALUE]... -out FILE.deb [-mtime UNIX]
  mkdeb inspect FILE.deb

build:   assemble a .deb from a JSON spec (timestamps from -mtime, else SOURCE_DATE_EPOCH, else now)
inspect: print the control data and file list of a .deb and verify its md5sums
`

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "build":
		return cmdBuild(args[1:], stdout, stderr)
	case "inspect":
		return cmdInspect(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "mkdeb: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

// varFlags collects repeated -var NAME=VALUE flags.
type varFlags map[string]string

func (v varFlags) String() string { return "" }

func (v varFlags) Set(s string) error {
	name, value, ok := strings.Cut(s, "=")
	if !ok || !validVarName(name) {
		return fmt.Errorf("expected NAME=VALUE with NAME matching [A-Z][A-Z0-9_]*, got %q", s)
	}
	if name == "VERSION" || name == "ARCH" {
		return fmt.Errorf("%s is set with -%s", name, strings.ToLower(name))
	}
	if _, dup := v[name]; dup {
		return fmt.Errorf("variable %s given twice", name)
	}
	v[name] = value
	return nil
}

func cmdBuild(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mkdeb build", flag.ContinueOnError)
	fs.SetOutput(stderr)
	specPath := fs.String("spec", "", "package spec (JSON)")
	version := fs.String("version", "", "package version (Debian syntax)")
	arch := fs.String("arch", "", "Debian architecture, e.g. amd64, arm64, all")
	out := fs.String("out", "", "output .deb path")
	mtime := fs.String("mtime", "", "timestamp for every entry (Unix seconds); default SOURCE_DATE_EPOCH, else now")
	vars := varFlags{}
	fs.Var(vars, "var", "NAME=VALUE variable for the spec (repeatable)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || *specPath == "" || *version == "" || *arch == "" || *out == "" {
		fmt.Fprint(stderr, "mkdeb build: -spec, -version, -arch and -out are required\n\n"+usage)
		return 2
	}
	t, err := buildTime(*mtime, os.Getenv("SOURCE_DATE_EPOCH"), time.Now)
	if err != nil {
		fmt.Fprintln(stderr, "mkdeb:", err)
		return 2
	}
	vars["VERSION"] = *version
	vars["ARCH"] = *arch
	pkg, err := LoadSpec(*specPath, vars, t)
	if err != nil {
		fmt.Fprintln(stderr, "mkdeb:", err)
		return 1
	}
	if err := pkg.WriteFile(*out); err != nil {
		fmt.Fprintln(stderr, "mkdeb:", err)
		return 1
	}
	fmt.Fprintf(stdout, "mkdeb: wrote %s (%s %s %s, %d files, Installed-Size %d KiB)\n",
		*out, pkg.Name(), pkg.Version(), pkg.Arch(), pkg.FileCount(), pkg.InstalledSize())
	return 0
}

// buildTime resolves the timestamp used for every archive entry.
func buildTime(flagVal, envVal string, now func() time.Time) (time.Time, error) {
	parse := func(src, s string) (time.Time, error) {
		n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil || n < 0 || n > 1<<33-1 { // ustar/GNU numeric field limit
			return time.Time{}, fmt.Errorf("%s: invalid Unix timestamp %q", src, s)
		}
		return time.Unix(n, 0).UTC(), nil
	}
	switch {
	case flagVal != "":
		return parse("-mtime", flagVal)
	case envVal != "":
		return parse("SOURCE_DATE_EPOCH", envVal)
	default:
		return now().UTC().Truncate(time.Second), nil
	}
}

func cmdInspect(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprint(stderr, "usage: mkdeb inspect FILE.deb\n")
		return 2
	}
	data, err := os.ReadFile(args[0])
	if err != nil {
		fmt.Fprintln(stderr, "mkdeb:", err)
		return 1
	}
	deb, err := ReadDeb(data)
	if err != nil {
		fmt.Fprintln(stderr, "mkdeb:", err)
		return 1
	}
	deb.Describe(stdout)
	if err := deb.VerifyMD5Sums(); err != nil {
		fmt.Fprintln(stderr, "mkdeb: md5sums:", err)
		return 1
	}
	fmt.Fprintf(stdout, "md5sums: OK (%d files)\n", len(deb.MD5Sums))
	return 0
}
