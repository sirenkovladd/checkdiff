package source

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// amazonExampleHTML is the structure from the user's example
// (Out for delivery; Step 3 of 4; 51%). The minified form is
// used so the HTML parser sees a single root <html> element
// wrapping the div; the indentation in the user's snippet
// would be fine too, but minified keeps the test data
// compact.
const amazonExampleHTML = `<!doctype html><html><body><div role="list" class="pt-status-milestones" aria-label="Delivery status: Out for delivery; Step 3 of 4.">` +
	`<div role="listitem" class="pt-status-milestone" data-reached="true" data-last-reached="false" data-percent-complete="100">` +
	`<div class="pt-status-milestone-marker"><div class="pt-status-milestone-marker-check"></div></div>` +
	`<div class="pt-status-milestone-label" role="img" aria-label="Ordered: Complete">Ordered</div>` +
	`</div>` +
	`<div role="listitem" class="pt-status-milestone" data-reached="true" data-last-reached="false" data-percent-complete="100">` +
	`<div class="pt-status-milestone-bar"><div class="pt-status-milestone-bar-progress"></div></div>` +
	`<div class="pt-status-milestone-marker"><div class="pt-status-milestone-marker-check"></div></div>` +
	`<div class="pt-status-milestone-label" role="img" aria-label="Shipped: Complete">Shipped</div>` +
	`</div>` +
	`<div role="listitem" class="pt-status-milestone" data-reached="true" data-last-reached="true" data-percent-complete="100">` +
	`<div class="pt-status-milestone-bar"><div class="pt-status-milestone-bar-progress"></div></div>` +
	`<div class="pt-status-milestone-marker"><div class="pt-status-milestone-marker-check"></div></div>` +
	`<div class="pt-status-milestone-label" role="img" aria-label="Out for delivery: Complete">Out for delivery</div>` +
	`</div>` +
	`<div role="listitem" class="pt-status-milestone" data-reached="false" data-last-reached="false" data-percent-complete="51">` +
	`<div class="pt-status-milestone-bar"><div class="pt-status-milestone-bar-progress"></div></div>` +
	`<div class="pt-status-milestone-marker"><div class="pt-status-milestone-marker-check"></div></div>` +
	`<div class="pt-status-milestone-label" role="img" aria-label="Delivered: Incomplete">Delivered</div>` +
	`</div>` +
	`</div></body></html>`

// amazonDeliveredHTML represents the "package is fully
// delivered" state: the last-reached milestone is the LAST
// one in the list, so there's no "next" milestone to read
// the in-progress percent from. The fetcher must fall back
// to the current step's own percent (100) in that case.
const amazonDeliveredHTML = `<!doctype html><html><body><div role="list" class="pt-status-milestones" aria-label="Delivery status: Delivered; Step 4 of 4.">` +
	`<div role="listitem" class="pt-status-milestone" data-reached="true" data-last-reached="false" data-percent-complete="100">` +
	`<div class="pt-status-milestone-label">Ordered</div>` +
	`</div>` +
	`<div role="listitem" class="pt-status-milestone" data-reached="true" data-last-reached="true" data-percent-complete="100">` +
	`<div class="pt-status-milestone-label">Delivered</div>` +
	`</div>` +
	`</div></body></html>`

func TestParseAmazonStatusExample(t *testing.T) {
	// The user's example: Out for delivery, Step 3 of 4, 51%
	// progress (on the next milestone, "Delivered"). The
	// Value() must match the user's example exactly.
	got, err := parseAmazonStatus([]byte(amazonExampleHTML))
	if err != nil {
		t.Fatalf("parseAmazonStatus: %v", err)
	}
	if got.CurrentStep != "Out for delivery" {
		t.Errorf("CurrentStep = %q, want %q", got.CurrentStep, "Out for delivery")
	}
	if got.StepCount != "Step 3 of 4" {
		t.Errorf("StepCount = %q, want %q", got.StepCount, "Step 3 of 4")
	}
	if got.Percent != "51" {
		t.Errorf("Percent = %q, want %q (from the next milestone's data-percent-complete)", got.Percent, "51")
	}
	if got.AriaLabel != "Delivery status: Out for delivery; Step 3 of 4." {
		t.Errorf("AriaLabel = %q", got.AriaLabel)
	}
	if v := got.Value(); v != "Out for delivery; Step 3 of 4 (51%)" {
		t.Errorf("Value = %q, want %q", v, "Out for delivery; Step 3 of 4 (51%)")
	}
}

func TestParseAmazonStatusDelivered(t *testing.T) {
	// When the package is fully delivered, the last-reached
	// milestone is the LAST one in the list. The fetcher
	// must fall back to the current step's own percent
	// (100), not panic or return "".
	got, err := parseAmazonStatus([]byte(amazonDeliveredHTML))
	if err != nil {
		t.Fatalf("parseAmazonStatus: %v", err)
	}
	if got.CurrentStep != "Delivered" {
		t.Errorf("CurrentStep = %q, want %q", got.CurrentStep, "Delivered")
	}
	if got.Percent != "100" {
		t.Errorf("Percent = %q, want %q (current step's own percent)", got.Percent, "100")
	}
	if v := got.Value(); v != "Delivered; Step 4 of 4 (100%)" {
		t.Errorf("Value = %q, want %q", v, "Delivered; Step 4 of 4 (100%)")
	}
}

func TestParseAmazonStatusMissingDiv(t *testing.T) {
	// A sign-in redirect (or a bot-detection challenge) returns
	// HTML without the status div. parseAmazonStatus must
	// surface a clear, user-actionable error rather than
	// panicking or returning a zero value.
	_, err := parseAmazonStatus([]byte(`<!doctype html><html><body>Please sign in</body></html>`))
	if err == nil {
		t.Fatalf("parseAmazonStatus: got nil error, want error")
	}
	if !strings.Contains(err.Error(), "delivery status div not found") {
		t.Errorf("error message should mention 'delivery status div not found', got %q", err.Error())
	}
}

func TestParseAmazonStatusNoLastReached(t *testing.T) {
	// The page is otherwise well-formed but every milestone
	// has data-last-reached="false" (e.g. tracking data is
	// being prepared but not yet finalised). The fetcher
	// must error rather than silently returning an empty
	// current step.
	html := `<div role="list" class="pt-status-milestones" aria-label="Delivery status: ?">` +
		`<div role="listitem" class="pt-status-milestone" data-reached="false" data-last-reached="false" data-percent-complete="0">` +
		`<div class="pt-status-milestone-label">Ordered</div>` +
		`</div></div>`
	_, err := parseAmazonStatus([]byte(html))
	if err == nil {
		t.Fatalf("parseAmazonStatus: got nil error, want error")
	}
	if !strings.Contains(err.Error(), "current step") {
		t.Errorf("error message should mention 'current step', got %q", err.Error())
	}
}

func TestAmazonStatusValueFormat(t *testing.T) {
	// The Value() string is the Item's ID and the
	// notification's title. Every branch of the format
	// helper must produce a coherent string (no stray
	// parens, no missing step).
	cases := []struct {
		name string
		s    amazonStatus
		want string
	}{
		{
			name: "all parts present",
			s:    amazonStatus{CurrentStep: "Out for delivery", StepCount: "Step 3 of 4", Percent: "51"},
			want: "Out for delivery; Step 3 of 4 (51%)",
		},
		{
			name: "no step count",
			s:    amazonStatus{CurrentStep: "Out for delivery", Percent: "51"},
			want: "Out for delivery (51%)",
		},
		{
			name: "no percent",
			s:    amazonStatus{CurrentStep: "Out for delivery", StepCount: "Step 3 of 4"},
			want: "Out for delivery; Step 3 of 4",
		},
		{
			name: "only current step",
			s:    amazonStatus{CurrentStep: "Delivered"},
			want: "Delivered",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.s.Value(); got != c.want {
				t.Errorf("Value() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestExtractStepCount(t *testing.T) {
	cases := []struct {
		name      string
		ariaLabel string
		want      string
	}{
		{
			name:      "full example",
			ariaLabel: "Delivery status: Out for delivery; Step 3 of 4.",
			want:      "Step 3 of 4",
		},
		{
			name:      "delivered",
			ariaLabel: "Delivery status: Delivered; Step 4 of 4.",
			want:      "Step 4 of 4",
		},
		{
			name:      "no step segment",
			ariaLabel: "Delivery status: Delivered.",
			want:      "",
		},
		{
			name:      "no trailing dot",
			ariaLabel: "Delivery status: Shipped; Step 2 of 4",
			want:      "Step 2 of 4",
		},
		{
			name:      "empty aria",
			ariaLabel: "",
			want:      "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := extractStepCount(c.ariaLabel); got != c.want {
				t.Errorf("extractStepCount(%q) = %q, want %q", c.ariaLabel, got, c.want)
			}
		})
	}
}

func TestDefaultAmazonReferer(t *testing.T) {
	// The default Referer is the order history page on the
	// same host as the tracking URL. The order history path
	// is the same on all Amazon regions (/gp/css/order-history/).
	cases := []struct {
		name string
		url  string
		want string
	}{
		{
			name: "amazon.ca tracking URL",
			url:  "https://www.amazon.ca/gp/your-account/ship-track?itemId=jmmplvkqtlmwxsp&orderId=702-3736279-7382631&shipmentId=N4dHz0mQb",
			want: "https://www.amazon.ca/gp/css/order-history/",
		},
		{
			name: "amazon.com tracking URL",
			url:  "https://www.amazon.com/gp/your-account/ship-track?itemId=abc&orderId=123&shipmentId=xyz",
			want: "https://www.amazon.com/gp/css/order-history/",
		},
		{
			name: "no path (just host)",
			url:  "https://www.amazon.de",
			want: "https://www.amazon.de/gp/css/order-history/",
		},
		{
			name: "no scheme",
			url:  "amazon.ca/something",
			want: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := defaultAmazonReferer(c.url); got != c.want {
				t.Errorf("defaultAmazonReferer(%q) = %q, want %q", c.url, got, c.want)
			}
		})
	}
}

func TestAmazonFetcherValidate(t *testing.T) {
	v := amazonFetcher{}
	// url is required.
	if err := v.Validate(&Source{Type: "amazon"}); err == nil {
		t.Errorf("missing url: got nil error, want error")
	}
	// url present is sufficient; cookies/referer are optional.
	if err := v.Validate(&Source{Type: "amazon", URL: "https://www.amazon.ca/gp/your-account/ship-track?itemId=abc"}); err != nil {
		t.Errorf("validate ok case: %v", err)
	}
}

func TestAmazonFetcherFormat(t *testing.T) {
	// The diff for an amazon source is one removed (old
	// status) + one added (new status). The format helper
	// surfaces both and includes the source's URL as the
	// Click target so tapping the notification opens the
	// tracking page.
	s := &Source{
		ID:   "pkg-1",
		Name: "Package 1",
		Type: "amazon",
		URL:  "https://www.amazon.ca/gp/your-account/ship-track?itemId=abc",
	}
	added := []Item{{ID: "Out for delivery; Step 3 of 4 (51%)", Title: "Out for delivery; Step 3 of 4 (51%)"}}
	removed := []Item{{ID: "Shipped; Step 2 of 4 (75%)", Title: "Shipped; Step 2 of 4 (75%)"}}

	n := amazonFetcher{}.Format(context.Background(), s, added, removed)

	if !strings.Contains(n.Title, "Package 1") {
		t.Errorf("Title should name the source, got %q", n.Title)
	}
	if !strings.Contains(n.Title, "Out for delivery") {
		t.Errorf("Title should show the new status, got %q", n.Title)
	}
	if !strings.Contains(n.Body, "From:") || !strings.Contains(n.Body, "Shipped") {
		t.Errorf("Body should show the old status, got:\n%s", n.Body)
	}
	if !strings.Contains(n.Body, "To:") || !strings.Contains(n.Body, "Out for delivery") {
		t.Errorf("Body should show the new status, got:\n%s", n.Body)
	}
	if n.Click != s.URL {
		t.Errorf("Click = %q, want source URL %q", n.Click, s.URL)
	}
	if n.Tags != "package" {
		t.Errorf("Tags = %q, want %q", n.Tags, "package")
	}
}

func TestAmazonFetcherFetchIntegration(t *testing.T) {
	// Drives the full Fetch against a test server serving
	// the example HTML, then asserts the returned Item has
	// the expected ID, Title, and Body. This is the closest
	// we can get to a real end-to-end test without hitting
	// amazon.ca.
	var sawUserAgent string
	var sawCookie string
	var sawReferer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawUserAgent = r.Header.Get("User-Agent")
		sawCookie = r.Header.Get("Cookie")
		sawReferer = r.Header.Get("Referer")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(amazonExampleHTML))
	}))
	defer srv.Close()

	s := &Source{
		ID:      "pkg-1",
		Name:    "Package 1",
		Type:    "amazon",
		URL:     srv.URL + "/gp/your-account/ship-track?itemId=abc",
		Cookies: "session-id=135-3461069-5839019; ubid-acbca=132-3664403-5444509",
		Referer: "https://www.amazon.ca/gp/css/order-history/",
	}

	items, err := amazonFetcher{}.Fetch(context.Background(), s, time.Now())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("Fetch: got %d items, want 1", len(items))
	}
	want := "Out for delivery; Step 3 of 4 (51%)"
	if items[0].ID != want {
		t.Errorf("Item.ID = %q, want %q", items[0].ID, want)
	}
	if items[0].Title != want {
		t.Errorf("Item.Title = %q, want %q", items[0].Title, want)
	}
	if !strings.Contains(items[0].Body, "Delivery status") {
		t.Errorf("Item.Body should contain the aria-label, got %q", items[0].Body)
	}

	// The fetch must send the cookies and referer through to
	// the server. The User-Agent must look like a real
	// browser, not checkdiff's default "checkdiff/0.1"
	// string, because Amazon serves different content for
	// bot-like UAs.
	if sawUserAgent == "" || !strings.Contains(sawUserAgent, "Mozilla") {
		t.Errorf("User-Agent = %q, want a Mozilla-style browser string", sawUserAgent)
	}
	if sawCookie != s.Cookies {
		t.Errorf("Cookie header = %q, want %q", sawCookie, s.Cookies)
	}
	if sawReferer != s.Referer {
		t.Errorf("Referer header = %q, want %q", sawReferer, s.Referer)
	}
}

func TestAmazonFetcherFetchDefaultReferer(t *testing.T) {
	// When the source's Referer is empty, the fetcher
	// must derive one from the URL's host (the standard
	// /gp/css/order-history/ path on the same host).
	var sawReferer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawReferer = r.Header.Get("Referer")
		w.Write([]byte(amazonExampleHTML))
	}))
	defer srv.Close()

	s := &Source{
		Type: "amazon",
		URL:  srv.URL + "/gp/your-account/ship-track?itemId=abc",
	}
	items, err := amazonFetcher{}.Fetch(context.Background(), s, time.Now())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	// Just assert that the fetch succeeded and produced
	// exactly one item; the ID was already checked in the
	// main integration test above.
	if len(items) != 1 {
		t.Errorf("Fetch: got %d items, want 1", len(items))
	}
	// httptest.NewServer gives us a 127.0.0.1:port URL;
	// the default referer should be that scheme+host plus
	// the order history path. The exact host:port varies,
	// so check the suffix.
	if !strings.HasSuffix(sawReferer, "/gp/css/order-history/") {
		t.Errorf("default Referer = %q, want suffix /gp/css/order-history/", sawReferer)
	}
}

func TestAmazonFetcherFetchHTTPError(t *testing.T) {
	// A 5xx response from the server must surface as a
	// fetch error (so the user gets a high-priority
	// warning notification, not a silent no-diff).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	s := &Source{Type: "amazon", URL: srv.URL + "/gp/your-account/ship-track?itemId=abc"}
	_, err := amazonFetcher{}.Fetch(context.Background(), s, time.Now())
	if err == nil {
		t.Fatalf("Fetch: got nil error, want error on 5xx")
	}
}

func TestAmazonFetcherFetchSignInRedirect(t *testing.T) {
	// The most common real-world failure: cookies are
	// missing/expired, Amazon returns a sign-in page that
	// has no status div. The fetcher must return a clear
	// error, not a zero value (which would silently
	// produce a "no diff" notification and hide the
	// problem).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<!doctype html><html><body>Please sign in to view this page.</body></html>`))
	}))
	defer srv.Close()

	s := &Source{Type: "amazon", URL: srv.URL + "/gp/your-account/ship-track?itemId=abc"}
	_, err := amazonFetcher{}.Fetch(context.Background(), s, time.Now())
	if err == nil {
		t.Fatalf("Fetch: got nil error, want error when status div is missing")
	}
	if !strings.Contains(err.Error(), "delivery status div not found") {
		t.Errorf("error should mention missing status div, got %q", err.Error())
	}
}
