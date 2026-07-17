//go:build ignore

package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	scotlandISEventsURL = "https://www.scotlandis.com/events/"
	scotlandISAjaxURL   = "https://www.scotlandis.com/wp-admin/admin-ajax.php"
	bayesEventsURL      = "https://www.hub.bayes.ed.ac.uk/events"
	bayesContextURL     = "https://inffuse-platform.appspot.com/js/v0.1/calendar/data?project_id=proj_3Q5i1tAnibf5vIpMGUvS7&exclude=services"
)

type event struct {
	Title    string `json:"title"`
	URL      string `json:"url"`
	Date     string `json:"date"`
	DateTime string `json:"datetime"`
	Detail   string `json:"detail"`
	start    time.Time
}

type partnerEvents struct {
	UpdatedAt  string  `json:"updatedAt"`
	ScotlandIS []event `json:"scotlandis"`
	Bayes      []event `json:"bayes"`
}

func main() {
	out := flag.String("out", "data/partner_events.json", "output JSON file")
	limit := flag.Int("limit", 3, "maximum events per partner")
	months := flag.Int("months", 2, "number of calendar months to scan, counting the current one")
	flag.Parse()

	if *months < 1 {
		*months = 1
	}

	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		fatal(err)
	}
	today := startOfDay(time.Now().In(loc))

	scotland, err := fetchScotlandIS(today, loc, *limit, *months)
	if err != nil {
		fatal(fmt.Errorf("fetch ScotlandIS events: %w", err))
	}
	bayes, err := fetchBayes(today, loc, *limit)
	if err != nil {
		fatal(fmt.Errorf("fetch Bayes events: %w", err))
	}

	payload := partnerEvents{
		UpdatedAt:  time.Now().In(loc).Format("2006-01-02"),
		ScotlandIS: scotland,
		Bayes:      bayes,
	}

	body, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		fatal(err)
	}
	body = append(body, '\n')

	if err := os.MkdirAll(filepath.Dir(*out), 0755); err != nil {
		fatal(err)
	}
	if err := os.WriteFile(*out, body, 0644); err != nil {
		fatal(err)
	}

	fmt.Printf("Updated %s with %d ScotlandIS events and %d Bayes events.\n", *out, len(scotland), len(bayes))
}

// The ScotlandIS calendar is an EventON widget that only renders one month at a
// time, so a quiet current month leaves the site with almost nothing to show.
// The landing page carries the focused month; each additional month is pulled
// from the same AJAX endpoint the calendar's "next" arrow uses.
func fetchScotlandIS(today time.Time, loc *time.Location, limit, months int) ([]event, error) {
	body, err := get(scotlandISEventsURL)
	if err != nil {
		return nil, err
	}
	page := string(body)

	events := parseScotlandISEvents(page, today, loc)

	// Both attribute sets have to be echoed back for the calendar to resolve
	// which month "next" means.
	evodata := dataAttrs(firstMatch(page, `<div class='evo-data'([^>]*)>`))
	shortcode := dataAttrs(firstMatch(page, `<div class='cal_arguments'([^>]*)>`))

	for i := 1; i < months; i++ {
		if evodata["cmonth"] == "" || evodata["cyear"] == "" {
			return nil, fmt.Errorf("calendar month %d: no month marker on %s", i+1, scotlandISEventsURL)
		}

		next, err := fetchScotlandISMonth(evodata, shortcode)
		if err != nil {
			return nil, fmt.Errorf("calendar month %d: %w", i+1, err)
		}

		events = append(events, parseScotlandISEvents(next.Content, today, loc)...)
		evodata["cmonth"], evodata["cyear"] = next.Month.String(), next.Year.String()
	}

	events = dedupe(events)
	sortEvents(events)
	return trim(events, limit), nil
}

type scotlandISMonth struct {
	Status  string      `json:"status"`
	Content string      `json:"content"`
	Month   json.Number `json:"month"`
	Year    json.Number `json:"year"`
}

// fetchScotlandISMonth advances the calendar one month past the state in
// evodata and returns the markup for that month.
func fetchScotlandISMonth(evodata, shortcode map[string]string) (*scotlandISMonth, error) {
	sortBy := evodata["sort_by"]
	if sortBy == "" {
		sortBy = "sort_date"
	}

	form := url.Values{}
	form.Set("action", "the_ajax_hook")
	form.Set("direction", "next")
	form.Set("ajaxtype", "switchmonth")
	form.Set("sort_by", sortBy)
	for key, value := range shortcode {
		form.Set("shortcode["+key+"]", value)
	}
	for key, value := range evodata {
		form.Set("evodata["+key+"]", value)
	}

	req, err := http.NewRequest(http.MethodPost, scotlandISAjaxURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "FoundersHubPartnerEvents/1.0")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("unexpected status %s", resp.Status)
	}

	var decoded scotlandISMonth
	dec := json.NewDecoder(resp.Body)
	dec.UseNumber()
	if err := dec.Decode(&decoded); err != nil {
		return nil, err
	}
	if decoded.Status != "GOOD" {
		return nil, fmt.Errorf("calendar returned status %q", decoded.Status)
	}
	if decoded.Month.String() == "" || decoded.Year.String() == "" {
		return nil, fmt.Errorf("calendar returned no month marker")
	}
	return &decoded, nil
}

// parseScotlandISEvents reads the shared EventON markup used by both the
// landing page and the month AJAX responses.
func parseScotlandISEvents(input string, today time.Time, loc *time.Location) []event {
	blocks := scotlandISEventBlocks(input)
	events := make([]event, 0, len(blocks))

	for _, block := range blocks {
		title := firstMatch(block, `<span itemprop='name'\s*>(.*?)</span>`)
		link := firstMatch(block, `<a itemprop='url'\s+href='([^']+)'`)
		startRaw := firstMatch(block, `<meta itemprop='startDate' content="([^"]+)"`)
		if title == "" || link == "" || startRaw == "" {
			continue
		}

		start, err := parseScotlandISStart(startRaw, loc)
		if err != nil {
			continue
		}
		if start.Before(today) {
			continue
		}

		detail := firstMatch(block, `data-location_name="([^"]*)"`)
		if detail == "" {
			detail = "ScotlandIS event"
		}

		events = append(events, event{
			Title:    clean(title),
			URL:      clean(link),
			Date:     start.Format("2 Jan 2006"),
			DateTime: start.Format("2006-01-02"),
			Detail:   clean(detail),
			start:    start,
		})
	}

	return events
}

// All-day events omit the time component.
func parseScotlandISStart(raw string, loc *time.Location) (time.Time, error) {
	for _, layout := range []string{"2006-1-2T15:04", "2006-1-2"} {
		if start, err := time.ParseInLocation(layout, raw, loc); err == nil {
			return start, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognised start date %q", raw)
}

func dataAttrs(input string) map[string]string {
	re := regexp.MustCompile(`data-([a-z_0-9]+)=["']([^"']*)["']`)
	attrs := map[string]string{}
	for _, match := range re.FindAllStringSubmatch(input, -1) {
		attrs[match[1]] = match[2]
	}
	return attrs
}

// Multi-day and repeating events can surface in more than one month.
func dedupe(events []event) []event {
	seen := make(map[string]bool, len(events))
	unique := make([]event, 0, len(events))
	for _, e := range events {
		key := e.URL + "|" + e.DateTime
		if seen[key] {
			continue
		}
		seen[key] = true
		unique = append(unique, e)
	}
	return unique
}

func fetchBayes(today time.Time, loc *time.Location, limit int) ([]event, error) {
	form := url.Values{
		"_origin":   []string{bayesEventsURL},
		"_referrer": []string{""},
	}
	req, err := http.NewRequest(http.MethodPost, bayesContextURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "FoundersHubPartnerEvents/1.0")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("unexpected status %s", resp.Status)
	}

	var decoded struct {
		Project struct {
			Data struct {
				Events []struct {
					Title       string      `json:"title"`
					URL         string      `json:"url"`
					Start       json.Number `json:"start"`
					Description string      `json:"description"`
					Location    string      `json:"location"`
				} `json:"events"`
			} `json:"data"`
		} `json:"project"`
	}
	dec := json.NewDecoder(resp.Body)
	dec.UseNumber()
	if err := dec.Decode(&decoded); err != nil {
		return nil, err
	}

	events := make([]event, 0, len(decoded.Project.Data.Events))
	for _, raw := range decoded.Project.Data.Events {
		ms, err := raw.Start.Int64()
		if err != nil || raw.Title == "" {
			continue
		}
		start := time.UnixMilli(ms).In(loc)
		if start.Before(today) {
			continue
		}

		detail := raw.Location
		if detail == "" {
			detail = truncate(clean(raw.Description), 150)
		}
		if detail == "" {
			detail = "Bayes Centre event"
		}

		link := raw.URL
		if link == "" {
			link = bayesEventsURL + "#UPCOMING-EVENTS"
		}

		events = append(events, event{
			Title:    clean(raw.Title),
			URL:      clean(link),
			Date:     start.Format("2 Jan 2006"),
			DateTime: start.Format("2006-01-02"),
			Detail:   detail,
			start:    start,
		})
	}

	sortEvents(events)
	return trim(events, limit), nil
}

func get(source string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, source, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "FoundersHubPartnerEvents/1.0")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("unexpected status %s", resp.Status)
	}
	return io.ReadAll(resp.Body)
}

func firstMatch(input, pattern string) string {
	re := regexp.MustCompile(pattern)
	match := re.FindStringSubmatch(input)
	if len(match) < 2 {
		return ""
	}
	return match[1]
}

func scotlandISEventBlocks(input string) []string {
	startRe := regexp.MustCompile(`<div id="event_[^"]+" class="eventon_list_event`)
	indexes := startRe.FindAllStringIndex(input, -1)
	blocks := make([]string, 0, len(indexes))
	for i, idx := range indexes {
		end := len(input)
		if i+1 < len(indexes) {
			end = indexes[i+1][0]
		} else if lightbox := strings.Index(input[idx[0]:], "<div class='evo_lightboxes'"); lightbox >= 0 {
			end = idx[0] + lightbox
		}
		blocks = append(blocks, input[idx[0]:end])
	}
	return blocks
}

func clean(input string) string {
	noTags := regexp.MustCompile(`(?s)<[^>]+>`).ReplaceAllString(input, " ")
	unescaped := html.UnescapeString(noTags)
	return strings.Join(strings.Fields(unescaped), " ")
}

func truncate(input string, max int) string {
	input = strings.TrimSpace(input)
	if len(input) <= max {
		return input
	}
	cut := max
	for cut > 0 && input[cut] != ' ' {
		cut--
	}
	if cut < max/2 {
		cut = max
	}
	return strings.TrimSpace(input[:cut]) + "..."
}

func sortEvents(events []event) {
	sort.Slice(events, func(i, j int) bool {
		if events[i].start.Equal(events[j].start) {
			return events[i].Title < events[j].Title
		}
		return events[i].start.Before(events[j].start)
	})
}

func trim(events []event, limit int) []event {
	if limit > 0 && len(events) > limit {
		return events[:limit]
	}
	return events
}

func startOfDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

func fatal(err error) {
	var out bytes.Buffer
	out.WriteString("update partner events: ")
	out.WriteString(err.Error())
	out.WriteByte('\n')
	fmt.Fprint(os.Stderr, out.String())
	os.Exit(1)
}
