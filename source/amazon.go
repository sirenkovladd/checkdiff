package source

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"checkdiff/template"
	"golang.org/x/net/html"
)

// amazonFetcher implements the Fetcher interface for the
// "amazon" source type: an Amazon shipment tracking page
// (e.g. https://www.amazon.ca/gp/your-account/ship-track?itemId=...
// &orderId=...&shipmentId=...). The page contains a div with
// role="list" and class "pt-status-milestones" whose
// aria-label and the data-last-reached child describe the
// current delivery state ("Out for delivery; Step 3 of 4
// (51%)"). When the state changes between runs, the user is
// notified.
//
// Where the json_value fetcher is the right choice: a JSON
// field that exposes the current state as a single string.
//
// Where amazon is the right choice: an HTML page where the
// current state is buried inside a specific DOM structure.
// The fetcher hard-codes the selectors it knows about
// (pt-status-milestones, data-last-reached, etc.); if Amazon
// redesigns the page, the fetcher will need an update.
//
// Diff semantics: a single Item whose ID is the formatted
// status string. When the ID changes, the diff machinery in
// check.One produces one added Item and one removed Item,
// and the Format helper renders a "from X to Y"
// notification.
//
// Cookies: Amazon's tracking page requires the user's
// session cookies to be sent, otherwise the response is a
// redirect to sign-in (or a bot-detection challenge). The
// user provides the Cookie header value via the optional
// `cookies` field in the source config. Keep the config
// file's permissions restrictive — the daemon writes it
// 0600 — since session cookies are a long-lived credential.
type amazonFetcher struct{}

// Type returns the registry key for this fetcher.
func (amazonFetcher) Type() string { return "amazon" }

// amazonStatus is the parsed state of an Amazon tracking
// page. The Value() method returns the canonical string
// used as the Item's ID; any change in Value() between
// runs triggers a notification.
type amazonStatus struct {
	// AriaLabel is the full aria-label of the milestones
	// div, e.g. "Delivery status: Out for delivery; Step 3
	// of 4.".
	AriaLabel string
	// CurrentStep is the text of the milestone marked
	// data-last-reached="true" (e.g. "Out for delivery").
	CurrentStep string
	// StepCount is the "Step X of Y" portion of the
	// aria-label (e.g. "Step 3 of 4").
	StepCount string
	// Percent is the in-progress percent (e.g. "51"). This
	// is the data-percent-complete of the NEXT milestone
	// (the one not yet reached) when one exists, or the
	// current milestone's own percent (always 100) when the
	// package is fully delivered. Stored as a string so the
	// caller's percent value (e.g. "51", "100", or "0") can
	// be formatted without lossy float conversion.
	Percent string
}

// Value returns the canonical status string used as the
// Item's ID. Format: "{CurrentStep}; {StepCount} ({Percent}%)"
// — matching the user's example "Out for delivery; Step 3
// of 4 (51%)". When StepCount is empty (the aria-label has
// no "Step X of Y" segment), the step part is dropped:
// "Out for delivery (51%)". When Percent is also empty
// (e.g. the percent couldn't be determined), the trailing
// parens are dropped: "Out for delivery".
func (s amazonStatus) Value() string {
	var b strings.Builder
	b.WriteString(s.CurrentStep)
	if s.StepCount != "" {
		b.WriteString("; ")
		b.WriteString(s.StepCount)
	}
	if s.Percent != "" {
		fmt.Fprintf(&b, " (%s%%)", s.Percent)
	}
	return b.String()
}

// Fetch retrieves the current delivery status from an
// Amazon shipment tracking page. The page is fetched with
// realistic browser headers (User-Agent, Accept, etc.) and
// the user's session cookies (if provided). The status is
// extracted from the "pt-status-milestones" div's
// aria-label and the data-last-reached milestone's label
// and the next milestone's percent. Returns a single Item
// whose ID is the formatted status string; a change in the
// ID between runs triggers a notification.
func (amazonFetcher) Fetch(ctx context.Context, s *Source, now time.Time) ([]Item, error) {
	url := template.Render(s.URL, now)
	if fetchVerbose {
		log.Printf("[%s] fetch: GET %s", s.ID, url)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	// Amazon's tracking page is browser-rendered. The
	// server returns a sign-in redirect (or a bot-detection
	// challenge) if the request doesn't look like a real
	// browser. The default headers below are the same ones
	// a Chrome 150 visit would send; the user provides
	// session cookies via the optional `cookies` field.
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")
	req.Header.Set("DNT", "1")
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Sec-Fetch-User", "?1")
	req.Header.Set("Sec-GPC", "1")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	if s.Cookies != "" {
		req.Header.Set("Cookie", s.Cookies)
	}
	// The Referer is the page the user came from. The
	// default is the order history page (the natural
	// navigation path a real user takes to reach a tracking
	// page); the user can override via the optional
	// `referer` field for regions that use a different
	// landing page.
	referer := s.Referer
	if referer == "" {
		referer = defaultAmazonReferer(s.URL)
	}
	if referer != "" {
		req.Header.Set("Referer", referer)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	// 5 MiB cap, same as the html fetcher. Amazon's
	// tracking page is small (a few hundred KiB) but the
	// shared cap means a misconfigured source can't fill
	// RAM.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	if err != nil {
		return nil, err
	}

	status, err := parseAmazonStatus(body)
	if err != nil {
		return nil, err
	}

	if fetchVerbose {
		log.Printf("[%s] fetch ok: status=%q (percent=%q)", s.ID, status.Value(), status.Percent)
	}
	return []Item{{
		ID:    status.Value(),
		Title: status.Value(),
		Body:  status.AriaLabel,
	}}, nil
}

// Validate applies the amazon-specific defaults: URL is
// required. Cookies is optional but the tracking page
// requires session cookies for most users; without them
// the response is usually a sign-in redirect and the
// fetcher returns an error from parseAmazonStatus (no
// status div found).
func (amazonFetcher) Validate(s *Source) error {
	if s.URL == "" {
		return fmt.Errorf("amazon requires url")
	}
	return nil
}

// Format builds a "status changed" notification. The diff
// for an amazon source is one removed (old status) + one
// added (new status), and the format helper surfaces both
// so the user can see exactly what changed. The Click
// header points at the source's URL so tapping the
// notification opens the tracking page.
func (amazonFetcher) Format(ctx context.Context, s *Source, added, removed []Item) Notification {
	var oldVal, newVal string
	if len(removed) > 0 {
		oldVal = removed[0].Title
	}
	if len(added) > 0 {
		newVal = added[0].Title
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Status changed for %s\n\n", s.Name)
	if oldVal != "" {
		fmt.Fprintf(&b, "From: %s\n", oldVal)
	}
	if newVal != "" {
		fmt.Fprintf(&b, "To:   %s\n", newVal)
	}
	fmt.Fprintf(&b, "\nSource: %s\n", s.URL)

	return Notification{
		Title:    fmt.Sprintf("📦 %s: %s", s.Name, newVal),
		Body:     b.String(),
		Priority: "default",
		Tags:     "package",
		Click:    s.URL,
	}
}

// defaultAmazonReferer returns a sensible default Referer
// for the supplied Amazon tracking URL: the order history
// page on the same host, which is the natural navigation
// path a real user takes to reach a tracking page. The URL
// is one of amazon.com / amazon.ca / amazon.de / etc.; the
// order history path is the same on all of them
// (/gp/css/order-history/). Returns "" if the URL can't be
// parsed (e.g. it doesn't have a "://" or a host).
func defaultAmazonReferer(rawURL string) string {
	i := strings.Index(rawURL, "://")
	if i < 0 {
		return ""
	}
	rest := rawURL[i+3:]
	j := strings.IndexAny(rest, "/?#")
	if j < 0 {
		return rawURL + "/gp/css/order-history/"
	}
	// rawURL[:i+3] is "scheme://"; rest[:j] is the host.
	// Concatenated: "scheme://host". Append the standard
	// order history path.
	return rawURL[:i+3+j] + "/gp/css/order-history/"
}

// parseAmazonStatus parses the Amazon tracking page HTML
// and returns the current delivery status. Returns an
// error if the status div isn't present (e.g. the user
// was redirected to a sign-in page, or Amazon changed the
// HTML). The error message is intentionally
// user-actionable: it tells the user the page may have
// changed, so they can update the fetcher if Amazon
// redesigned the tracking page.
func parseAmazonStatus(body []byte) (amazonStatus, error) {
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return amazonStatus{}, fmt.Errorf("parse HTML: %w", err)
	}

	root := findMilestonesDiv(doc)
	if root == nil {
		return amazonStatus{}, errors.New("delivery status div not found (page may require sign-in, or the HTML has changed)")
	}

	ariaLabel := getAttr(root, "aria-label")
	if ariaLabel == "" {
		return amazonStatus{}, errors.New("delivery status div has no aria-label")
	}

	// Walk the milestone children in order. The current
	// step is the one with data-last-reached="true". The
	// percent is the data-percent-complete of the NEXT
	// milestone (the one currently in progress, e.g.
	// "Delivered" while "Out for delivery" is the current
	// step). If no next milestone exists, the package is
	// fully delivered and we use the current step's own
	// percent (which is 100).
	var currentStep string
	var currentPercent string
	var nextPercent string
	var sawLastReached bool
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && hasClass(n, "pt-status-milestone") {
			lastReached := getAttr(n, "data-last-reached") == "true"
			percent := getAttr(n, "data-percent-complete")
			label := findMilestoneLabel(n)
			if label != "" {
				if lastReached {
					currentStep = label
					currentPercent = percent
					sawLastReached = true
				} else if sawLastReached && nextPercent == "" {
					// First milestone AFTER the
					// last-reached one: its percent is
					// the in-progress value the user
					// sees (e.g. "51" while "Out for
					// delivery" is the current step).
					nextPercent = percent
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)

	if currentStep == "" {
		return amazonStatus{}, errors.New("could not identify current step (no data-last-reached=\"true\" milestone)")
	}

	// If there's no next milestone, the package is fully
	// delivered. The current step's own percent is 100,
	// which is the correct "in-progress" value at that
	// point.
	percent := nextPercent
	if percent == "" {
		percent = currentPercent
	}

	return amazonStatus{
		AriaLabel:   ariaLabel,
		CurrentStep: currentStep,
		StepCount:   extractStepCount(ariaLabel),
		Percent:     percent,
	}, nil
}

// findMilestonesDiv walks the parsed document depth-first
// and returns the first div with role="list" and class
// containing "pt-status-milestones", or nil if not found.
// The existing htmlSelector type only supports tag + class
// (no attribute matching), so this fetcher walks the DOM
// directly.
func findMilestonesDiv(n *html.Node) *html.Node {
	if n.Type == html.ElementNode && n.Data == "div" {
		if getAttr(n, "role") == "list" && hasClass(n, "pt-status-milestones") {
			return n
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if found := findMilestonesDiv(c); found != nil {
			return found
		}
	}
	return nil
}

// hasClass reports whether n has the given class token in
// its class attribute. Class matching is token-based and
// case-sensitive (matching the htmlSelector helper for the
// html source type).
func hasClass(n *html.Node, class string) bool {
	for _, a := range n.Attr {
		if a.Key == "class" {
			for _, c := range strings.Fields(a.Val) {
				if c == class {
					return true
				}
			}
			return false
		}
	}
	return false
}

// getAttr returns the value of the named attribute on n,
// or "" if the attribute is not present. Case-sensitive
// (HTML attribute names are case-insensitive in practice,
// but the golang.org/x/net/html parser lowercases them
// before calling Data, and Amazon's HTML is well-formed
// enough that callers pass canonical names).
func getAttr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// findMilestoneLabel returns the trimmed text content of
// the element with class "pt-status-milestone-label"
// under n, or "" if not found. Only the first such element
// is considered; the page has one label per milestone.
func findMilestoneLabel(n *html.Node) string {
	var found *html.Node
	var walk func(*html.Node)
	walk = func(m *html.Node) {
		if found != nil {
			return
		}
		if m.Type == html.ElementNode && hasClass(m, "pt-status-milestone-label") {
			found = m
			return
		}
		for c := m.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	if found == nil {
		return ""
	}
	var b strings.Builder
	var collect func(*html.Node)
	collect = func(m *html.Node) {
		if m.Type == html.TextNode {
			b.WriteString(m.Data)
		}
		for c := m.FirstChild; c != nil; c = c.NextSibling {
			collect(c)
		}
	}
	collect(found)
	return strings.TrimSpace(b.String())
}

// extractStepCount parses the "Step X of Y" part from the
// aria-label (e.g. "Delivery status: Out for delivery;
// Step 3 of 4." → "Step 3 of 4"). Returns "" if the
// aria-label doesn't contain a "Step " segment — the
// format helper then drops the step portion from the
// notification value.
func extractStepCount(ariaLabel string) string {
	i := strings.Index(ariaLabel, "Step ")
	if i < 0 {
		return ""
	}
	rest := ariaLabel[i:]
	// The step count ends at the next ';' or '.' or the
	// end of the string.
	for j := 0; j < len(rest); j++ {
		if c := rest[j]; c == ';' || c == '.' {
			return strings.TrimSpace(rest[:j])
		}
	}
	return strings.TrimSpace(rest)
}
