package airplay

import (
	"strconv"
	"strings"

	"ludworks/internal/cast/renderer"
)

// Discovery data. The _airplay._tcp TXT "features" (RAOP TXT "ft") is a 64-bit mask written
// as "0xLOW" or "0xLOW,0xHIGH" (openairplay feature table); "flags" (RAOP "sf") holds the
// status flags. Bits used here:
//
//	features bit 0   SupportsAirPlayVideoV1 — the device plays a URL with /play
//	features bit 38  SupportsUnifiedMediaControl  ┐ AirPlay 2 receiver: /play needs the
//	features bit 48  SupportsCoreUtilsPairingAndEncryption ┘ SETUP/RECORD session first
//	features bit 43  SupportsSystemPairing        ┐ HAP pairing (PIN and transient)
//	features bit 46  SupportsHKPairingAndAccessControl ┘ (with 48)
//	features bit 49  SupportsAirPlayVideoV2
//	flags 0x8        PIN required             ┐ video needs pairing first
//	flags 0x200      one-time pairing required ┘ (also: RAOP needs device verification)
//	flags 0x80       password required
//
// RAOP TXT: et = encryption types (0 none, 1 RSA/AES, 3 FairPlay, 4 MFi-SAP, 5 FairPlay
// SAPv2.5), ek = 1 when the AES key exchange is expected, cn = codecs (0 PCM, 1 ALAC,
// 2 AAC, 3 AAC-ELD), md = metadata (0 text, 1 artwork, 2 progress), pw = password,
// tp = transports, am = model, ch/sr/ss = channels, sample rate, sample size.

const (
	featVideoV1        = 1 << 0
	featUnifiedMedia   = 1 << 38
	featSystemPairing  = 1 << 43
	featHKPairing      = 1 << 46
	featCoreUtilsPair  = 1 << 48
	featVideoV2        = 1 << 49
	flagPINRequired    = 0x8
	flagPasswordNeeded = 0x80
	flagOneTimePairing = 0x200
)

// deviceInfo is what the driver derives from a device's endpoints.
type deviceInfo struct {
	raop, ap        *renderer.Endpoint
	features        uint64
	flags           uint64
	password        bool
	accessHome      bool // acl=1: only members of the device's Home may use it
	encNone, encRSA bool
	encMFi          bool // et contains 4 (MFi-SAP auth-setup)
	encListed       bool
	expectKey       bool // ek=1
	alac            bool
	audioFormatOK   bool
	mdText, mdArt   bool
	mdProgress      bool
	model           string
}

func parseDevice(dev *renderer.Device) *deviceInfo {
	info := &deviceInfo{alac: true, audioFormatOK: true}
	if ep, ok := dev.Endpoint(renderer.ServiceRAOP); ok {
		info.raop = &ep
	}
	if ep, ok := dev.Endpoint(renderer.ServiceAirPlay); ok {
		info.ap = &ep
	}
	txt := func(k string) string {
		if info.ap != nil {
			if v, ok := info.ap.TXT[k]; ok {
				return v
			}
		}
		if info.raop != nil {
			return info.raop.TXT[k]
		}
		return ""
	}
	raopTXT := func(k string) string {
		if info.raop != nil {
			return info.raop.TXT[k]
		}
		return ""
	}
	feat := txt("features")
	if feat == "" {
		feat = raopTXT("ft")
	}
	info.features, _ = parseFeatures(feat)
	for _, f := range []string{txt("flags"), raopTXT("sf")} {
		if v, err := strconv.ParseUint(strings.TrimPrefix(strings.ToLower(f), "0x"), 16, 64); err == nil {
			info.flags |= v
		}
	}
	info.password = strings.EqualFold(txt("pw"), "true") || strings.EqualFold(raopTXT("pw"), "true") ||
		info.flags&flagPasswordNeeded != 0
	info.accessHome = txt("acl") == "1"
	info.model = txt("model")
	if info.model == "" {
		info.model = raopTXT("am")
	}

	if et := raopTXT("et"); et != "" {
		info.encListed = true
		for _, t := range splitList(et) {
			switch t {
			case "0":
				info.encNone = true
			case "1":
				info.encRSA = true
			case "4":
				info.encMFi = true
			}
		}
	}
	info.expectKey = raopTXT("ek") == "1"
	if cn := raopTXT("cn"); cn != "" {
		info.alac = false
		for _, c := range splitList(cn) {
			if c == "1" {
				info.alac = true
			}
		}
	}
	for k, want := range map[string]string{"ch": "2", "sr": "44100", "ss": "16"} {
		if v := raopTXT(k); v != "" && v != want {
			info.audioFormatOK = false
		}
	}
	if tp := raopTXT("tp"); tp != "" && !strings.Contains(strings.ToUpper(tp), "UDP") {
		info.audioFormatOK = false
	}
	for _, m := range splitList(raopTXT("md")) {
		switch m {
		case "0":
			info.mdText = true
		case "1":
			info.mdArt = true
		case "2":
			info.mdProgress = true
		}
	}
	return info
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parseFeatures reads "0xLOW" or "0xLOW,0xHIGH".
func parseFeatures(s string) (uint64, bool) {
	if s == "" {
		return 0, false
	}
	lo, hi, two := strings.Cut(s, ",")
	l, err := strconv.ParseUint(strings.TrimPrefix(strings.ToLower(strings.TrimSpace(lo)), "0x"), 16, 32)
	if err != nil {
		return 0, false
	}
	if !two {
		return l, true
	}
	h, err := strconv.ParseUint(strings.TrimPrefix(strings.ToLower(strings.TrimSpace(hi)), "0x"), 16, 32)
	if err != nil {
		return 0, false
	}
	return h<<32 | l, true
}

// audio reports whether RAOP streaming is possible in principle.
func (i *deviceInfo) audio() bool { return i.raop != nil && i.alac && i.audioFormatOK }

// video reports whether the device plays URLs.
func (i *deviceInfo) video() bool {
	return i.ap != nil && i.features&(featVideoV1|featVideoV2) != 0
}

// hap reports whether the device does HAP pairing (PIN or transient).
func (i *deviceInfo) hap() bool {
	return i.features&(featSystemPairing|featHKPairing|featCoreUtilsPair) != 0
}

// transient reports whether transient pairing is worth trying.
func (i *deviceInfo) transient() bool {
	return i.features&(featSystemPairing|featCoreUtilsPair) != 0
}

// airplay2 reports whether /play needs an AirPlay 2 session (SETUP/RECORD) first.
func (i *deviceInfo) airplay2() bool {
	return i.features&(featUnifiedMedia|featCoreUtilsPair) != 0
}

// videoNeedsPairing reports whether the device refuses video until paired.
func (i *deviceInfo) videoNeedsPairing() bool {
	return i.flags&(flagPINRequired|flagOneTimePairing) != 0
}

// raopEncryption picks the stream encryption: false for none, true for RSA/AES; ok is
// false when the receiver offers neither.
func (i *deviceInfo) raopEncryption() (rsa, ok bool) {
	switch {
	case !i.encListed:
		return false, true
	case i.encRSA && (i.expectKey || !i.encNone):
		return true, true
	case i.encNone:
		return false, true
	}
	return false, false
}

// authSetupFirst reports whether to run the unencrypted MFi auth-setup before ANNOUNCE
// (AirPort Express with AirPlay 2 firmware wants it; other et=4 receivers get it only after
// refusing an ANNOUNCE, since some stall when it is sent).
func (i *deviceInfo) authSetupFirst() bool {
	return i.encMFi && strings.HasPrefix(i.model, "AirPort")
}
