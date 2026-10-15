// Package store is press's half of the tenant-isolation guarantee: the key
// layout. Every object press writes lives under a tenant prefix, and the
// prefix is derived from the caller's verified identity — never from the
// request. This file's pure functions are therefore the whole boundary, and
// they are tested as pure functions, because a boundary tested only through
// the S3 client is a boundary tested one bug at a time.
package store

import (
	"fmt"
	"regexp"
	"strings"
)

// A tenant is an organization on the one Postgres cluster (identity's
// account). The string form is validated at the API seam; inside this package
// it is opaque.
type tenant = string

// Kind is the content family an object belongs to. It is part of the key, so
// an artifact's kind is visible in its path and a mistaken copy cannot move
// a private object into a public tree.
type Kind string

const (
	// KindDocs is public documentation. It is served at the site root with
	// no auth, which is why its prefix is public-by-convention and nothing
	// in it may be private.
	KindDocs Kind = "docs"
	// KindNotes is private documentation. Its objects are served only
	// behind identity's JWT check.
	KindNotes Kind = "notes"
	// KindPantry is a pantry service's published documentation, versioned
	// with the package.
	KindPantry Kind = "pantry"
)

// Valid reports whether k is a kind this layout knows.
func (k Kind) Valid() bool {
	return k == KindDocs || k == KindNotes || k == KindPantry
}

// Private reports whether objects of this kind are private. It is the one
// place the words "docs is public, notes is private" are written down, and
// everything that cares (the deploy, the readiness check, a future cache
// header) reads it here rather than restating it.
func (k Kind) Private() bool {
	return k == KindNotes
}

// segment matches one path segment press will put in a key. The fleet's names
// are lowercase alphanumeric with dashes, so this is the union of them plus
// pantry's version-ish characters — deliberately tighter than S3 allows,
// because a key that parses is a key that can be reasoned about.
var segment = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

func checkSegment(s string) error {
	if len(s) > 128 {
		return fmt.Errorf("segment %q is too long", s)
	}
	if !segment.MatchString(s) {
		return fmt.Errorf("segment %q is not a name press would put in a key", s)
	}
	if strings.Contains(s, "..") {
		return fmt.Errorf("segment %q contains '..'", s)
	}
	return nil
}

func checkTenant(t string) error {
	if err := checkSegment(t); err != nil {
		return fmt.Errorf("tenant %q is not a valid tenant id: %w", t, err)
	}
	return nil
}

// IngestKey is where a received tarball lands. The receipt id is generated
// server-side and never taken from the request, so a caller cannot aim its
// own upload at another tenant's receipt id.
func IngestKey(t, receiptID string) (string, error) {
	if err := checkTenant(t); err != nil {
		return "", err
	}
	if err := checkSegment(receiptID); err != nil {
		return "", fmt.Errorf("receipt id %q is not a valid receipt id: %w", receiptID, err)
	}
	return fmt.Sprintf("tenants/%s/ingest/%s.tar.gz", t, receiptID), nil
}

// ArtifactPrefix is where a kind's built artifacts land for a tenant. It is
// the directory nginx mounts: tenants/<t>/dist/docs at /, tenants/<t>/dist/
// notes at /notes. There is no key outside a tenant prefix, so the question
// "can tenant A read tenant B's artifacts" is answered by the directory
// layout and not by a policy someone has to remember to write.
func ArtifactPrefix(t string, k Kind) (string, error) {
	if !k.Valid() {
		return "", fmt.Errorf("kind %q is not a kind press builds", k)
	}
	if err := checkTenant(t); err != nil {
		return "", err
	}
	return fmt.Sprintf("tenants/%s/dist/%s/", t, k), nil
}

// PantryArtifactPrefix is a pantry service's artifacts, versioned under its
// own name. A pantry service is a project inside a tenant, so the prefix is
// one level deeper and the project name is validated the same way.
func PantryArtifactPrefix(t, project, version string) (string, error) {
	if err := checkTenant(t); err != nil {
		return "", err
	}
	if err := checkSegment(project); err != nil {
		return "", fmt.Errorf("project %q is not a valid project name: %w", project, err)
	}
	if err := checkSegment(version); err != nil {
		return "", fmt.Errorf("version %q is not a valid version: %w", version, err)
	}
	return fmt.Sprintf("tenants/%s/dist/pantry/%s/%s/", t, project, version), nil
}
