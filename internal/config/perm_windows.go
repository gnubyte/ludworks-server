//go:build windows

package config

// Windows data directory security.
//
// %ProgramData% lets every user create subfolders, and the creator owns them. A user who
// creates %ProgramData%\Ludworks (or any directory later used as the data directory) before
// Ludworks does could otherwise keep ownership (and with it the right to change permissions),
// plant files the server trusts (config.json, secret.key, ludworks.log, links) or leave
// explicit permission entries behind. So the server never adopts a data directory, or a file
// it trusts, that is owned by or accessible to anyone but SYSTEM, Administrators,
// TrustedInstaller, the Ludworks service account and the account running the process.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	sidSystem           = "S-1-5-18"
	sidAdministrators   = "S-1-5-32-544"
	sidTrustedInstaller = "S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464"
	sidCreatorOwner     = "S-1-3-0" // placeholder replaced by the creator in inherited entries
	sidOwnerRights      = "S-1-3-4" // the object's owner (who is checked separately)

	fileDeleteChild    = 0x40 // FILE_DELETE_CHILD
	aceAllowedCallback = 9    // ACCESS_ALLOWED_CALLBACK_ACE_TYPE
	aceDeniedCallback  = 10   // ACCESS_DENIED_CALLBACK_ACE_TYPE

	// writeRights let a principal change or replace a file (configuration checks).
	writeRights = windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA | windows.FILE_WRITE_EA | fileDeleteChild |
		windows.DELETE | windows.WRITE_DAC | windows.WRITE_OWNER | windows.GENERIC_ALL | windows.GENERIC_WRITE |
		windows.MAXIMUM_ALLOWED
	// replaceRights on a parent directory let a principal move or replace what is inside it.
	replaceRights = fileDeleteChild | windows.WRITE_DAC | windows.WRITE_OWNER | windows.GENERIC_ALL | windows.MAXIMUM_ALLOWED
)

// privateDirSDDL is the security descriptor of a data directory: an optional owner and a
// protected DACL granting full control (inherited by everything below) to SYSTEM,
// Administrators and the given accounts only. The OWNER RIGHTS entry limits an owner to
// reading permissions, so owning an object never implies the right to change its DACL.
func privateDirSDDL(owner string, sids ...string) string {
	s := ""
	if owner != "" {
		s = "O:" + owner
	}
	s += "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"
	for _, sid := range sids {
		if sid != sidSystem && sid != sidAdministrators {
			s += "(A;OICI;FA;;;" + sid + ")"
		}
	}
	return s + "(A;OICI;RC;;;OW)"
}

func currentUserSID() (string, error) {
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	return tu.User.Sid.String(), nil
}

// trustedSIDs are the principals that may own Ludworks data or appear in its access lists.
func trustedSIDs(extra ...string) (map[string]bool, error) {
	cur, err := currentUserSID()
	if err != nil {
		return nil, err
	}
	t := map[string]bool{
		sidSystem: true, sidAdministrators: true, sidTrustedInstaller: true, sidCreatorOwner: true, sidOwnerRights: true,
		ServiceSID(WindowsServiceName): true, cur: true,
	}
	for _, s := range extra {
		t[s] = true
	}
	return t, nil
}

type aceInfo struct {
	known, allow, inheritOnly bool
	sid                       string
	mask                      uint32
}

// objectSecurity is the owner and DACL of one file system object.
type objectSecurity struct {
	owner   string
	hasDACL bool // false: NULL DACL, i.e. everyone has full access
	aces    []aceInfo
	reparse bool // a symbolic link, junction or other reparse point
	dir     bool
}

// inspect reads path's owner and DACL. With follow false a reparse point (link, junction) is
// inspected itself, never its target.
func inspect(path string, follow bool) (*objectSecurity, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	flags := uint32(windows.FILE_FLAG_BACKUP_SEMANTICS) // needed to open directories
	if !follow {
		flags |= windows.FILE_FLAG_OPEN_REPARSE_POINT
	}
	h, err := windows.CreateFile(p, windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, flags, 0)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	defer windows.CloseHandle(h)
	var fi windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &fi); err != nil {
		return nil, &fs.PathError{Op: "stat", Path: path, Err: err}
	}
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return nil, &fs.PathError{Op: "read security of", Path: path, Err: err}
	}
	o := &objectSecurity{
		reparse: fi.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0,
		dir:     fi.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0,
	}
	if owner, _, err := sd.Owner(); err == nil && owner != nil {
		o.owner = owner.String()
	}
	dacl, _, err := sd.DACL()
	switch {
	case err == nil && dacl != nil:
		o.hasDACL = true
		for i := 0; i < int(dacl.AceCount); i++ {
			var ace *windows.ACCESS_ALLOWED_ACE
			if err := windows.GetAce(dacl, uint32(i), &ace); err != nil {
				return nil, &fs.PathError{Op: "read security of", Path: path, Err: err}
			}
			a := aceInfo{mask: uint32(ace.Mask), inheritOnly: ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0}
			switch ace.Header.AceType {
			case windows.ACCESS_ALLOWED_ACE_TYPE, aceAllowedCallback:
				a.known, a.allow = true, true
			case windows.ACCESS_DENIED_ACE_TYPE, aceDeniedCallback:
				a.known = true
			}
			if a.known {
				a.sid = (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String()
			}
			o.aces = append(o.aces, a)
		}
	case err != nil && !errors.Is(err, windows.ERROR_OBJECT_NOT_FOUND):
		return nil, &fs.PathError{Op: "read security of", Path: path, Err: err}
	}
	return o, nil
}

// problem says why the object is not private to trusted principals ("" if it is). With
// writeOnly, untrusted read access is accepted (configuration files may be world-readable)
// and only rights to change or replace the object count.
func (o *objectSecurity) problem(trusted map[string]bool, writeOnly bool) string {
	if !trusted[o.owner] {
		return "is owned by " + accountName(o.owner)
	}
	if !o.hasDACL {
		return "has no access control list (everyone has full access)"
	}
	for _, a := range o.aces {
		switch {
		case !a.known:
			return "has a permission entry of an unsupported type"
		case !a.allow || trusted[a.sid]:
		case writeOnly && (a.inheritOnly || a.mask&writeRights == 0):
		default:
			return "grants access to " + accountName(a.sid)
		}
	}
	return ""
}

func accountName(sid string) string {
	if sid == "" {
		return "nobody (no owner)"
	}
	if s, err := windows.StringToSid(sid); err == nil {
		if acct, dom, _, err := s.LookupAccount(""); err == nil {
			if dom != "" {
				acct = dom + `\` + acct
			}
			return acct + " (" + sid + ")"
		}
	}
	return sid
}

// untrusted is the error for a data directory entry another account owns or can access.
func untrusted(path, problem string) error {
	return fmt.Errorf("%s %s. Ludworks only uses data that SYSTEM, Administrators and the Ludworks "+
		"account control: another user may have created or changed it to read or plant data. Inspect it, "+
		"then remove it or restore its permissions (see the Windows section of docs/security.md)", path, problem)
}

// walkTree visits path and everything below it in pre-order without following links or
// junctions (a reparse point is visited, its target is not).
func walkTree(path string, visit func(path string, o *objectSecurity) error) error {
	o, err := inspect(path, false)
	if err != nil {
		return err
	}
	if err := visit(path, o); err != nil {
		return err
	}
	if !o.dir || o.reparse {
		return nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := walkTree(filepath.Join(path, e.Name()), visit); err != nil {
			return err
		}
	}
	return nil
}

// verifyTree checks that nothing in the tree is owned by or accessible to an untrusted
// principal.
func verifyTree(root string, trusted map[string]bool) error {
	return walkTree(root, func(p string, o *objectSecurity) error {
		if prob := o.problem(trusted, false); prob != "" {
			return untrusted(p, prob)
		}
		return nil
	})
}

// createPrivateDir creates path with privateDirSDDL in one step (no window in which the
// inherited, more open permissions apply).
func createPrivateDir(path, owner string, sids ...string) error {
	sd, err := windows.SecurityDescriptorFromString(privateDirSDDL(owner, sids...))
	if err != nil {
		return err
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	sa := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	if err := windows.CreateDirectory(p, sa); err != nil {
		return &fs.PathError{Op: "create", Path: path, Err: err}
	}
	return nil
}

// setPrivateDACL replaces path's DACL with privateDirSDDL (and its owner, if given). Objects
// below it get their inherited entries recomputed; explicit entries are kept.
func setPrivateDACL(path, owner string, sids ...string) error {
	for _, s := range sids {
		if _, err := windows.StringToSid(s); err != nil {
			return err
		}
	}
	sd, err := windows.SecurityDescriptorFromString(privateDirSDDL("", sids...))
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	info := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION)
	var ownerSID *windows.SID
	if owner != "" {
		if ownerSID, err = windows.StringToSid(owner); err != nil {
			return err
		}
		info |= windows.OWNER_SECURITY_INFORMATION
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, info, ownerSID, nil, dacl, nil)
}

// setOwner makes owner the owner of path itself (a link is changed, not its target).
func setOwner(path, owner string) error {
	sid, err := windows.StringToSid(owner)
	if err != nil {
		return err
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(p, windows.WRITE_OWNER|windows.READ_CONTROL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return &fs.PathError{Op: "open", Path: path, Err: err}
	}
	defer windows.CloseHandle(h)
	return windows.SetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION, sid, nil, nil, nil)
}

// restrictDir gives path a private DACL for SYSTEM, Administrators, the current account and
// the Ludworks service account. Children inherit it.
func restrictDir(path string) error {
	cur, err := currentUserSID()
	if err != nil {
		return err
	}
	return setPrivateDACL(path, "", cur, ServiceSID(WindowsServiceName))
}

// RestrictDirTo sets a protected DACL on path granting full control (inherited by children)
// to SYSTEM, BUILTIN\Administrators and the given account SIDs only.
func RestrictDirTo(path string, sids ...string) error { return setPrivateDACL(path, "", sids...) }

// ensureDataDir creates the data directory privately, or checks an existing one: it must be
// owned by a trusted principal. If its access list is more open than that (e.g. a folder
// created by hand that inherited its parent's permissions), it is made private and then
// everything inside is checked, since whoever had access may have left something behind.
func ensureDataDir(path string) error {
	cur, err := currentUserSID()
	if err != nil {
		return err
	}
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return fmt.Errorf("create data dir: %w", err)
		}
		err := createPrivateDir(path, "", cur, ServiceSID(WindowsServiceName))
		if err == nil {
			return nil
		}
		if !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			return fmt.Errorf("create data dir: %w", err)
		}
		// Created concurrently: check it like any existing directory.
	} else if err != nil {
		return fmt.Errorf("data dir: %w", err)
	}
	trusted, err := trustedSIDs()
	if err != nil {
		return err
	}
	link, err := inspect(path, false)
	if err != nil {
		return fmt.Errorf("check data dir: %w", err)
	}
	if link.reparse { // e.g. a junction to another drive: the link itself must be trusted too
		if prob := link.problem(trusted, false); prob != "" {
			return untrusted(path+" (a link)", prob)
		}
	}
	o, err := inspect(path, true)
	if err != nil {
		return fmt.Errorf("check data dir: %w", err)
	}
	if !o.dir {
		return fmt.Errorf("data dir %s is not a directory", path)
	}
	if !trusted[o.owner] {
		return untrusted(path, "is owned by "+accountName(o.owner))
	}
	if o.problem(trusted, false) == "" {
		return nil
	}
	if err := restrictDir(path); err != nil {
		return fmt.Errorf("restrict data dir permissions: %w", err)
	}
	return verifyTree(path, trusted)
}

// dataEntries are the files and directories the server trusts directly: keys, database,
// configuration, logs and the directories holding user files, backups and TLS keys.
func dataEntries(c *Config) []string {
	return []string{
		c.SecretKeyPath(), c.DBPath(), c.DBPath() + "-wal", c.DBPath() + "-shm", c.SetupTokenPath(),
		filepath.Join(c.DataDir, "config.json"), filepath.Join(c.DataDir, "ludworks.log"),
		filepath.Join(c.DataDir, "ludworks.log.1"),
		c.StorageDir(), c.TmpDir(), c.ThumbsDir(), c.BackupsDir(), c.TLSDir(),
	}
}

// checkDataEntries refuses data that another account owns or can access, even inside a
// private data directory (explicit permission entries survive a parent's DACL change).
func checkDataEntries(c *Config) error {
	trusted, err := trustedSIDs()
	if err != nil {
		return err
	}
	for _, p := range dataEntries(c) {
		o, err := inspect(p, false)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("check %s: %w", p, err)
		}
		if prob := o.problem(trusted, false); prob != "" {
			return untrusted(p, prob)
		}
	}
	return nil
}

// checkConfigFile refuses a configuration file that an untrusted account owns or can change
// (it sets data_dir, TLS keys, listen address and trusted proxies). Reading is allowed.
func checkConfigFile(path string) error {
	trusted, err := trustedSIDs()
	if err != nil {
		return err
	}
	for _, follow := range []bool{false, true} {
		o, err := inspect(path, follow)
		if err != nil {
			return fmt.Errorf("check config file: %w", err)
		}
		if prob := o.problem(trusted, true); prob != "" {
			return fmt.Errorf("config file %s %s; refusing to use it because another user could have written it", path, prob)
		}
		if !o.reparse {
			break
		}
	}
	return nil
}

// CheckPrivate returns an error unless path (not followed if it is a link) is owned by and
// accessible to trusted principals only.
func CheckPrivate(path string) error {
	trusted, err := trustedSIDs()
	if err != nil {
		return err
	}
	o, err := inspect(path, false)
	if err != nil {
		return err
	}
	if o.reparse {
		return fmt.Errorf("%s is a link or junction", path)
	}
	if prob := o.problem(trusted, false); prob != "" {
		return untrusted(path, prob)
	}
	return nil
}

// checkAncestors refuses a data directory whose parent directories an untrusted account owns
// or could use to move or replace it.
func checkAncestors(path string, trusted map[string]bool) error {
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		root := filepath.Dir(dir) == dir
		o, err := inspect(dir, false)
		if err != nil {
			return fmt.Errorf("check %s: %w", dir, err)
		}
		if !trusted[o.owner] {
			return untrusted(dir+" (a parent of the data directory)", "is owned by "+accountName(o.owner))
		}
		mask := uint32(replaceRights)
		if !root { // a volume root cannot be renamed
			mask |= windows.DELETE
		}
		for _, a := range o.aces {
			if a.known && a.allow && !a.inheritOnly && !trusted[a.sid] && a.mask&mask != 0 {
				return untrusted(dir+" (a parent of the data directory)", "lets "+accountName(a.sid)+" move or replace its contents")
			}
		}
		if root {
			return nil
		}
	}
}

// PrepareServiceDataDir makes path the data directory of the Windows service: owned by
// Administrators, full control only for SYSTEM, Administrators and the service account
// (svcSID). A new directory is created with that security in one step. An existing one (e.g.
// kept from an earlier installation) is adopted only if nothing in it is owned by or accessible
// to another account; otherwise installation stops without changing anything. Objects owned
// by the installing administrator's own account are handed to Administrators, since the
// service will not trust that account. Must run elevated.
func PrepareServiceDataDir(path, svcSID string) error {
	cur, err := currentUserSID()
	if err != nil {
		return err
	}
	trusted, err := trustedSIDs(svcSID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := checkAncestors(path, trusted); err != nil {
		return err
	}
	o, err := inspect(path, false)
	if errors.Is(err, fs.ErrNotExist) {
		err = createPrivateDir(path, sidAdministrators, svcSID)
		if err == nil {
			return nil
		}
		if !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			return err
		}
		o, err = inspect(path, false) // created concurrently: check it like an existing one
	}
	if err != nil {
		return err
	}
	if o.reparse {
		return fmt.Errorf("%s is a link or junction; pass the real directory with --data", path)
	}
	if !o.dir {
		return fmt.Errorf("%s is not a directory", path)
	}
	var reown []string
	err = walkTree(path, func(p string, x *objectSecurity) error {
		if prob := x.problem(trusted, false); prob != "" {
			return untrusted(p, prob)
		}
		if x.owner == cur && cur != svcSID && cur != sidSystem && cur != sidAdministrators {
			reown = append(reown, p)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := setPrivateDACL(path, sidAdministrators, svcSID); err != nil {
		return fmt.Errorf("secure %s: %w", path, err)
	}
	for _, p := range reown {
		if p == path {
			continue
		}
		if err := setOwner(p, sidAdministrators); err != nil {
			return fmt.Errorf("hand %s to Administrators: %w", p, err)
		}
	}
	return nil
}
