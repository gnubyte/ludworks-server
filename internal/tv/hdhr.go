package tv

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"ludworks/internal/auth"
	"ludworks/internal/core"
	"ludworks/internal/httpx"
)

// hdhrDevice is the authorised context of an HDHomeRun request.
type hdhrDevice struct {
	p        *core.Principal
	token    string
	settings Settings
	base     string // absolute .../api/v1/hdhr/<token>
	deviceID string
}

var errHDHRNotFound = core.ErrNotFound

// hdhrAuth authorises an HDHomeRun request. Everything that fails — emulation disabled,
// malformed, unknown, expired or revoked token, a token without tv:feed (or tv:read) or one that is not
// the user's current feed token — is an indistinguishable 404. Tokens are looked up by their
// SHA-256 (auth.VerifyToken), so the lookup time does not depend on how much of a guess
// matches.
func (m *Module) hdhrAuth(r *http.Request) (*hdhrDevice, error) {
	ctx := r.Context()
	st := m.tvSettings(ctx)
	if !st.HDHREnabled {
		return nil, errHDHRNotFound
	}
	tok := r.PathValue("feed_token")
	if len(tok) < 8 || len(tok) > 200 || !strings.HasPrefix(tok, "lwp_") {
		return nil, errHDHRNotFound
	}
	p, err := auth.VerifyToken(ctx, m.d, tok)
	if err != nil || p == nil || !hasFeedScopes(p) {
		return nil, errHDHRNotFound
	}
	var userID string
	err = m.d.DB.SQL.QueryRowContext(ctx, `SELECT user_id FROM tv_feed_tokens WHERE token_id = ?`, p.SessionID).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && userID != p.UserID) {
		return nil, errHDHRNotFound
	}
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte("ludworks-hdhr:" + p.SessionID))
	return &hdhrDevice{
		p: p, token: tok, settings: st, deviceID: strings.ToUpper(hex.EncodeToString(sum[:4])),
		base: m.baseURL(ctx) + "/api/v1/hdhr/" + url.PathEscape(tok),
	}, nil
}

// HDHRDiscover is discover.json (field names as Plex/Jellyfin expect).
type HDHRDiscover struct {
	FriendlyName    string `json:"FriendlyName"`
	Manufacturer    string `json:"Manufacturer"`
	ModelNumber     string `json:"ModelNumber"`
	FirmwareName    string `json:"FirmwareName"`
	FirmwareVersion string `json:"FirmwareVersion"`
	DeviceID        string `json:"DeviceID"`
	DeviceAuth      string `json:"DeviceAuth"`
	BaseURL         string `json:"BaseURL"`
	LineupURL       string `json:"LineupURL"`
	TunerCount      int    `json:"TunerCount"`
}

func (m *Module) hdhrDiscover(w http.ResponseWriter, r *http.Request) error {
	dev, err := m.hdhrAuth(r)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, HDHRDiscover{
		FriendlyName: dev.settings.HDHRDeviceName, Manufacturer: "Ludworks", ModelNumber: "HDTC-2US",
		FirmwareName: "hdhomeruntc_atsc", FirmwareVersion: "20200101", DeviceID: dev.deviceID,
		DeviceAuth: "ludworks-" + strings.ToLower(dev.deviceID), BaseURL: dev.base, LineupURL: dev.base + "/lineup.json",
		TunerCount: dev.settings.HDHRTunerCount,
	})
}

// HDHRLineupEntry is one lineup.json entry.
type HDHRLineupEntry struct {
	GuideNumber string `json:"GuideNumber"`
	GuideName   string `json:"GuideName"`
	URL         string `json:"URL"`
}

func (m *Module) hdhrLineup(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	dev, err := m.hdhrAuth(r)
	if err != nil {
		return err
	}
	rows, err := visibleChannels(ctx, m.d.DB.SQL, dev.p.UserID)
	if err != nil {
		return err
	}
	base := m.baseURL(ctx)
	tq := "?access_token=" + url.QueryEscape(dev.token)
	ff := m.ffmpeg(ctx) != ""
	out := make([]HDHRLineupEntry, 0, len(rows))
	seen := map[int]int{}
	for _, c := range rows {
		// Guide numbers must be unique: a shared channel whose number another channel already
		// uses becomes a sub-channel ("5.1", "5.2", …).
		num := strconv.Itoa(c.Number)
		if n := seen[c.Number]; n > 0 {
			num += "." + strconv.Itoa(n)
		}
		seen[c.Number]++
		out = append(out, HDHRLineupEntry{GuideNumber: num, GuideName: c.Name, URL: streamURL(base, c.ID, tq, ff)})
	}
	return httpx.WriteJSON(w, http.StatusOK, out)
}

// HDHRLineupStatus is lineup_status.json.
type HDHRLineupStatus struct {
	ScanInProgress int      `json:"ScanInProgress"`
	ScanPossible   int      `json:"ScanPossible"`
	Source         string   `json:"Source"`
	SourceList     []string `json:"SourceList"`
}

func (m *Module) hdhrLineupStatus(w http.ResponseWriter, r *http.Request) error {
	if _, err := m.hdhrAuth(r); err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, HDHRLineupStatus{ScanInProgress: 0, ScanPossible: 1, Source: "Cable", SourceList: []string{"Cable"}})
}

func (m *Module) hdhrDeviceXML(w http.ResponseWriter, r *http.Request) error {
	dev, err := m.hdhrAuth(r)
	if err != nil {
		return err
	}
	udn := strings.ToLower(dev.deviceID)
	body := `<?xml version="1.0" encoding="UTF-8"?>
<root xmlns="urn:schemas-upnp-org:device-1-0">
  <specVersion><major>1</major><minor>0</minor></specVersion>
  <URLBase>` + xmlText(dev.base) + `</URLBase>
  <device>
    <deviceType>urn:schemas-upnp-org:device:MediaServer:1</deviceType>
    <friendlyName>` + xmlText(dev.settings.HDHRDeviceName) + `</friendlyName>
    <manufacturer>Ludworks</manufacturer>
    <modelName>HDTC-2US</modelName>
    <modelNumber>HDTC-2US</modelNumber>
    <serialNumber>` + xmlText(dev.deviceID) + `</serialNumber>
    <UDN>uuid:` + udn + `-0000-4000-8000-` + udn + udn[:4] + `</UDN>
  </device>
</root>
`
	h := w.Header()
	h.Set("Content-Type", "application/xml; charset=utf-8")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	_, err = w.Write([]byte(body))
	return err
}

// hdhrLineupPost acknowledges a channel scan request (Plex POSTs lineup.post?scan=start);
// the lineup is always current, so there is nothing to scan.
func (m *Module) hdhrLineupPost(w http.ResponseWriter, r *http.Request) error {
	if _, err := m.hdhrAuth(r); err != nil {
		return err
	}
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusOK)
	return nil
}
