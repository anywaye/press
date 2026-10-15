package store

import (
	"strings"
	"testing"
)

// The key layout is the tenant-isolation boundary, so it is tested as a pure
// function: every rule a reader would have to trust is one assertion here.

func TestIngestKeyIsTheCallersOwnPrefixAndNothingElse(t *testing.T) {
	key, err := IngestKey("acme", "abc123")
	if err != nil {
		t.Fatalf("ingest key: %v", err)
	}
	if want := "tenants/acme/ingest/abc123.tar.gz"; key != want {
		t.Fatalf("key = %q, want %q", key, want)
	}
}

func TestIngestKeyRefusesATenantThatIsNotOneSegment(t *testing.T) {
	for _, bad := range []string{"", "a/b", "../other", "..", "A Org", ".hidden", "org\t"} {
		if _, err := IngestKey(bad, "r1"); err == nil {
			t.Fatalf("tenant %q was accepted", bad)
		}
	}
}

func TestArtifactPrefixMatchesWhatNginxMounts(t *testing.T) {
	docs, err := ArtifactPrefix("acme", KindDocs)
	if err != nil {
		t.Fatalf("docs prefix: %v", err)
	}
	notes, err := ArtifactPrefix("acme", KindNotes)
	if err != nil {
		t.Fatalf("notes prefix: %v", err)
	}
	// The two trees of the docs build, mounted at / and /notes.
	if docs != "tenants/acme/dist/docs/" {
		t.Fatalf("docs prefix = %q", docs)
	}
	if notes != "tenants/acme/dist/notes/" {
		t.Fatalf("notes prefix = %q", notes)
	}
	// A private kind never shares a prefix with a public one.
	if strings.HasPrefix(notes, docs) || strings.HasPrefix(docs, notes) {
		t.Fatal("public and private artifact prefixes overlap")
	}
}

func TestPantryArtifactPrefixIsProjectAndVersionScoped(t *testing.T) {
	key, err := PantryArtifactPrefix("acme", "payments", "v1.2.3")
	if err != nil {
		t.Fatalf("pantry prefix: %v", err)
	}
	if want := "tenants/acme/dist/pantry/payments/v1.2.3/"; key != want {
		t.Fatalf("key = %q, want %q", key, want)
	}
	for _, bad := range [][2]string{{"../other", "v1"}, {"a/b", "v1"}, {"payments", "../v1"}} {
		if _, err := PantryArtifactPrefix("acme", bad[0], bad[1]); err == nil {
			t.Fatalf("project/version %v was accepted", bad)
		}
	}
}

func TestKindKnowsWhatIsPrivate(t *testing.T) {
	// The one place public and private are written down; everything that
	// cares reads it here.
	if !KindNotes.Private() {
		t.Fatal("notes must be private")
	}
	if KindDocs.Private() || KindPantry.Private() {
		t.Fatal("docs and pantry artifacts are public by kind; the gating happens at serve, not here")
	}
	if (Kind("articles")).Valid() {
		t.Fatal("articles is not a kind yet — it is the next content kind, and a layout entry for it belongs in the commit that ships it")
	}
}
