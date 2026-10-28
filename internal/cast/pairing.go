package cast

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"regexp"
	"time"

	"ludworks/internal/cast/renderer"
	"ludworks/internal/core"
	"ludworks/internal/db"
	"ludworks/internal/httpx"
)

// Pairing (API.md §14): Apple TVs need a one-time PIN pairing before they accept AirPlay
// video. The driver's renderer.Pairer speaks the protocol; the credentials it returns are kept
// server-wide per device in cast_pairings, sealed with the server key (AES-256-GCM via the
// settings cipher, bound to the device id) like the server's other secrets.

const (
	pairingTTL         = 2 * time.Minute // a PIN must be confirmed within this
	maxPairings        = 8               // pairings in progress server-wide (one per user)
	pairingStartWait   = 20 * time.Second
	pairingConfirmWait = 30 * time.Second
)

var pinPattern = regexp.MustCompile(`^[0-9]{4}$`)

type pendingPairing struct {
	id, userID, deviceID string
	pairing              renderer.Pairing
	expires              time.Time
}

func credsAAD(deviceID string) string { return "cast-pairing:" + deviceID }

// loadCreds returns the stored pairing credentials of a device (nil when it was never paired
// or the sealed value cannot be opened).
func (m *Module) loadCreds(ctx context.Context, deviceID string) []byte {
	var enc string
	err := m.d.DB.SQL.QueryRowContext(ctx, `SELECT creds_enc FROM cast_pairings WHERE device_id = ?`, deviceID).Scan(&enc)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			m.log.Warn("cast: reading pairing failed", "device", deviceID, "err", err)
		}
		return nil
	}
	creds, err := m.d.Settings.Cipher().Open(enc, credsAAD(deviceID))
	if err != nil {
		m.log.Warn("cast: stored pairing cannot be decrypted; pair the device again", "device", deviceID)
		return nil
	}
	return creds
}

// saveCreds seals and stores a device's pairing credentials.
func (m *Module) saveCreds(ctx context.Context, deviceID, userID string, creds []byte) error {
	enc, err := m.d.Settings.Cipher().Seal(creds, credsAAD(deviceID))
	if err != nil {
		return err
	}
	_, err = m.d.DB.SQL.ExecContext(ctx, `INSERT INTO cast_pairings (device_id, creds_enc, paired_by, created_at)
		VALUES (?, ?, ?, ?) ON CONFLICT (device_id) DO UPDATE SET creds_enc = excluded.creds_enc,
		paired_by = excluded.paired_by, created_at = excluded.created_at`,
		deviceID, enc, userID, db.FormatTime(m.now()))
	return err
}

// pairedIDs returns the ids of every paired device.
func (m *Module) pairedIDs(ctx context.Context) map[string]bool {
	out := map[string]bool{}
	rs, err := m.d.DB.SQL.QueryContext(ctx, `SELECT device_id FROM cast_pairings`)
	if err != nil {
		m.log.Warn("cast: listing pairings failed", "err", err)
		return out
	}
	defer rs.Close()
	for rs.Next() {
		var id string
		if rs.Scan(&id) == nil {
			out[id] = true
		}
	}
	return out
}

// takePairing removes and returns a pending pairing.
func (m *Module) takePairing(id string) *pendingPairing {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.pairings[id]
	delete(m.pairings, id)
	return p
}

// sweepPairings closes expired pairings (all of them when all is set).
func (m *Module) sweepPairings(all bool) {
	now := m.now()
	var drop []*pendingPairing
	m.mu.Lock()
	for id, p := range m.pairings {
		if all || now.After(p.expires) {
			drop = append(drop, p)
			delete(m.pairings, id)
		}
	}
	m.mu.Unlock()
	for _, p := range drop {
		_ = p.pairing.Close()
	}
}

var (
	errPairingUnsupported = core.NewError(http.StatusUnprocessableEntity, codePairingUnsupported,
		"This device needs a kind of pairing or password this server cannot do (for a HomePod or speaker, allow everyone on the network in its \"Allow speaker access\" setting)")
	errPairingNotFound = core.NotFound("Pairing not found or expired; start pairing again")
)

// POST /cast/devices/{id}/pair
func (m *Module) startPairing(w http.ResponseWriter, r *http.Request) error {
	p := core.MustPrincipal(r.Context())
	e, err := m.deviceFromPath(r)
	if err != nil {
		return err
	}
	_, drv := m.capabilities(&e.dev)
	pr, ok := drv.(renderer.Pairer)
	if !ok {
		return errPairingUnsupported
	}
	// One pairing per user: starting again replaces the previous attempt.
	m.mu.Lock()
	var old []*pendingPairing
	for id, pp := range m.pairings {
		if pp.userID == p.UserID {
			old = append(old, pp)
			delete(m.pairings, id)
		}
	}
	full := len(m.pairings) >= maxPairings
	m.mu.Unlock()
	for _, pp := range old {
		_ = pp.pairing.Close()
	}
	if full {
		w.Header().Set("Retry-After", "30")
		return core.NewError(http.StatusTooManyRequests, "rate_limited", "Too many pairings are in progress; try again shortly")
	}
	ctx, cancel := context.WithTimeout(r.Context(), pairingStartWait)
	defer cancel()
	pairing, err := pr.StartPairing(ctx, &e.dev)
	if err != nil {
		if errors.Is(err, renderer.ErrPairingUnsupported) {
			return errPairingUnsupported
		}
		m.log.Info("cast: starting pairing failed", "device", e.dev.ID, "err", err)
		return core.NewError(http.StatusBadGateway, codeDeviceUnavailable, "The device did not start pairing")
	}
	pp := &pendingPairing{id: core.NewID(), userID: p.UserID, deviceID: e.dev.ID, pairing: pairing, expires: m.now().Add(pairingTTL)}
	m.mu.Lock()
	m.pairings[pp.id] = pp
	m.mu.Unlock()
	return httpx.WriteJSON(w, http.StatusOK, map[string]string{"pairing_id": pp.id})
}

// POST /cast/devices/{id}/pair/confirm
func (m *Module) confirmPairing(w http.ResponseWriter, r *http.Request) error {
	p := core.MustPrincipal(r.Context())
	var req struct {
		PairingID string `json:"pairing_id"`
		PIN       string `json:"pin"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		return err
	}
	fe := map[string]string{}
	if !httpx.ValidID(req.PairingID) {
		fe["pairing_id"] = "is required"
	}
	if !pinPattern.MatchString(req.PIN) {
		fe["pin"] = "Enter the 4-digit PIN shown on the TV"
	}
	if len(fe) > 0 {
		return core.Validation(fe)
	}
	e, err := m.deviceFromPath(r)
	if err != nil {
		return err
	}
	m.mu.Lock()
	pp := m.pairings[req.PairingID]
	m.mu.Unlock()
	if pp == nil || pp.userID != p.UserID || pp.deviceID != e.dev.ID || m.now().After(pp.expires) {
		return errPairingNotFound
	}
	// The pairing is used once, whatever the outcome (a wrong PIN ends it on the device too).
	if pp = m.takePairing(req.PairingID); pp == nil {
		return errPairingNotFound
	}
	defer pp.pairing.Close()
	ctx, cancel := context.WithTimeout(r.Context(), pairingConfirmWait)
	defer cancel()
	creds, err := pp.pairing.Confirm(ctx, req.PIN)
	switch {
	case errors.Is(err, renderer.ErrWrongPIN):
		return core.Validation(map[string]string{"pin": "Wrong PIN. Start pairing again to get a new one."})
	case errors.Is(err, renderer.ErrPairingUnsupported):
		return errPairingUnsupported
	case err != nil:
		m.log.Info("cast: pairing failed", "device", e.dev.ID, "err", err)
		return core.NewError(http.StatusBadGateway, codeDeviceUnavailable, "Pairing with the device failed")
	case len(creds) == 0 || len(creds) > 64<<10:
		return core.NewError(http.StatusBadGateway, codeDeviceUnavailable, "Pairing with the device failed")
	}
	if err := m.saveCreds(r.Context(), e.dev.ID, p.UserID, creds); err != nil {
		return err
	}
	m.d.Audit.Record(r.Context(), "cast.device.pair", "cast_device", e.dev.ID, map[string]any{"name": e.dev.Name})
	return httpx.NoContent(w)
}

// DELETE /cast/devices/{id}/pairing (admin). The device does not have to be visible right
// now (it may be switched off).
func (m *Module) forgetPairing(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	res, err := m.d.DB.SQL.ExecContext(r.Context(), `DELETE FROM cast_pairings WHERE device_id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return core.NotFound("This device is not paired")
	}
	m.d.Audit.Record(r.Context(), "cast.device.unpair", "cast_device", id, nil)
	return httpx.NoContent(w)
}

// isPaired reports whether pairing credentials are stored for a device.
func (m *Module) isPaired(ctx context.Context, deviceID string) bool {
	var one int
	err := m.d.DB.SQL.QueryRowContext(ctx, `SELECT 1 FROM cast_pairings WHERE device_id = ?`, deviceID).Scan(&one)
	return err == nil
}
