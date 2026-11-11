package store

import "testing"

func TestContentKeysShareTheContentRoot(t *testing.T) {
	tenant := "acme"

	key, err := ContentKey(tenant, "docs", "guides/quickstart")
	if err != nil {
		t.Fatalf("ContentKey: %v", err)
	}
	if want := "tenants/acme/content/docs/guides/quickstart.md"; key != want {
		t.Fatalf("ContentKey = %q, want %q", key, want)
	}

	theme, err := ThemeKey(tenant)
	if err != nil {
		t.Fatalf("ThemeKey: %v", err)
	}
	if want := "tenants/acme/content/theme.json"; theme != want {
		t.Fatalf("ThemeKey = %q, want %q", theme, want)
	}

	manifest, err := ManifestKey(tenant)
	if err != nil {
		t.Fatalf("ManifestKey: %v", err)
	}
	if want := "tenants/acme/content/manifest.json"; manifest != want {
		t.Fatalf("ManifestKey = %q, want %q", manifest, want)
	}
}

func TestContentKeyRejectsBadInput(t *testing.T) {
	if _, err := ContentKey("acme", "blog", "hello"); err == nil {
		t.Fatal("unknown section was accepted")
	}
	for _, slug := range []string{"../other", "docs/../other", "docs//other", "/docs", "docs/"} {
		if _, err := ContentKey("acme", "docs", slug); err == nil {
			t.Fatalf("slug %q was accepted", slug)
		}
	}
	if _, err := ThemeKey("../acme"); err == nil {
		t.Fatal("bad tenant was accepted")
	}
}
