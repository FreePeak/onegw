package config

import (
	"fmt"

	"github.com/BurntSushi/toml"
)

// AuthKey is one client key with its optional policy. rpm/tpm of 0 mean
// unlimited; an empty models list allows every model.
type AuthKey struct {
	Key    string   `toml:"key"`
	Name   string   `toml:"name"`   // human label used in usage data and errors
	RPM    int      `toml:"rpm"`    // requests per minute
	TPM    int      `toml:"tpm"`    // tokens per minute
	Models []string `toml:"models"` // allowed model strings: bare model, provider/model, or combo name
}

// Label returns the non-secret label used in usage data and errors.
func (k *AuthKey) Label() string { return keyLabel(k) }

// decodeKeys resolves Auth.Raw — captured from TOML as either the legacy
// flat list (keys = ["sk-..."]) or policy tables ([[auth.keys]]) — into
// Auth.KeyList. Raw points at a value only when the TOML actually set
// auth.keys; programmatic KeyList assignment (tests, ONEGW_KEYS override)
// passes through untouched. Flat keys become unlimited entries. Idempotent:
// Raw is cleared once translated.
func (a *Auth) decodeKeys(md toml.MetaData) error {
	if a.Raw == nil {
		return nil
	}
	var flat []string
	if err := md.PrimitiveDecode(*a.Raw, &flat); err == nil {
		a.KeyList = make([]AuthKey, len(flat))
		for i, k := range flat {
			a.KeyList[i] = AuthKey{Key: k}
		}
		a.Raw = nil
		return nil
	}
	var tbls []AuthKey
	if err := md.PrimitiveDecode(*a.Raw, &tbls); err != nil {
		return fmt.Errorf("auth.keys must be a list of key strings or [[auth.keys]] tables: %w", err)
	}
	a.KeyList = tbls
	a.Raw = nil
	return nil
}

// validateKeys checks the normalized key list for the invariants the
// gateway relies on.
func validateKeys(keys []AuthKey) error {
	seen := map[string]bool{}
	for i := range keys {
		k := &keys[i]
		if k.Key == "" {
			return fmt.Errorf("auth.keys entry %d missing key", i)
		}
		if seen[k.Key] {
			return fmt.Errorf("duplicate auth key %q", maskKey(k.Key))
		}
		seen[k.Key] = true
		if k.RPM < 0 || k.TPM < 0 {
			return fmt.Errorf("auth key %s has negative rpm/tpm", keyLabel(k))
		}
		for _, m := range k.Models {
			if m == "" {
				return fmt.Errorf("auth key %s has empty models entry", keyLabel(k))
			}
		}
	}
	return nil
}

// keyLabel is the non-secret label for a key: its name when set, else a
// masked form of the raw key. Raw keys never reach usage data or errors.
func keyLabel(k *AuthKey) string {
	if k.Name != "" {
		return k.Name
	}
	return maskKey(k.Key)
}

// maskKey keeps the first four and last two characters.
func maskKey(k string) string {
	if len(k) <= 8 {
		return "***"
	}
	return k[:4] + "***" + k[len(k)-2:]
}
