package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/jchevertonwynne/homelab-go/access"
)

const (
	allowed     = "allowed@example.com"
	removed     = "removed@example.com"
	backupToken = "test-backup-token"
)

// ask issues a GET as the given address, or with no Access header at all when
// email is empty, against the real route table.
func ask(t *testing.T, h http.Handler, path, email string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	if email != "" {
		r.Header.Set(access.EmailHeader, email)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func testRoutes(t *testing.T, allowedEmails string) http.Handler {
	t.Helper()
	h, _ := routes(newTestServer(t).Server, access.NewAllowlist(allowedEmails, ""), backupToken)
	return h
}

// The case the allowlist exists for. Access issues a session and does not
// consult its policy again, so someone removed from it keeps sending a valid
// header until that session expires — a month, at this hostname's setting.
func TestRemovedAddressIsRefused(t *testing.T) {
	h := testRoutes(t, allowed)

	for _, path := range []string{"/", "/entries", "/summary", "/chart", "/export.csv", "/import", "/overnight"} {
		if rec := ask(t, h, path, removed); rec.Code != http.StatusForbidden {
			t.Errorf("GET %s as a removed address = %d, want 403", path, rec.Code)
		}
		if rec := ask(t, h, path, allowed); rec.Code == http.StatusForbidden {
			t.Errorf("GET %s as an allowed address = 403", path)
		}
	}
}

// A route added to RegisterRoutes later is behind the catch-all, and so
// authenticated, without anyone having to remember to make it so.
func TestUnknownPathIsStillAuthenticated(t *testing.T) {
	h := testRoutes(t, allowed)

	rec := ask(t, h, "/whatever-comes-next", removed)
	if rec.Code != http.StatusForbidden {
		t.Errorf("an unknown path as a removed address = %d, want 403", rec.Code)
	}
	// And an admitted caller reaches the mux, which has nothing there.
	if rec := ask(t, h, "/whatever-comes-next", allowed); rec.Code != http.StatusNotFound {
		t.Errorf("an unknown path as an allowed address = %d, want 404", rec.Code)
	}
}

// If Access were removed or bypassed the header would simply stop arriving.
// That must not read as "no identity required".
func TestNoHeaderIsRefused(t *testing.T) {
	h := testRoutes(t, allowed)

	if rec := ask(t, h, "/", ""); rec.Code != http.StatusForbidden {
		t.Errorf("GET / with no Access header = %d, want 403", rec.Code)
	}
}

// Local development: -dev-user stands in for the header, and is admitted
// whether or not the list names it.
func TestDevUserStandsInForTheHeader(t *testing.T) {
	h, _ := routes(newTestServer(t).Server, access.NewAllowlist(allowed, "dev@local"), "")

	for _, path := range []string{"/", "/backup.db"} {
		if rec := ask(t, h, path, ""); rec.Code == http.StatusForbidden {
			t.Errorf("GET %s with a devUser configured and no header = 403", path)
		}
	}
}

// The kubelet, Alloy and the browser's asset requests have no Access session
// between them, and none of these responses says anything about anybody.
func TestUnauthenticatedRoutesStayOpen(t *testing.T) {
	h := testRoutes(t, allowed)

	for _, path := range []string{"/healthz", "/metrics", "/sw.js"} {
		if rec := ask(t, h, path, ""); rec.Code != http.StatusOK {
			t.Errorf("GET %s with no Access header = %d, want 200", path, rec.Code)
		}
	}
}

// askWithBearer issues a header-less GET presenting token as the backup job
// does.
func askWithBearer(t *testing.T, h http.Handler, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// /backup.db has two callers: the in-cluster CronJob, which has no Access
// session and presents the backup token, and the settings page's download
// link, which has a session and is held to the allowlist.
func TestBackupServesTheCronJobAndAllowedAddressesOnly(t *testing.T) {
	h := testRoutes(t, allowed)

	if rec := askWithBearer(t, h, "/backup.db", backupToken); rec.Code != http.StatusOK {
		t.Errorf("the CronJob's backup with its token = %d, want 200", rec.Code)
	}
	if rec := ask(t, h, "/backup.db", allowed); rec.Code != http.StatusOK {
		t.Errorf("an allowed address downloading a backup = %d, want 200", rec.Code)
	}
	if rec := ask(t, h, "/backup.db", removed); rec.Code != http.StatusForbidden {
		t.Errorf("a removed address downloading a backup = %d, want 403", rec.Code)
	}
	if got := ask(t, h, "/backup.db", allowed).Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control on a backup = %q, want no-store", got)
	}
}

// The case the token exists for. With Access removed or bypassed, a request
// arrives through cloudflared with no header at all, and that must not be
// read as the backup job: it would hand every weigh-in to the internet.
func TestBackupRefusesAHeaderlessRequestWithoutTheToken(t *testing.T) {
	h := testRoutes(t, allowed)

	if rec := ask(t, h, "/backup.db", ""); rec.Code != http.StatusForbidden {
		t.Errorf("a header-less backup with no token = %d, want 403", rec.Code)
	}
	if rec := askWithBearer(t, h, "/backup.db", "wrong"); rec.Code != http.StatusForbidden {
		t.Errorf("a header-less backup with the wrong token = %d, want 403", rec.Code)
	}
	// A removed address cannot get in by also presenting the token: the
	// identity, when there is one, is what is judged.
	r := httptest.NewRequest(http.MethodGet, "/backup.db", nil)
	r.Header.Set(access.EmailHeader, removed)
	r.Header.Set("Authorization", "Bearer "+backupToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Errorf("a removed address presenting the token = %d, want 403", rec.Code)
	}
}

// Unconfigured, the token path admits nobody — not even a request whose
// Authorization header is an empty bearer.
func TestBackupWithNoTokenConfiguredAdmitsNoHeaderlessCaller(t *testing.T) {
	h, _ := routes(newTestServer(t).Server, access.NewAllowlist(allowed, ""), "")

	for _, token := range []string{"", backupToken} {
		if rec := askWithBearer(t, h, "/backup.db", token); rec.Code != http.StatusForbidden {
			t.Errorf("a header-less backup presenting %q with no token configured = %d, want 403", token, rec.Code)
		}
	}
}

func TestReadBackupToken(t *testing.T) {
	if got, err := readBackupToken(""); got != "" || err != nil {
		t.Errorf(`readBackupToken("") = %q, %v; want "", nil`, got, err)
	}

	dir := t.TempDir()
	good := filepath.Join(dir, "token")
	if err := os.WriteFile(good, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := readBackupToken(good); got != "s3cret" || err != nil {
		t.Errorf("readBackupToken(token file) = %q, %v; want the trimmed token", got, err)
	}

	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readBackupToken(empty); err == nil {
		t.Error("readBackupToken(empty file) succeeded, want an error")
	}
	if _, err := readBackupToken(filepath.Join(dir, "missing")); err == nil {
		t.Error("readBackupToken(missing file) succeeded, want an error")
	}
}

// Unconfigured is the state this app runs in until its ConfigMap exists, and
// it must behave exactly as it did before the flag: anyone Access admits.
func TestEmptyAllowlistAdmitsAnyAccessCaller(t *testing.T) {
	h := testRoutes(t, "")

	if rec := ask(t, h, "/", removed); rec.Code == http.StatusForbidden {
		t.Error("an empty allowlist refused a caller Access admitted")
	}
	// Still not an open door: the header has to be there.
	if rec := ask(t, h, "/", ""); rec.Code != http.StatusForbidden {
		t.Error("an empty allowlist admitted a request with no Access header")
	}
}

// Every authenticated response, refusals included, must stay out of
// Cloudflare's edge cache.
func TestAuthenticatedResponsesAreNotCached(t *testing.T) {
	h := testRoutes(t, allowed)

	for _, email := range []string{allowed, removed} {
		if got := ask(t, h, "/", email).Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("Cache-Control for %q = %q, want no-store", email, got)
		}
	}
}
