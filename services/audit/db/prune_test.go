package db_test

import (
	"testing"
	"time"

	"github.com/plat5dev/plat5/audit/db"
)

func TestParsePruneCutoff(t *testing.T) {
	now := time.Date(2026, 11, 27, 14, 0, 0, 0, time.UTC)

	got, err := db.ParsePruneCutoff("2026-01", "", now)
	if err != nil || !got.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("before 2026-01: %v %v", got, err)
	}
	// Current month is allowed as exclusive cutoff (keeps 2026-11, drops <= 2026-10).
	got, err = db.ParsePruneCutoff("2026-11", "", now)
	if err != nil || !got.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("before current month: %v %v", got, err)
	}
	// 12m older than 2026-11 -> cutoff 2025-11 (drops <= 2025-10, keeps 12 full months + current).
	got, err = db.ParsePruneCutoff("", "12m", now)
	if err != nil || !got.Equal(time.Date(2025, 11, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("older-than 12m: %v %v", got, err)
	}
	// Month-end date (March 31) does not skip February.
	mar31 := time.Date(2027, 3, 31, 12, 0, 0, 0, time.UTC)
	got, err = db.ParsePruneCutoff("", "1m", mar31)
	if err != nil || !got.Equal(time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("older-than 1m on March 31: %v %v", got, err)
	}

	for name, tc := range map[string]struct{ before, older string }{
		"neither":      {"", ""},
		"both":         {"2026-01", "12m"},
		"future month": {"2026-12", ""},
		"bad month":    {"2026-13", ""},
		"short month":  {"2026-1", ""},
		"zero months":  {"", "0m"},
		"bad unit":     {"", "30d"},
		"negative":     {"", "-1m"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := db.ParsePruneCutoff(tc.before, tc.older, now); err == nil {
				t.Fatalf("expected error for %+v", tc)
			}
		})
	}
}
