// Package settings is the namespaced, admin-editable settings store. Each namespace is a
// JSON object whose shape is defined by a Go struct registered at startup. Only values that
// differ from the defaults are persisted (table settings(ns, json, updated_at)), so changing
// a default in code takes effect for installs that never overrode it. Secret fields are
// AES-256-GCM encrypted at rest and redacted to {"set": bool} in the public view.
package settings

import (
	"bytes"
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"ludworks/internal/db"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrations returns the settings table migrations (module "settings").
func Migrations() fs.FS {
	sub, _ := fs.Sub(migrationsFS, "migrations")
	return sub
}

// Store is safe for concurrent use.
type Store struct {
	db     *db.DB
	master []byte
	cipher *Cipher

	mu        sync.RWMutex
	regs      map[string]*registration
	cache     map[string][]byte // ns -> canonical merged JSON (secrets decrypted)
	listeners []func(ns string)

	writeMu sync.Mutex // serialises read-modify-write updates
}

type registration struct {
	ns       string
	typ      reflect.Type // struct type
	defaults map[string]json.RawMessage
	fields   map[string]bool
	secrets  map[string]bool
	validate func(any) error
}

// New creates the store, applying its own migration. masterKey is the 32-byte server secret.
func New(ctx context.Context, d *db.DB, masterKey []byte) (*Store, error) {
	if len(masterKey) != KeySize {
		return nil, errors.New("settings: master key must be 32 bytes")
	}
	c, err := NewCipher(DeriveKey(masterKey, "settings-secrets-v1"))
	if err != nil {
		return nil, err
	}
	if err := d.Migrate(ctx, "settings", Migrations()); err != nil {
		return nil, err
	}
	s := &Store{
		db:     d,
		master: append([]byte(nil), masterKey...),
		cipher: c,
		regs:   map[string]*registration{},
		cache:  map[string][]byte{},
	}
	RegisterCore(s)
	return s, nil
}

// Cipher returns an AES-256-GCM cipher (derived from the server secret) that other modules
// may use for their own secrets at rest, e.g. TOTP secrets: Seal(secret, "totp:"+userID).
func (s *Store) Cipher() *Cipher { return s.cipher }

// DeriveKey derives a purpose-specific 32-byte key from the server secret.
func (s *Store) DeriveKey(purpose string) []byte { return DeriveKey(s.master, purpose) }

// Register declares namespace ns. defaults must be a struct (or pointer to one) with json
// tags; its type defines the namespace schema. secretFields are JSON names of string fields
// stored encrypted. validate (may be nil) receives a *T holding the merged candidate value
// and may normalise it in place; return a FieldErrors / *ValidationError for per-field
// messages. Register panics on programmer errors (duplicate ns, bad types) at startup.
func (s *Store) Register(ns string, defaults any, secretFields []string, validate func(any) error) {
	t := reflect.TypeOf(defaults)
	v := reflect.ValueOf(defaults)
	if t != nil && t.Kind() == reflect.Pointer {
		t, v = t.Elem(), v.Elem()
	}
	if t == nil || t.Kind() != reflect.Struct {
		panic(fmt.Sprintf("settings: namespace %q defaults must be a struct", ns))
	}
	if ns == "" || strings.ContainsAny(ns, ". ") {
		panic(fmt.Sprintf("settings: invalid namespace %q", ns))
	}
	raw, err := json.Marshal(v.Interface())
	if err != nil {
		panic(fmt.Sprintf("settings: namespace %q: %v", ns, err))
	}
	var defs map[string]json.RawMessage
	if err := json.Unmarshal(raw, &defs); err != nil {
		panic(fmt.Sprintf("settings: namespace %q: %v", ns, err))
	}
	r := &registration{ns: ns, typ: t, defaults: defs, fields: map[string]bool{}, secrets: map[string]bool{}, validate: validate}
	for k := range defs {
		r.fields[k] = true
	}
	for _, f := range secretFields {
		if !r.fields[f] {
			panic(fmt.Sprintf("settings: namespace %q: secret field %q not found", ns, f))
		}
		var probe string
		if json.Unmarshal(defs[f], &probe) != nil {
			panic(fmt.Sprintf("settings: namespace %q: secret field %q must be a string", ns, f))
		}
		r.secrets[f] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.regs[ns]; dup {
		panic(fmt.Sprintf("settings: namespace %q registered twice", ns))
	}
	s.regs[ns] = r
}

// Registered reports whether ns has been registered.
func (s *Store) Registered(ns string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.regs[ns]
	return ok
}

// Namespaces lists registered namespaces, sorted.
func (s *Store) Namespaces() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.regs))
	for ns := range s.regs {
		out = append(out, ns)
	}
	sort.Strings(out)
	return out
}

// OnChange registers fn to be called (synchronously, after commit) with the namespace name
// whenever a namespace is updated. Keep fn fast; spawn work if needed.
func (s *Store) OnChange(fn func(ns string)) {
	s.mu.Lock()
	s.listeners = append(s.listeners, fn)
	s.mu.Unlock()
}

// ErrUnknownNamespace is returned by Get for unregistered namespaces.
var ErrUnknownNamespace = errors.New("settings: unknown namespace")

// Get decodes the effective settings of ns (defaults overlaid with stored values, secrets
// decrypted) into dst, which should be a pointer to the registered struct type.
func (s *Store) Get(ctx context.Context, ns string, dst any) error {
	b, err := s.merged(ctx, ns)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, dst)
}

func (s *Store) reg(ns string) (*registration, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.regs[ns]
	return r, ok
}

func (s *Store) merged(ctx context.Context, ns string) ([]byte, error) {
	r, ok := s.reg(ns)
	if !ok {
		return nil, fmt.Errorf("%w %q", ErrUnknownNamespace, ns)
	}
	s.mu.RLock()
	b, ok := s.cache[ns]
	s.mu.RUnlock()
	if ok {
		return b, nil
	}
	stored, err := s.loadStored(ctx, s.db.SQL, ns)
	if err != nil {
		return nil, err
	}
	m, err := s.overlay(r, stored)
	if err != nil {
		return nil, err
	}
	b, _, err = s.canonical(r, m)
	if err != nil {
		return nil, fmt.Errorf("settings: stored %s settings invalid: %w", ns, err)
	}
	s.mu.Lock()
	s.cache[ns] = b
	s.mu.Unlock()
	return b, nil
}

// loadStored returns the persisted override map (secrets still sealed).
func (s *Store) loadStored(ctx context.Context, q db.Querier, ns string) (map[string]json.RawMessage, error) {
	var raw string
	err := q.QueryRowContext(ctx, `SELECT json FROM settings WHERE ns = ?`, ns).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return map[string]json.RawMessage{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("settings: load %s: %w", ns, err)
	}
	m := map[string]json.RawMessage{}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("settings: decode %s: %w", ns, err)
	}
	return m, nil
}

// overlay merges stored overrides over defaults, decrypting secrets. Stored keys that are no
// longer part of the schema (after an upgrade) are ignored.
func (s *Store) overlay(r *registration, stored map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	m := make(map[string]json.RawMessage, len(r.defaults))
	for k, v := range r.defaults {
		m[k] = v
	}
	for k, v := range stored {
		if !r.fields[k] {
			continue
		}
		if r.secrets[k] {
			var sealed string
			if err := json.Unmarshal(v, &sealed); err != nil {
				return nil, fmt.Errorf("settings: %s.%s: bad sealed value", r.ns, k)
			}
			pt, err := s.cipher.Open(sealed, aadFor(r.ns, k))
			if err != nil {
				return nil, fmt.Errorf("settings: %s.%s: %w", r.ns, k, err)
			}
			v, _ = json.Marshal(string(pt))
		}
		m[k] = v
	}
	return m, nil
}

func aadFor(ns, field string) string { return "settings:" + ns + "." + field }

// canonical decodes m into the registered type (strictly) and re-encodes it, returning the
// canonical JSON and the decoded *T.
func (s *Store) canonical(r *registration, m map[string]json.RawMessage) ([]byte, any, error) {
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, nil, err
	}
	ptr := reflect.New(r.typ)
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(ptr.Interface()); err != nil {
		return nil, nil, err
	}
	out, err := json.Marshal(ptr.Interface())
	if err != nil {
		return nil, nil, err
	}
	return out, ptr.Interface(), nil
}

// Update applies a JSON merge patch to one namespace. See UpdateMany for semantics.
func (s *Store) Update(ctx context.Context, ns string, patch json.RawMessage) error {
	return s.UpdateMany(ctx, map[string]json.RawMessage{ns: patch})
}

// UpdateMany validates patches for several namespaces and commits them atomically (all or
// nothing). Patch semantics per field: omitted = keep; null = reset to default (for secret
// fields: clear); for secret fields a string sets the value ("" clears) and an object such
// as {"set":true} (the redacted form returned by Public) keeps the current value, so a GET
// response can be PATCHed back unchanged. Unknown namespaces/fields and type errors yield a
// *ValidationError keyed "ns.field".
func (s *Store) UpdateMany(ctx context.Context, patches map[string]json.RawMessage) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	verr := &ValidationError{Fields: map[string]string{}}
	newStored := map[string]map[string]json.RawMessage{}
	nss := make([]string, 0, len(patches))
	for ns := range patches {
		nss = append(nss, ns)
	}
	sort.Strings(nss)
	for _, ns := range nss {
		r, ok := s.reg(ns)
		if !ok {
			verr.Fields[ns] = "unknown settings namespace"
			continue
		}
		stored, err := s.loadStored(ctx, s.db.SQL, ns)
		if err != nil {
			return err
		}
		next, err := s.applyPatch(r, stored, patches[ns], verr)
		if err != nil {
			return err
		}
		if next != nil {
			newStored[ns] = next
		}
	}
	if len(verr.Fields) > 0 {
		return verr
	}
	now := db.FormatTime(time.Now())
	err := s.db.Tx(ctx, func(tx *sql.Tx) error {
		for _, ns := range nss {
			raw, err := json.Marshal(newStored[ns])
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO settings (ns, json, updated_at) VALUES (?, ?, ?)
				ON CONFLICT (ns) DO UPDATE SET json = excluded.json, updated_at = excluded.updated_at`,
				ns, string(raw), now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("settings: save: %w", err)
	}
	s.mu.Lock()
	for _, ns := range nss {
		delete(s.cache, ns)
	}
	listeners := append([]func(string){}, s.listeners...)
	s.mu.Unlock()
	for _, ns := range nss {
		for _, fn := range listeners {
			fn(ns)
		}
	}
	return nil
}

// applyPatch returns the new stored override map for r, recording field errors in verr.
func (s *Store) applyPatch(r *registration, stored map[string]json.RawMessage, patch json.RawMessage, verr *ValidationError) (map[string]json.RawMessage, error) {
	var p map[string]json.RawMessage
	if err := json.Unmarshal(patch, &p); err != nil || p == nil {
		verr.Fields[r.ns] = "must be a JSON object"
		return nil, nil
	}
	next := make(map[string]json.RawMessage, len(stored))
	for k, v := range stored {
		if r.fields[k] { // drop keys no longer in the schema
			next[k] = v
		}
	}
	plainSecrets := map[string]string{} // new secret values, sealed after validation
	before := len(verr.Fields)
	for k, v := range p {
		key := r.ns + "." + k
		if !r.fields[k] {
			verr.Fields[key] = "unknown setting"
			continue
		}
		trimmed := bytes.TrimSpace(v)
		isNull := bytes.Equal(trimmed, []byte("null"))
		if r.secrets[k] {
			switch {
			case isNull:
				delete(next, k)
			case len(trimmed) > 0 && trimmed[0] == '{':
				// redacted round-trip form: keep current value
			default:
				var sv string
				if err := json.Unmarshal(trimmed, &sv); err != nil {
					verr.Fields[key] = "must be a string or null"
					continue
				}
				if len(sv) > 4096 {
					verr.Fields[key] = "too long"
					continue
				}
				if sv == "" {
					delete(next, k)
				} else {
					plainSecrets[k] = sv
				}
			}
			continue
		}
		if isNull {
			delete(next, k)
			continue
		}
		next[k] = append(json.RawMessage(nil), trimmed...)
	}
	if len(verr.Fields) > before {
		return nil, nil
	}
	// Build the decrypted candidate, type-check it and run the validator.
	cand, err := s.overlay(r, without(next, r.secrets))
	if err != nil {
		return nil, err
	}
	for k := range r.secrets {
		if pv, ok := plainSecrets[k]; ok {
			cand[k], _ = json.Marshal(pv)
		} else if sv, ok := next[k]; ok {
			var sealed string
			_ = json.Unmarshal(sv, &sealed)
			pt, err := s.cipher.Open(sealed, aadFor(r.ns, k))
			if err != nil {
				return nil, fmt.Errorf("settings: %s.%s: %w", r.ns, k, err)
			}
			cand[k], _ = json.Marshal(string(pt))
		}
	}
	_, val, err := s.canonical(r, cand)
	if err != nil {
		var te *json.UnmarshalTypeError
		if errors.As(err, &te) && te.Field != "" {
			verr.Fields[r.ns+"."+te.Field] = "invalid type, expected " + te.Type.String()
		} else {
			verr.Fields[r.ns] = "invalid value: " + err.Error()
		}
		return nil, nil
	}
	if r.validate != nil {
		if err := r.validate(val); err != nil {
			var ve *ValidationError
			var fe FieldErrors
			switch {
			case errors.As(err, &ve):
				for f, msg := range ve.Fields {
					verr.Fields[r.ns+"."+f] = msg
				}
			case errors.As(err, &fe):
				for f, msg := range fe {
					verr.Fields[r.ns+"."+f] = msg
				}
			default:
				verr.Fields[r.ns] = err.Error()
			}
			return nil, nil
		}
	}
	// Persist validator-normalised values for the overridden (non-secret) fields.
	norm, _ := json.Marshal(val)
	var normMap map[string]json.RawMessage
	_ = json.Unmarshal(norm, &normMap)
	for k := range next {
		if !r.secrets[k] {
			next[k] = normMap[k]
		}
	}
	for k, pv := range plainSecrets {
		sealed, err := s.cipher.Seal([]byte(pv), aadFor(r.ns, k))
		if err != nil {
			return nil, err
		}
		next[k], _ = json.Marshal(sealed)
	}
	return next, nil
}

func without(m map[string]json.RawMessage, drop map[string]bool) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(m))
	for k, v := range m {
		if !drop[k] {
			out[k] = v
		}
	}
	return out
}

// Public returns every namespace with secret fields redacted to {"set": bool}. Namespaces
// that fail to load are omitted; use PublicView to get the error instead.
func (s *Store) Public(ctx context.Context) map[string]any {
	out, _ := s.PublicView(ctx)
	return out
}

// PublicView is Public with error reporting.
func (s *Store) PublicView(ctx context.Context) (map[string]any, error) {
	out := map[string]any{}
	var errs []error
	for _, ns := range s.Namespaces() {
		v, err := s.PublicNamespace(ctx, ns)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out[ns] = v
	}
	return out, errors.Join(errs...)
}

// PublicNamespace returns one namespace, redacted.
func (s *Store) PublicNamespace(ctx context.Context, ns string) (map[string]any, error) {
	r, ok := s.reg(ns)
	if !ok {
		return nil, fmt.Errorf("%w %q", ErrUnknownNamespace, ns)
	}
	b, err := s.merged(ctx, ns)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	for k := range r.secrets {
		str, _ := m[k].(string)
		m[k] = map[string]any{"set": str != ""}
	}
	return m, nil
}

// ValidationError carries per-field messages, keyed "ns.field" (or "ns" for whole-namespace
// problems). httpx renders it as 422 validation_failed with details.fields.
type ValidationError struct{ Fields map[string]string }

func (e *ValidationError) Error() string {
	keys := make([]string, 0, len(e.Fields))
	for k := range e.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+": "+e.Fields[k])
	}
	return "settings validation failed: " + strings.Join(parts, "; ")
}

// FieldErrors returns the field map (lets httpx render it without importing settings types).
func (e *ValidationError) FieldErrors() map[string]string { return e.Fields }

// FieldErrors is a convenience error type for validators: field name -> message.
type FieldErrors map[string]string

func (f FieldErrors) Error() string { return (&ValidationError{Fields: f}).Error() }

// Err returns nil when there are no field errors.
func (f FieldErrors) Err() error {
	if len(f) == 0 {
		return nil
	}
	return f
}

// Validator adapts a typed validation function to Register's func(any) error.
func Validator[T any](fn func(*T) error) func(any) error {
	return func(v any) error {
		t, ok := v.(*T)
		if !ok {
			return fmt.Errorf("settings: validator got %T", v)
		}
		return fn(t)
	}
}
