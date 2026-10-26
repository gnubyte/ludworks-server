package config

import (
	"crypto/sha1"
	"encoding/binary"
	"strconv"
	"strings"
	"unicode/utf16"
)

// WindowsServiceName is the name of the Windows service, and of its virtual account
// NT SERVICE\Ludworks.
const WindowsServiceName = "Ludworks"

// ServiceSID returns the SID of the virtual account "NT SERVICE\<name>": S-1-5-80 followed by
// the SHA-1 of the upper-cased UTF-16LE service name read as five little-endian 32-bit values.
// It is the same on every machine and known before the service exists, so the data directory
// can be checked and created for it before the service is registered.
func ServiceSID(name string) string {
	u := utf16.Encode([]rune(strings.ToUpper(name)))
	b := make([]byte, 2*len(u))
	for i, c := range u {
		binary.LittleEndian.PutUint16(b[2*i:], c)
	}
	sum := sha1.Sum(b)
	var sb strings.Builder
	sb.WriteString("S-1-5-80")
	for i := 0; i < len(sum); i += 4 {
		sb.WriteByte('-')
		sb.WriteString(strconv.FormatUint(uint64(binary.LittleEndian.Uint32(sum[i:])), 10))
	}
	return sb.String()
}
