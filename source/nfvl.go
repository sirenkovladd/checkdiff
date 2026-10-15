package source

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"checkdiff/template"
	"golang.org/x/net/html"
)

// nfvlFetcher implements the Fetcher interface for the "nfvl"
// source type: an NFVL (No Frills Volleyball League,
// esportsdesk.com) team schedule. The schedule page only
// renders fully in response to a form POST (plain GETs return
// a partial page or a bot challenge), so this fetcher POSTs
// the same form a browser submits (monthID=0 for the whole
// schedule, selectedTeamID for the team filter) and parses
// the game table into one Item per game.
//
// Where html is the right choice: a page retrievable with a
// plain GET whose entries match a CSS-ish selector.
//
// Where nfvl is the right choice: an esportsdesk schedule
// page, which requires the POST form above. Pointing an html
// source at the schedule URL only ever sees the partial
// page and records a wrong baseline.
//
// Diff semantics: one Item per game with a stable ID (the
// esportsdesk gameID when the row links to a boxscore,
// otherwise date+teams). A newly posted game (e.g. Oct 21
// appearing on the schedule) is an addition and notifies;
// score updates to an existing game change nothing about
// its ID, so results being entered do not re-notify.
type nfvlFetcher struct{}

// Type returns the registry key for this fetcher.
func (nfvlFetcher) Type() string { return "nfvl" }

// defaultNFVLClientID is the esportsdesk client for nfvl.ca.
const defaultNFVLClientID = 2768

// nfvlScheduleURL builds the canonical schedule URL for a
// client/league pair. The team filter travels in the POST
// body, not the URL.
func nfvlScheduleURL(clientID, leagueID int) string {
	return fmt.Sprintf("https://www.nfvl.ca/leagues/schedules.cfm?clientid=%d&leagueID=%d&schedType=main&printPage=0",
		clientID, leagueID)
}

// nfvlTimeRe matches the scheduled-game time in the
// result/time cell (e.g. "8:15 PM").
var nfvlTimeRe = regexp.MustCompile(`\d{1,2}:\d{2}\s*[AP]M`)

// nfvlGameIDRe matches the boxscore link's game identifier.
var nfvlGameIDRe = regexp.MustCompile(`gameID=(\d+)`)

// Fetch POSTs the schedule form and returns one Item per
// game row. A page with zero parsable games is an error,
// not an empty baseline: recording an empty set for a
// challenge/partial page would turn the real schedule into
// a flood of additions on the next run.
func (nfvlFetcher) Fetch(ctx context.Context, s *Source, now time.Time) ([]Item, error) {
	scheduleURL := template.Render(s.URL, now)
	if strings.TrimSpace(scheduleURL) == "" {
		scheduleURL = nfvlScheduleURL(s.ClientID, s.LeagueID)
	}
	form := url.Values{
		"monthID":        {"0"},
		"selectedTeamID": {strconv.Itoa(s.TeamID)},
		"gameType":       {""},
	}
	if fetchVerbose {
		log.Printf("[%s] fetch: POST %s (league=%d team=%d)", s.ID, scheduleURL, s.LeagueID, s.TeamID)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", scheduleURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/147.0.0.0 Safari/537.36")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", scheduleURL)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("POST %s: %s", scheduleURL, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20)) // 5 MiB cap, same as html
	if err != nil {
		return nil, err
	}

	games, err := parseNFVLSchedule(body)
	if err != nil {
		return nil, err
	}
	if len(games) == 0 {
		return nil, fmt.Errorf("no games parsed (page structure may have changed)")
	}

	items := make([]Item, 0, len(games))
	for _, g := range games {
		id := fmt.Sprintf("nfvl:%s|%s|%s", g.date, g.away, g.home)
		link := scheduleURL
		if g.gameID != 0 {
			id = fmt.Sprintf("nfvl:%d", g.gameID)
			link = fmt.Sprintf("https://www.nfvl.ca/leagues/boxscores_volleyball.cfm?clientID=%d&leagueID=%d&gameID=%d",
				s.ClientID, s.LeagueID, g.gameID)
		}
		title := fmt.Sprintf("%s %s: %s vs %s @ %s", g.date, g.status, g.away, g.home, g.location)
		items = append(items, Item{
			ID:    id,
			Title: title,
			Body:  fmt.Sprintf("%s %s-%s %s\n%s @ %s", g.away, g.awayScore, g.homeScore, g.home, g.status, g.location),
			Link:  link,
		})
	}
	if fetchVerbose {
		log.Printf("[%s] fetch ok: %d games", s.ID, len(items))
	}
	return items, nil
}

// Validate requires league and team IDs; the esportsdesk
// client defaults to nfvl.ca's. A custom URL is allowed so
// tests can point the fetcher at a local server; when empty
// the canonical schedule URL is built at fetch time.
func (nfvlFetcher) Validate(s *Source) error {
	if s.LeagueID == 0 {
		return fmt.Errorf("nfvl requires league_id")
	}
	if s.TeamID == 0 {
		return fmt.Errorf("nfvl requires team_id")
	}
	if s.ClientID == 0 {
		s.ClientID = defaultNFVLClientID
	}
	return nil
}

// Format renders added/removed games with the shared
// list layout, so a newly posted game notifies with its
// date, matchup, and venue. Click opens the boxscore for
// the first added game, falling back to the source link
// and schedule URL.
func (nfvlFetcher) Format(ctx context.Context, s *Source, added, removed []Item) Notification {
	click := s.URL
	if strings.TrimSpace(click) == "" {
		click = nfvlScheduleURL(s.ClientID, s.LeagueID)
	}
	if len(added) > 0 && added[0].Link != "" {
		click = added[0].Link
	} else if s.Link != "" {
		click = s.Link
	}
	return formatListDiff(s, added, removed, click)
}

// nfvlGame is one parsed schedule row.
type nfvlGame struct {
	date      string
	status    string // "Final", "Final (no scores)", a time like "8:15 PM", or "Scheduled"
	away      string
	home      string
	awayScore string // numeric or "-" when not played
	homeScore string
	location  string
	gameID    int
}

// parseNFVLSchedule walks the schedule HTML in document
// order, tracking the current date header and parsing each
// game row beneath it. Two row layouts exist: the older
// one with a numeric game-number column, and the Fall 2026
// one without it (detected the same way the reference
// script does — a numeric second cell means the old
// layout).
func parseNFVLSchedule(body []byte) ([]nfvlGame, error) {
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("parse HTML: %w", err)
	}
	var games []nfvlGame
	currentDate := "Unknown"
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "h5" && nfvlHasToken(n, "class", "h5heading") {
			if d := nfvlHeaderDate(n); d != "" {
				currentDate = d
			}
		}
		if n.Type == html.ElementNode && n.Data == "tr" {
			if g, ok := nfvlParseRow(n, currentDate); ok {
				games = append(games, g)
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return games, nil
}

// nfvlHeaderDate returns the date text of an h5 heading
// (the strong child's text), or "" when absent.
func nfvlHeaderDate(h5 *html.Node) string {
	var strong *html.Node
	var find func(*html.Node)
	find = func(n *html.Node) {
		if strong != nil {
			return
		}
		if n.Type == html.ElementNode && n.Data == "strong" {
			strong = n
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			find(c)
		}
	}
	find(h5)
	if strong == nil {
		return ""
	}
	return strings.Join(strings.Fields(extractText(strong)), " ")
}

// nfvlParseRow parses one tr into a game. It returns false
// for non-game rows (header rows, rows without a team
// link, short rows).
func nfvlParseRow(tr *html.Node, date string) (nfvlGame, bool) {
	var tds []*html.Node
	for c := tr.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && c.Data == "td" {
			tds = append(tds, c)
		}
	}
	if len(tds) < 6 {
		return nfvlGame{}, false
	}
	var awayTD, awayScoreTD, homeTD, homeScoreTD, locTD *html.Node
	if len(tds) >= 7 {
		if _, err := strconv.Atoi(nfvlCellText(tds[1])); err == nil {
			awayTD, awayScoreTD = tds[2], tds[3]
			homeTD, homeScoreTD = tds[4], tds[5]
			locTD = tds[6]
		}
	}
	if awayTD == nil {
		// Fall 2026 layout — the away cell must carry a team link.
		if !nfvlHasTeamLink(tds[1]) {
			return nfvlGame{}, false
		}
		awayTD, awayScoreTD = tds[1], tds[2]
		homeTD, homeScoreTD = tds[3], tds[4]
		locTD = tds[5]
	}
	if !nfvlHasTeamLink(awayTD) {
		return nfvlGame{}, false
	}

	away := nfvlTeamName(awayTD)
	home := nfvlTeamName(homeTD)
	if away == "" || home == "" {
		return nfvlGame{}, false
	}

	awayScore := nfvlCellScore(awayScoreTD)
	homeScore := nfvlCellScore(homeScoreTD)

	resultCell := nfvlCellText(tds[0])
	var status string
	switch {
	case awayScore != "-" || homeScore != "-":
		status = "Final"
	case strings.Contains(resultCell, "Final"):
		status = "Final (no scores)"
	default:
		if m := nfvlTimeRe.FindString(resultCell); m != "" {
			status = m
		} else {
			status = "Scheduled"
		}
	}

	return nfvlGame{
		date:      date,
		status:    status,
		away:      away,
		home:      home,
		awayScore: awayScore,
		homeScore: homeScore,
		location:  nfvlCellText(locTD),
		gameID:    nfvlRowGameID(tr),
	}, true
}

// nfvlCellText returns the whitespace-collapsed text of a cell.
func nfvlCellText(td *html.Node) string {
	return strings.Join(strings.Fields(extractText(td)), " ")
}

// nfvlCellScore returns the numeric score of a score cell,
// or "-" when the game hasn't been played (jersey icon or
// blank cell).
func nfvlCellScore(td *html.Node) string {
	if nfvlHasDescendantToken(td, "fa-tshirt") {
		return "-"
	}
	text := strings.ReplaceAll(nfvlCellText(td), "\u00a0", "")
	text = strings.TrimSpace(text)
	if text == "" {
		return "-"
	}
	if _, err := strconv.Atoi(text); err != nil {
		return "-"
	}
	return text
}

// nfvlTeamName extracts the desktop team name: the first
// link with class "text-dark", falling back to any link
// pointing at a team page.
func nfvlTeamName(td *html.Node) string {
	var fallback string
	var find func(*html.Node) string
	find = func(n *html.Node) string {
		if n.Type == html.ElementNode && n.Data == "a" {
			href := nfvlAttr(n, "href")
			text := strings.Join(strings.Fields(extractText(n)), " ")
			if text == "" {
				return ""
			}
			if hasClass(n, "text-dark") {
				return text
			}
			if fallback == "" && strings.Contains(href, "stats_1team") {
				fallback = text
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if got := find(c); got != "" {
				return got
			}
		}
		return ""
	}
	if got := find(td); got != "" {
		return got
	}
	return fallback
}

// nfvlHasTeamLink reports whether a cell links to a team page.
func nfvlHasTeamLink(td *html.Node) bool {
	found := false
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if found {
			return
		}
		if n.Type == html.ElementNode && n.Data == "a" {
			if hasClass(n, "text-dark") || strings.Contains(nfvlAttr(n, "href"), "stats_1team") {
				found = true
				return
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(td)
	return found
}

// nfvlRowGameID extracts the boxscore gameID from a row's
// links, or 0 for scheduled games that have none yet.
func nfvlRowGameID(tr *html.Node) int {
	id := 0
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if id != 0 {
			return
		}
		if n.Type == html.ElementNode && n.Data == "a" {
			if m := nfvlGameIDRe.FindStringSubmatch(nfvlAttr(n, "href")); m != nil {
				if v, err := strconv.Atoi(m[1]); err == nil {
					id = v
					return
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(tr)
	return id
}

// nfvlHasToken reports whether n carries token in its
// space-separated attribute (used for multi-class
// attributes like `class="h5heading mb-3"`).
func nfvlHasToken(n *html.Node, attr, token string) bool {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, attr) {
			for _, t := range strings.Fields(a.Val) {
				if t == token {
					return true
				}
			}
			return false
		}
	}
	return false
}

// nfvlHasDescendantToken reports whether any descendant
// element carries the given class token (e.g. the
// no-score jersey icon `fa-tshirt`).
func nfvlHasDescendantToken(n *html.Node, token string) bool {
	found := false
	var walk func(*html.Node)
	walk = func(m *html.Node) {
		if found {
			return
		}
		if m.Type == html.ElementNode && hasClass(m, token) {
			found = true
			return
		}
		for c := m.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return found
}

// nfvlAttr returns an attribute value or "".
func nfvlAttr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}
