package proposal_test

import (
	"testing"
	"time"

	"github.com/mauv0809/crispy-broccoli/internal/proposal"
	"github.com/mauv0809/crispy-broccoli/internal/strategy"
)

func TestAddCadence_Table(t *testing.T) {
	base := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		c    strategy.Cadence
		want time.Time
	}{
		{strategy.CadenceMonthly, time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)},
		{strategy.CadenceQuarterly, time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)},
		{strategy.CadenceSemiAnnual, time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)},
		{strategy.CadenceAnnual, time.Date(2027, 5, 8, 12, 0, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		got, err := proposal.AddCadence(base, tc.c)
		if err != nil {
			t.Errorf("%s: err = %v", tc.c, err)
			continue
		}
		if !got.Equal(tc.want) {
			t.Errorf("%s: got %s, want %s", tc.c, got, tc.want)
		}
	}
}

func TestAddCadence_UnknownReturnsError(t *testing.T) {
	_, err := proposal.AddCadence(time.Now(), strategy.Cadence("bogus"))
	if err == nil {
		t.Error("want error for bogus cadence")
	}
}

func TestAddCadence_PreservesTimezone(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("America/New_York timezone not available: %v", err)
	}
	base := time.Date(2026, 5, 8, 12, 0, 0, 0, loc)
	got, err := proposal.AddCadence(base, strategy.CadenceMonthly)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got.Location() != loc {
		t.Errorf("location lost: got %s, want %s", got.Location(), loc)
	}
}
