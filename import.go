package main

import (
	"net/http"
	"time"

	"weight-tracker/internal/db"
	"weight-tracker/internal/importer"
)

func (s *server) handleImportForm(w http.ResponseWriter, _ *http.Request) {
	if err := tmpl.ExecuteTemplate(w, "import", nil); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *server) handleImport(w http.ResponseWriter, r *http.Request) {
	file, _, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "missing file", http.StatusBadRequest)
		return
	}
	defer file.Close()

	unit := r.FormValue("unit")
	if unit != "kg" && unit != "lb" {
		http.Error(w, "invalid unit", http.StatusBadRequest)
		return
	}

	result, err := importer.Parse(file, unit)
	if err != nil {
		http.Error(w, "could not parse CSV: "+err.Error(), http.StatusBadRequest)
		return
	}

	existing, err := db.ExistingRecordedAtSet(s.db)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	newEntries := make([]db.NewEntry, len(result.Rows))
	for i, row := range result.Rows {
		newEntries[i] = db.NewEntry{RecordedAt: row.RecordedAt, WeightKg: row.WeightKg}
	}

	inserted, err := db.BulkCreateEntries(s.db, newEntries, existing, time.Now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	skippedErrors := result.Skipped
	const maxShown = 20
	truncated := 0
	if len(skippedErrors) > maxShown {
		truncated = len(skippedErrors) - maxShown
		skippedErrors = skippedErrors[:maxShown]
	}

	data := struct {
		Inserted   int
		Duplicates int
		Invalid    int
		Errors     []importer.SkippedRow
		Truncated  int
	}{
		Inserted:   inserted,
		Duplicates: len(result.Rows) - inserted,
		Invalid:    len(result.Skipped),
		Errors:     skippedErrors,
		Truncated:  truncated,
	}
	if err := tmpl.ExecuteTemplate(w, "import-result", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
