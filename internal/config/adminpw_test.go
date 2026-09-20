package config

// Admin password resolution (adminpw.go): the stored-file read that every
// Load performs, and the boot-only generation that persists a fresh
// credential when nothing was configured.

import (
	"os"
	"path/filepath"
	"testing"
)

func clearAdminPWEnv(t *testing.T) {
	t.Helper()
	t.Setenv("ONEGW_ADMIN_PASSWORD", "")
	t.Setenv("ONEGW_DATA_DIR", "")
}

// Generation is boot-only: two successive loads of an unconfigured file must
// agree, because Load also runs on every SIGHUP / HTTP reload and a rotating
// password would kill every session and strand the operator between boots.
func TestAdminPasswordGeneratedOnceThenReadBack(t *testing.T) {
	clearAdminPWEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "onegw.toml")
	if err := os.WriteFile(path, []byte("[server]\nlisten = \"127.0.0.1:0\"\ndata_dir = \""+dir+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Load alone never writes: the fallback stays in place until startup asks.
	first, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if first.Server.AdminPassword != "admin" {
		t.Fatalf("Load must not generate, got %q", first.Server.AdminPassword)
	}
	if _, err := os.Stat(filepath.Join(dir, AdminPasswordFile)); !os.IsNotExist(err) {
		t.Fatal("Load wrote a password file; generation belongs to startup")
	}

	// The startup tail mints + persists once.
	gen, err := first.GenerateAndStoreAdminPassword()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if !gen {
		t.Fatal("unconfigured first boot must report a generated password")
	}
	pw := first.Server.AdminPassword
	if pw == "" || pw == "admin" {
		t.Fatalf("generated password %q is the fallback", pw)
	}
	if !first.AdminPasswordGenerated() {
		t.Fatal("AdminPasswordGenerated must be true after minting")
	}
	fi, err := os.Stat(filepath.Join(dir, AdminPasswordFile))
	if err != nil {
		t.Fatalf("marker file: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("marker file mode %v, want 0600", fi.Mode().Perm())
	}

	// Every later load (reload, restart) reads the same credential.
	second, err := Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if second.Server.AdminPassword != pw {
		t.Fatalf("password changed under reload: %q -> %q", pw, second.Server.AdminPassword)
	}
	if !second.AdminPasswordConfigured() {
		t.Fatal("a stored credential must count as configured")
	}
	// And startup asking again is a no-op, not a rotation.
	if gen, err := second.GenerateAndStoreAdminPassword(); err != nil || gen {
		t.Fatalf("second boot regenerated: gen=%v err=%v", gen, err)
	}
}

func TestAdminPasswordExplicitConfigWins(t *testing.T) {
	clearAdminPWEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "onegw.toml")
	if err := os.WriteFile(path, []byte("[server]\ndata_dir = \""+dir+"\"\nadmin_password = \"chosen-long-enough\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if gen, err := cfg.GenerateAndStoreAdminPassword(); err != nil || gen {
		t.Fatalf("generated over an explicit password: gen=%v err=%v", gen, err)
	}
	if cfg.Server.AdminPassword != "chosen-long-enough" {
		t.Fatalf("explicit password overridden: %q", cfg.Server.AdminPassword)
	}
	if _, err := os.Stat(filepath.Join(dir, AdminPasswordFile)); !os.IsNotExist(err) {
		t.Fatal("password file written despite an explicit config password")
	}
}

// Env outranks a stored file: the operator's ONEGW_ADMIN_PASSWORD must not be
// silently replaced by whatever an earlier boot left in the data dir.
func TestAdminPasswordEnvBeatsStoredFile(t *testing.T) {
	clearAdminPWEnv(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, AdminPasswordFile), []byte("stale-from-boot\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ONEGW_ADMIN_PASSWORD", "from-env")
	path := filepath.Join(dir, "onegw.toml")
	if err := os.WriteFile(path, []byte("[server]\ndata_dir = \""+dir+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.AdminPassword != "from-env" {
		t.Fatalf("stored file outran env: %q", cfg.Server.AdminPassword)
	}
}

// data_dir = "memory" is the in-store sentinel every file-owning subsystem
// honors: no generation, no disk, no writes into the operator's ~/.onegw.
func TestAdminPasswordMemorySentinelSkipsDisk(t *testing.T) {
	clearAdminPWEnv(t)
	cfg := &Config{}
	cfg.Defaults()
	cfg.Server.DataDir = "memory"
	if gen, err := cfg.GenerateAndStoreAdminPassword(); err != nil || gen {
		t.Fatalf("memory sentinel generated a password: gen=%v err=%v", gen, err)
	}
	if cfg.Server.AdminPassword != "admin" {
		t.Fatalf("memory sentinel changed the password: %q", cfg.Server.AdminPassword)
	}
}

func TestAdminPasswordStoredFileRestoredOnLoad(t *testing.T) {
	clearAdminPWEnv(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, AdminPasswordFile), []byte("stored-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "onegw.toml")
	if err := os.WriteFile(path, []byte("[server]\ndata_dir = \""+dir+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.AdminPassword != "stored-secret" {
		t.Fatalf("stored password not adopted on load: %q", cfg.Server.AdminPassword)
	}
}

// A read-only data dir must not downgrade the gateway to the guessable
// "admin": the generated credential still guards this boot, and the caller
// is told it could not be persisted.
func TestAdminPasswordUnwritableDataDirStillGenerates(t *testing.T) {
	clearAdminPWEnv(t)
	dir := filepath.Join(t.TempDir(), "locked")
	if err := os.MkdirAll(dir, 0o500); err != nil { // r-x: nothing creatable inside
		t.Fatal(err)
	}
	cfg := &Config{}
	cfg.Defaults()
	cfg.Server.DataDir = dir
	gen, perr := cfg.GenerateAndStoreAdminPassword()
	if !gen {
		t.Fatalf("read-only data dir skipped generation (err=%v)", perr)
	}
	if perr == nil {
		t.Fatal("persist failure must be reported")
	}
	if cfg.Server.AdminPassword == "" || cfg.Server.AdminPassword == "admin" {
		t.Fatalf("password fell back to the default: %q", cfg.Server.AdminPassword)
	}
	_ = os.Chmod(dir, 0o700)
}
