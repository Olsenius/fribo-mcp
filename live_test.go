//go:build live

package main

// Read-only checks against the real Fribo API. Nothing is registered,
// changed or deleted, and only counts and statuses are logged.
//
//	FRIBO_EMAIL=... FRIBO_PASSWORD=... go test -tags live -run Live -count=1 -v .
import (
	"context"
	"os"
	"testing"
	"time"
)

func TestLiveReadOnly(t *testing.T) {
	email, password := os.Getenv("FRIBO_EMAIL"), os.Getenv("FRIBO_PASSWORD")
	if email == "" || password == "" {
		t.Skip("FRIBO_EMAIL and FRIBO_PASSWORD must be set")
	}
	ctx := context.Background()
	c := newClient(env("FRIBO_API_URL", defaultAPIURL), email, password)

	cabins, err := c.Cabins(ctx)
	if err != nil || len(cabins) == 0 {
		t.Fatalf("cabins: %d %v", len(cabins), err)
	}
	t.Logf("cabins: %d", len(cabins))
	id, err := resolveCabin(ctx, c, Settings{CabinID: os.Getenv("FRIBO_CABIN_ID")}, "")
	if err != nil {
		t.Fatal(err)
	}

	contracts, err := c.Contracts(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	ct, err := plowingContract(contracts)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("contract: %s %s, %s to %s", ct.Type, ct.Status, ct.StartDate.Format(dateLayout), ct.EndDate.Format(dateLayout))

	cfg, err := c.PlowingConfig(ctx, ct.OrganizationID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	t.Logf("season %s to %s (active today: %v), arrival deadline %s, earliest guaranteed arrival %s",
		day(cfg.StartSeasonDate).Format(dateLayout), day(cfg.EndSeasonDate).Format(dateLayout), inSeason(now, cfg),
		describeDeadline(cfg.PlowingSchedule.WorkDaysDeadline, "arrival"), earliestGuaranteedArrival(cfg, now))

	stays, err := c.Stays(ctx, id, 500)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("stays: %d, upcoming: %d", len(stays), len(upcoming(stays, now)))
	// Paging: a small page size must still return everything once.
	seen := map[string]bool{}
	for _, s := range stays {
		if seen[s.ID] {
			t.Fatalf("stay listed twice: paging is off")
		}
		seen[s.ID] = true
	}
	if len(stays) > 0 {
		if _, err := c.Stay(ctx, id, stays[0].ID); err != nil {
			t.Fatalf("stay details: %v", err)
		}
	}

	ev, err := c.PlowingHistory(ctx, id, 100)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plowing events: %d", len(ev))
}
