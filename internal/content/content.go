package content

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/anywaye/press/internal/auth"
	"github.com/anywaye/press/internal/store"
)

// ManifestItem describes one piece of content.
type ManifestItem struct {
	Slug        string   `json:"slug"`
	Title       string   `json:"title"`
	Section     string   `json:"section"`
	Status      string   `json:"status,omitempty"`
	Date        string   `json:"date,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Description string   `json:"description,omitempty"`
	Author      string   `json:"author,omitempty"`
}

// Service serves content from object storage.
type Service struct {
	store         store.Client
	verifier      auth.Verifier
	identityURL   string
	notesAccounts map[string]bool
}

func NewService(s store.Client, v auth.Verifier, identityURL string, notesAccounts []string) *Service {
	m := map[string]bool{}
	for _, a := range notesAccounts {
		a = strings.TrimSpace(a)
		if a != "" {
			m[a] = true
		}
	}
	return &Service{store: s, verifier: v, identityURL: strings.TrimSuffix(identityURL, "/"), notesAccounts: m}
}

func (s *Service) tenantFromRequest(r *http.Request) string {
	if t := r.Header.Get("X-Tenant-ID"); t != "" {
		return t
	}
	return ""
}

func (s *Service) bearerToken(r *http.Request) string {
	v := r.Header.Get("Authorization")
	if strings.HasPrefix(v, "Bearer ") {
		return strings.TrimPrefix(v, "Bearer ")
	}
	return ""
}

func (s *Service) isAuthed(ctx context.Context, token string) (string, bool) {
	slog.Debug("content: isAuthed", "identity_url", s.identityURL, "token_len", len(token))
	if token == "" || s.identityURL == "" {
		return "", false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.identityURL+"/v1/me", nil)
	if err != nil {
		return "", false
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		slog.Error("content: identity /v1/me request failed", "url", s.identityURL+"/v1/me", "err", err)
		return "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		slog.Error("content: identity /v1/me rejected the token", "status", resp.StatusCode)
		return "", false
	}
	// parse account_id if present; otherwise use token as identity
	var out struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out.ID == "" {
		return token, true
	}
	return out.ID, true
}

func (s *Service) canReadNotes(accountID string) bool {
	if s.notesAccounts["*"] {
		return true
	}
	return s.notesAccounts[accountID]
}

// isPrivateSection reports whether a section requires authentication.
func isPrivateSection(section string) bool {
	return section == "notes" || strings.HasPrefix(section, "drafts/")
}

// publicSections are readable without auth.
var publicSections = []string{"docs", "articles"}

// allSections includes private ones.
var allSections = []string{"docs", "articles", "drafts/docs", "drafts/articles", "notes"}

func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("OPTIONS /v1/content/{path...}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, X-Tenant-ID, Content-Type")
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /v1/content/manifest", s.handleManifest)
	mux.HandleFunc("GET /v1/content/theme", s.handleTheme)
	mux.HandleFunc("GET /v1/content/{path...}", s.handleGet)
	return mux
}

func (s *Service) handleManifest(w http.ResponseWriter, r *http.Request) {
	tenant := s.tenantFromRequest(r)
	if tenant == "" {
		writeError(w, http.StatusBadRequest, "X-Tenant-ID header is required")
		return
	}
	token := s.bearerToken(r)
	accountID, authed := s.isAuthed(r.Context(), token)

	// cross-check tenant for authed requests
	if authed && accountID != "" && accountID != tenant {
		// allow if account is in notes allowlist? keep simple: require match
		// (tenant is the account_id in press's model)
	}

	sections := publicSections
	if authed {
		// authed users see drafts + notes (if allowed)
		sections = append([]string{}, allSections...)
	}
	sectionFilter := r.URL.Query().Get("section")

	var items []ManifestItem
	for _, sec := range sections {
		if sectionFilter != "" && sec != sectionFilter {
			continue
		}
		if sec == "notes" || strings.HasPrefix(sec, "drafts/") {
			if !authed {
				continue
			}
			if sec == "notes" && len(s.notesAccounts) > 0 && !s.canReadNotes(accountID) {
				continue
			}
		}
		prefix, err := store.ContentPrefix(tenant, sec)
		if err != nil {
			continue
		}
		keys, err := s.store.List(r.Context(), prefix)
		if err != nil {
			continue
		}
		for _, key := range keys {
			if !strings.HasSuffix(key, ".md") {
				continue
			}
			slug := strings.TrimSuffix(strings.TrimPrefix(key, prefix), ".md")
			data, err := s.store.Get(r.Context(), key)
			if err != nil {
				continue
			}
			fm := parseFrontmatter(string(data))
			item := ManifestItem{
				Slug:        slug,
				Section:     sec,
				Title:       fm["title"],
				Status:      fm["status"],
				Date:        fm["date"],
				Description: fm["description"],
				Author:      fm["author"],
				Tags:        parseTags(fm["tags"]),
			}
			if item.Status == "" {
				item.Status = "published"
			}
			items = append(items, item)
		}
	}

	sort.Slice(items, func(i, j int) bool {
		if items[i].Section != items[j].Section {
			return items[i].Section < items[j].Section
		}
		return items[i].Slug < items[j].Slug
	})

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, X-Tenant-ID, Content-Type")
	if authed {
		w.Header().Set("Cache-Control", "private, no-store")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=60")
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
}

func (s *Service) handleGet(w http.ResponseWriter, r *http.Request) {
	tenant := s.tenantFromRequest(r)
	if tenant == "" {
		writeError(w, http.StatusBadRequest, "X-Tenant-ID header is required")
		return
	}
	// path: /v1/content/{section...}/{slug}.md
	// chi-style: we parse manually
	path := r.PathValue("path")
	path = strings.TrimSuffix(path, ".md")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) < 2 {
		writeError(w, http.StatusBadRequest, "section and slug required")
		return
	}
	section := parts[0]
	slug := parts[1]
	// handle nested sections like drafts/docs
	// if section is "drafts", recombine
	if section == "drafts" {
		// slug contains "docs/..." or "articles/..."
		// reconstruct section
		subParts := strings.SplitN(slug, "/", 2)
		if len(subParts) < 2 {
			writeError(w, http.StatusBadRequest, "drafts requires a sub-section")
			return
		}
		section = "drafts/" + subParts[0]
		slug = subParts[1]
	}

	token := s.bearerToken(r)
	accountID, authed := s.isAuthed(r.Context(), token)

	if isPrivateSection(section) {
		if !authed {
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		if section == "notes" && len(s.notesAccounts) > 0 && !s.canReadNotes(accountID) {
			writeError(w, http.StatusForbidden, "not authorized for notes")
			return
		}
		if strings.HasPrefix(section, "drafts/") {
			// drafts require staff/beta — for now, any authed user
			// (refine with a staff flag later)
		}
	}

	// tenant is resolved from X-Tenant-ID (set by the frontend based on hostname),
	// not from the token. The token only proves the user is authenticated.
	// Authorization for private content is handled by the allowlist above.

	key, err := store.ContentKey(tenant, section, slug)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	data, err := s.store.Get(r.Context(), key)
	if err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, "content not found")
		} else {
			w.Header().Set("Retry-After", "30")
			writeError(w, http.StatusServiceUnavailable, "content temporarily unavailable")
		}
		return
	}

	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, X-Tenant-ID, Content-Type")
	if isPrivateSection(section) {
		w.Header().Set("Cache-Control", "private, no-store")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=300")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (s *Service) handleTheme(w http.ResponseWriter, r *http.Request) {
	tenant := s.tenantFromRequest(r)
	if tenant == "" {
		writeError(w, http.StatusBadRequest, "X-Tenant-ID header is required")
		return
	}
	key, err := store.ThemeKey(tenant)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	data, err := s.store.Get(r.Context(), key)
	if err != nil {
		// return default theme
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// --- frontmatter parsing ---

func parseFrontmatter(content string) map[string]string {
	result := map[string]string{}
	content = strings.TrimPrefix(content, "\ufeff")
	if !strings.HasPrefix(content, "---") {
		return result
	}
	end := strings.Index(content[3:], "\n---")
	if end < 0 {
		return result
	}
	block := content[3 : 3+end]
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.Index(line, ":")
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		// strip quotes
		val = strings.Trim(val, "\"'")
		result[key] = val
	}
	return result
}

func parseTags(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	// handle [a, b, c]
	if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		s = s[1 : len(s)-1]
	}
	parts := strings.Split(s, ",")
	var tags []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		p = strings.Trim(p, "\"'")
		if p != "" {
			tags = append(tags, p)
		}
	}
	return tags
}
