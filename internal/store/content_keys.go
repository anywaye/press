package store

import "fmt"

// ContentKey is where a single content file lands for a tenant. Sections are
// "docs", "articles", "drafts/docs", "drafts/articles" and "notes"; the slug
// is the file's stem without the .md suffix.
func ContentKey(tenant, section, slug string) (string, error) {
	if err := checkTenant(tenant); err != nil {
		return "", err
	}
	if !validSection(section) {
		return "", fmt.Errorf("section %q is not a section press stores", section)
	}
	if err := checkSegment(slug); err != nil {
		return "", fmt.Errorf("slug %q is not a valid slug: %w", slug, err)
	}
	return fmt.Sprintf("tenants/%s/content/%s/%s.md", tenant, section, slug), nil
}

// ContentPrefix is the prefix under which a section's files live, for listing.
func ContentPrefix(tenant, section string) (string, error) {
	if err := checkTenant(tenant); err != nil {
		return "", err
	}
	if !validSection(section) {
		return "", fmt.Errorf("section %q is not a section press stores", section)
	}
	return fmt.Sprintf("tenants/%s/content/%s/", tenant, section), nil
}

// ThemeKey is where a tenant's theme configuration lives.
func ThemeKey(tenant string) (string, error) {
	if err := checkTenant(tenant); err != nil {
		return "", err
	}
	return fmt.Sprintf("tenants/%s/theme.json", tenant), nil
}

// ManifestKey is where a tenant's content manifest lives.
func ManifestKey(tenant string) (string, error) {
	if err := checkTenant(tenant); err != nil {
		return "", err
	}
	return fmt.Sprintf("tenants/%s/content/manifest.json", tenant), nil
}

func validSection(s string) bool {
	switch s {
	case "docs", "articles", "drafts/docs", "drafts/articles", "notes":
		return true
	}
	return false
}
