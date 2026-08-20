package main

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"sort"
	"time"

	"weight-tracker/internal/db"
	"weight-tracker/internal/weight"
)

func (s *server) handleExport(w http.ResponseWriter, _ *http.Request) {
	entries, err := db.ListEntries(s.db)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// ListEntries returns newest-first; a chronological export reads more
	// naturally in a spreadsheet and matches how the importer expects rows.
	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].RecordedAt.Before(entries[j].RecordedAt)
	})

	filename := fmt.Sprintf("weight-tracker-export-%s.csv", time.Now().Format("2006-01-02"))
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))

	// The header stays weight_kg rather than switching to grams: kilograms
	// are what the app shows and what a spreadsheet reader expects, the
	// importer already recognizes this column, and three decimals of a
	// kilogram represent whole grams exactly, so the round trip back through
	// import is lossless. Values are now clean (81.647, not the
	// 81.64662660000001 that REAL storage used to print).
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"recorded_at", "weight_kg"})
	for _, e := range entries {
		_ = cw.Write([]string{
			e.RecordedAt.Format(time.RFC3339),
			weight.FormatKgInput(e.WeightG),
		})
	}
	cw.Flush()
}
