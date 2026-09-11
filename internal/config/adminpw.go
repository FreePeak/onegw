package config

// Admin password resolution.
//
// A gateway installed without an explicit admin_password — a bare `onegw`
// run, or the docker image whose baked config ships the key empty — used to
// fall back to the literal "admin", a credential every scanner guesses.
// Instead the process mints a random password on first boot, stores it at
// <data_dir>/admin_password (0600) so restarts keep the same value, and
// startup logs it once so the operator can read it off the console (or
// `docker logs`).
//
// Precedence (highest first):
//
//	admin_password in the TOML  >  ONEGW_ADMIN_PASSWORD  >  <data_dir>/admin_password  >  generated on first boot (and persisted)  >  "admin"
//
// The ladder is split in two because config.Load is ALSO a validator
// (writeConfigAtomically round-trips candidate content through it):
//
//   - UseStoredAdminPassword is read-only and runs inside every Load, so a
//     SIGHUP/HTTP reload of a password-less config keeps the stored
//     credential instead of silently downgrading to "admin".
//   - GenerateAndStoreAdminPassword writes, and is called only from gateway
//     startup (cmd/onegw/main.go), never from a validation path or the CLI.
//
// data_dir = "memory" (the in-store test sentinel) and an unset data dir
// skip disk entirely, which also keeps every test that builds a Config by
// hand out of the operator's real ~/.onegw.

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// AdminPasswordFile is the data-dir file holding the effective admin
// password when nothing was configured.
const AdminPasswordFile = "admin_password"

// GenerateAdminPassword returns a fresh random credential: 16 crypto/rand
// bytes as 22 URL-safe base64 characters ([A-Za-z0-9_-]) — safe in a TOML
// basic string and as an X-Admin-Password header value.
func GenerateAdminPassword() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("admin password: crypto/rand unavailable: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

// UseStoredAdminPassword is the read-only step of the ladder: while nothing
// was configured (no config key, no env — Defaults() records both), adopt
// the password a previous boot persisted at <data_dir>/admin_password.
// Call it AFTER Defaults(), which also resolves the data dir it reads from.
// No-ops on the memory sentinel; a missing/unreadable file changes nothing.
func (c *Config) UseStoredAdminPassword() {
	if c.adminPwConfigured {
		return
	}
	b, err := os.ReadFile(c.adminPwPath())
	if err != nil {
		return
	}
	if pw := strings.TrimSpace(string(b)); pw != "" {
		c.Server.AdminPassword = pw
		c.adminPwConfigured = true
	}
}

// GenerateAndStoreAdminPassword is the boot-time tail of the ladder: with
// nothing configured anywhere (no config key, no env, no stored file) it
// mints a password, persists it, and returns generated=true so startup can
// log the credential exactly once. It refuses to touch a password an
// operator chose, and it never writes for the memory sentinel / unset data
// dir. A persistence failure comes back as err while the generated value
// STAYS applied for this boot — never downgrade to the weak default.
func (c *Config) GenerateAndStoreAdminPassword() (bool, error) {
	if c.adminPwConfigured {
		return false, nil // a key, the env value, or a stored file already wins
	}
	c.UseStoredAdminPassword()
	if c.adminPwConfigured {
		return false, nil
	}
	dir := c.Server.DataDir
	if dir == "" || dir == "memory" {
		return false, nil
	}
	pw, err := GenerateAdminPassword()
	if err != nil {
		return false, err
	}
	c.Server.AdminPassword = pw
	c.adminPwConfigured = true
	c.adminPwGenerated = true
	// A read-only data dir (e.g. a root-owned bind mount on the container's
	// /data) must NOT leave the gateway on the guessable "admin" fallback:
	// the credential still protects this boot, and the returned error tells
	// the operator it has to be re-read from the log after every restart.
	return true, WriteAdminPasswordFile(dir, pw)
}

// AdminPasswordGenerated reports that startup minted (and persisted) the
// effective admin password because nothing was configured anywhere.
func (c *Config) AdminPasswordGenerated() bool { return c != nil && c.adminPwGenerated }

// AdminPasswordConfigured reports an operator-chosen credential (config key,
// env, or a stored file) as opposed to the built-in fallback.
func (c *Config) AdminPasswordConfigured() bool { return c != nil && c.adminPwConfigured }

func (c *Config) adminPwPath() string {
	return filepath.Join(c.Server.DataDir, AdminPasswordFile)
}

// WriteAdminPasswordFile stores pw at <dir>/admin_password (0600, creating
// dir 0700 if needed). Called on first boot, and mirrored by the dashboard's
// password change so a container recreated from a config whose key is empty
// picks the operator's password up from the data volume.
func WriteAdminPasswordFile(dir, pw string) error {
	if dir == "" || dir == "memory" {
		return nil
	}
	if strings.TrimSpace(pw) == "" {
		return errors.New("refusing to store an empty admin password")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".adminpw-*.tmp")
	if err != nil {
		return fmt.Errorf("temp file: %w", err)
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op once renamed
	if _, err := fmt.Fprintf(tmp, "%s\n", pw); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, filepath.Join(dir, AdminPasswordFile))
}
