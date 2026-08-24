package handlers

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"weight-tracker/internal/db"
)

// HandleBackup serves a consistent snapshot of the whole database as a
// downloadable file. Unlike /export.csv — which covers entries only, and
// only the fields the CSV format carries — this is the actual SQLite file,
// so it restores goals, markers, period overrides, and ids too: copy it
// into place and the app picks up exactly where the snapshot was taken.
//
// The snapshot is built via VACUUM INTO rather than by copying the database
// file off disk, because a live database has in-flight WAL content that a
// plain file copy would miss or tear.
func (s *Server) HandleBackup(w http.ResponseWriter, r *http.Request) {
	// VACUUM INTO writes a whole second copy, so it needs somewhere with
	// room for one. The database is small (a decade of twice-daily weigh-ins
	// is a few MB), which is why the system temp dir is fine even where it
	// is a RAM-backed tmpfs, as on a default Raspberry Pi OS install.
	dir, err := os.MkdirTemp("", "weight-tracker-backup")
	if err != nil {
		http.Error(w, "could not create a staging directory: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Removes the snapshot whether the download succeeds, fails, or the
	// client disconnects halfway through.
	defer os.RemoveAll(dir)

	// VACUUM INTO refuses to write to a path that already exists, and
	// MkdirTemp has just given us an empty directory of our own.
	snapshot := filepath.Join(dir, "snapshot.db")
	if err := db.BackupTo(r.Context(), s.db, snapshot); err != nil {
		http.Error(w, "could not create the snapshot: "+err.Error(), http.StatusInternalServerError)
		return
	}

	f, err := os.Open(snapshot)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	filename := fmt.Sprintf("weight-tracker-backup-%s.db", s.now().Format("2006-01-02"))
	w.Header().Set("Content-Type", "application/vnd.sqlite3")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	// Set explicitly so the browser can show real download progress rather
	// than falling back to chunked transfer for a file whose size we know.
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))

	// A copy failure here is almost always the client going away, and the
	// headers are already sent, so there is no status code left to change —
	// just stop.
	io.Copy(w, f)
}
