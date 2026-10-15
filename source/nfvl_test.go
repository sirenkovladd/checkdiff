package source

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

const nfvlFixtureNewLayout = `
<html><body>
<h5 class="h5heading"><strong>Wed Oct 14, 2026</strong></h5>
<table><tr>
<td><a href="boxscores_volleyball.cfm?clientID=2768&amp;leagueID=6972&amp;gameID=2571372">Final</a></td>
<td><span class="d-sm-inline d-none"><a class="text-dark" href="stats_1team.cfm?clientID=2768&amp;teamID=677054&amp;leagueID=6972">El Fuego (NE)</a></span></td>
<td>3</td>
<td><span class="d-sm-inline d-none"><a class="text-dark" href="stats_1team.cfm?clientID=2768&amp;teamID=677115&amp;leagueID=6972">Gold Diggers (SE)</a></span></td>
<td>0</td>
<td>Moberly Elementary</td>
<td>map</td>
</tr></table>
<h5 class="h5heading"><strong>Wed Oct 21, 2026</strong></h5>
<table><tr>
<td>8:15 PM</td>
<td><span class="d-sm-inline d-none"><a class="text-dark" href="stats_1team.cfm?clientID=2768&amp;teamID=677054&amp;leagueID=6972">El Fuego (NE)</a></span></td>
<td><i class="fa fa-tshirt"></i>&nbsp;</td>
<td><span class="d-sm-inline d-none"><a class="text-dark" href="stats_1team.cfm?clientID=2768&amp;teamID=677046&amp;leagueID=6972">No Pecs No Sets (NE)</a></span></td>
<td><i class="fa fa-tshirt"></i>&nbsp;</td>
<td>Renfrew Elementary</td>
<td>map</td>
</tr></table>
</body></html>
`

const nfvlFixtureOldLayout = `
<html><body>
<h5 class="h5heading"><strong>Wed Sep 16, 2026</strong></h5>
<table><tr>
<td><a href="boxscores_volleyball.cfm?clientID=2768&amp;leagueID=6972&amp;gameID=2571171">Final</a></td>
<td>12</td>
<td><span class="d-sm-inline d-none"><a class="text-dark" href="stats_1team.cfm?teamID=677046">No Pecs No Sets (NE)</a></span></td>
<td>3</td>
<td><span class="d-sm-inline d-none"><a class="text-dark" href="stats_1team.cfm?teamID=677054">El Fuego (NE)</a></span></td>
<td>1</td>
<td>Renfrew Elementary</td>
</tr></table>
</body></html>
`

func nfvlTestServer(t *testing.T, body string, checkForm func(url.Values)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm: %v", err)
		}
		if checkForm != nil {
			checkForm(r.PostForm)
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, body)
	}))
}

func TestNFVLFetchPostsFormAndParsesGames(t *testing.T) {
	srv := nfvlTestServer(t, nfvlFixtureNewLayout, func(f url.Values) {
		if got := f.Get("monthID"); got != "0" {
			t.Errorf("monthID = %q, want 0", got)
		}
		if got := f.Get("selectedTeamID"); got != "677054" {
			t.Errorf("selectedTeamID = %q, want 677054", got)
		}
	})
	defer srv.Close()

	s := &Source{ID: "elf", Name: "El Fuego", Type: "nfvl", URL: srv.URL, LeagueID: 6972, TeamID: 677054}
	items, err := nfvlFetcher{}.Fetch(context.Background(), s, time.Now())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2", len(items))
	}
	// Final game keeps the gameID-based stable ID.
	if items[0].ID != "nfvl:2571372" {
		t.Errorf("final ID = %q, want nfvl:2571372", items[0].ID)
	}
	// Scheduled game surfaces time + opponent + venue.
	sched := items[1]
	for _, want := range []string{"8:15 PM", "No Pecs No Sets (NE)", "Renfrew Elementary", "Oct 21"} {
		if !strings.Contains(sched.Title, want) {
			t.Errorf("scheduled Title %q missing %q", sched.Title, want)
		}
	}
	if sched.Link == "" || strings.Contains(sched.Link, "gameID") {
		t.Errorf("scheduled Link should fall back to the schedule URL, got %q", sched.Link)
	}
}

func TestNFVLFetchOldLayout(t *testing.T) {
	srv := nfvlTestServer(t, nfvlFixtureOldLayout, nil)
	defer srv.Close()

	s := &Source{ID: "elf", Name: "El Fuego", Type: "nfvl", URL: srv.URL, LeagueID: 6972, TeamID: 677054}
	items, err := nfvlFetcher{}.Fetch(context.Background(), s, time.Now())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
	}
	// Old layout: away=No Pecs, home=El Fuego — must not be swapped.
	if !strings.Contains(items[0].Title, "No Pecs No Sets (NE) vs El Fuego (NE)") {
		t.Errorf("teams swapped or misparsed, Title = %q", items[0].Title)
	}
}

func TestNFVLScheduledGameFallbackID(t *testing.T) {
	fetch := func() []Item {
		srv := nfvlTestServer(t, nfvlFixtureNewLayout, nil)
		defer srv.Close()
		s := &Source{ID: "elf", Name: "El Fuego", Type: "nfvl", URL: srv.URL, LeagueID: 6972, TeamID: 677054}
		items, err := nfvlFetcher{}.Fetch(context.Background(), s, time.Now())
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		return items
	}
	first, second := fetch(), fetch()
	if len(first) != len(second) {
		t.Fatalf("run lengths differ: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i].ID != second[i].ID {
			t.Errorf("item %d ID unstable: %q vs %q", i, first[i].ID, second[i].ID)
		}
	}
}

func TestNFVLValidate(t *testing.T) {
	v := nfvlFetcher{}
	if err := v.Validate(&Source{Type: "nfvl", TeamID: 677054}); err == nil {
		t.Errorf("missing league: got nil error, want error")
	}
	if err := v.Validate(&Source{Type: "nfvl", LeagueID: 6972}); err == nil {
		t.Errorf("missing team: got nil error, want error")
	}
	s := &Source{Type: "nfvl", LeagueID: 6972, TeamID: 677054}
	if err := v.Validate(s); err != nil {
		t.Fatalf("validate ok case: %v", err)
	}
	if s.ClientID != 2768 {
		t.Errorf("ClientID = %d, want default 2768", s.ClientID)
	}
	custom := &Source{Type: "nfvl", LeagueID: 6972, TeamID: 677054, URL: "http://example.com/sched"}
	if err := v.Validate(custom); err != nil {
		t.Fatalf("custom URL case: %v", err)
	}
	if custom.URL != "http://example.com/sched" {
		t.Errorf("custom URL not preserved, got %q", custom.URL)
	}
}

func TestNFVLFormatAdded(t *testing.T) {
	s := &Source{ID: "elf", Name: "El Fuego Oct 21 game", Type: "nfvl", URL: "https://example.com/sched"}
	added := []Item{{ID: "nfvl:Wed Oct 21, 2026|El Fuego (NE)|No Pecs No Sets (NE)",
		Title: "Wed Oct 21, 2026 8:15 PM: El Fuego (NE) vs No Pecs No Sets (NE) @ Renfrew Elementary"}}
	n := nfvlFetcher{}.Format(context.Background(), s, added, nil)
	if !strings.Contains(n.Body, "No Pecs No Sets (NE)") || !strings.Contains(n.Body, "Oct 21") {
		t.Errorf("body should name opponent and date, got:\n%s", n.Body)
	}
}

func TestNFVLErrorOnNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	s := &Source{ID: "elf", Name: "El Fuego", Type: "nfvl", URL: srv.URL, LeagueID: 6972, TeamID: 677054}
	if _, err := (nfvlFetcher{}.Fetch(context.Background(), s, time.Now())); err == nil {
		t.Errorf("202 response: got nil error, want error")
	}
}

func TestNFVLErrorOnEmptySchedule(t *testing.T) {
	srv := nfvlTestServer(t, `<html><body><p>challenge</p></body></html>`, nil)
	defer srv.Close()
	s := &Source{ID: "elf", Name: "El Fuego", Type: "nfvl", URL: srv.URL, LeagueID: 6972, TeamID: 677054}
	if _, err := (nfvlFetcher{}.Fetch(context.Background(), s, time.Now())); err == nil {
		t.Errorf("empty schedule: got nil error, want error (never record an empty baseline)")
	}
}
