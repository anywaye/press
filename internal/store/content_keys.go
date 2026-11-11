package store

import (
	"fmt"
	"strings"
)

// ContentKey is where a single content file lands for a tenant. Sections are
// "docs", "articles", "drafts/docs", "drafts/articles" and "notes"; the slug
// is the file's stem without the .md suffix and may carry nested directories.
func ContentKey(tenant, section, slug string) (string, error) {
	if err := checkTenant(tenant); err != nil {
		return "", err
	}
	if !validSection(section) {
		return "", fmt.Errorf("section %q is not a section press stores", section)
	}
	if err := checkSlug(slug); err != nil {
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

// ThemeKey is where a tenant's theme configuration lives. Theme sits beside the
// content sections under the same content root, because that is where the
// publisher puts it.
func ThemeKey(tenant string) (string, error) {
	if err := checkTenant(tenant); err != nil {
		return "", err
	}
	return fmt.Sprintf("tenants/%s/content/theme.json", tenant), nil
}

// ManifestKey is where a tenant's content manifest lives.
func ManifestKey(tenant string) (string, error) {
	if err := checkTenant(tenant); err != nil {
		return "", err
	}
	return fmt.Sprintf("tenants/%s/content/manifest.json", tenant), nil
}

// checkSlug validates a slug that may contain nested directories. Each
// segment must satisfy checkSegment, so "docs/guides/quickstart" is allowed
// while "docs/../other" and "docs//x" are not.
func checkSlug(s string) error {
	if len(s) > 256 {
		return fmt.Errorf("slug %q is too long", s)
	}
	if strings.HasPrefix(s, "/") || strings.HasSuffix(s, "/") || strings.Contains(s, "//") {
		return fmt.Errorf("slug %q has an empty path segment", s)
	}
	for _, seg := range strings.Split(s, "/") {
		if err := checkSegment(seg); err != nil {
			return err
		}
	}
	return nil
}

func validSection(s string) bool {
	switch s {
	case "docs", "articles", "drafts/docs", "drafts/articles", "notes":
		return true
	}
	return false
}
