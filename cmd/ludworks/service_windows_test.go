//go:build windows

package main

import (
	"slices"
	"testing"
	"unicode/utf16"
)

func TestMultiSZ(t *testing.T) {
	got := multiSZ([]string{"SeChangeNotifyPrivilege", "Ab"})
	want := append(utf16.Encode([]rune("SeChangeNotifyPrivilege\x00Ab\x00")), 0)
	if !slices.Equal(got, want) {
		t.Fatalf("multiSZ = %v, want %v", got, want)
	}
	if got := multiSZ(nil); !slices.Equal(got, []uint16{0, 0}) {
		t.Fatalf("empty multiSZ = %v", got)
	}
}

// The service keeps only SeChangeNotifyPrivilege: no SeImpersonatePrivilege (potato-style
// escalation to SYSTEM) and no SeCreateGlobalPrivilege.
func TestServiceRequiredPrivilegesMinimal(t *testing.T) {
	if !slices.Equal(serviceRequiredPrivileges, []string{"SeChangeNotifyPrivilege"}) {
		t.Fatalf("serviceRequiredPrivileges = %v", serviceRequiredPrivileges)
	}
}
