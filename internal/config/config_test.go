package config

import "testing"

func TestValidateRejectsNonHTTPExportURL(t *testing.T) {
	for _, url := range []string{"ftp://agg:8080/x", "agg:8080/admin/usage/import", "file:///tmp/x"} {
		c := &Config{Usage: UsageCfg{ExportURL: url}}
		if err := c.Validate(); err == nil {
			t.Fatalf("export_url %q must be rejected", url)
		}
	}
	c := &Config{Usage: UsageCfg{ExportURL: "https://agg:8080/admin/usage/import"}}
	if err := c.Validate(); err != nil {
		t.Fatalf("https export_url must pass: %v", err)
	}
}
