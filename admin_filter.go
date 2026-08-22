package main

import (
	"html/template"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const maxAdminUsageRange = 366 * 24 * time.Hour

type adminWindow struct {
	Range  string
	Bucket string
	From   time.Time
	To     time.Time
}

type adminFilter struct {
	Range    string
	Bucket   string
	From     time.Time
	To       time.Time
	Bounded  bool
	TZ       string
	Model    string
	Provider string
	APIKey   string
	Outcome  string
}

type selectOption struct {
	Value string
	Label string
	On    bool
}

type filterOptions struct {
	Models    []string
	Providers []string
	Keys      []string
}

func parseAdminWindow(q url.Values, now time.Time) adminWindow {
	f := parseAdminFilter(q, now, "usage")
	return adminWindow{Range: f.Range, Bucket: f.Bucket, From: f.From, To: f.To}
}

func parseAdminFilter(q url.Values, now time.Time, kind string) adminFilter {
	now = now.UTC()
	f := adminFilter{
		Model:    strings.TrimSpace(q.Get("model")),
		Provider: strings.TrimSpace(q.Get("provider")),
		APIKey:   strings.TrimSpace(q.Get("api_key")),
		Outcome:  parseOutcome(q.Get("outcome")),
	}
	if tz := strings.TrimSpace(q.Get("tz")); tz != "" {
		if loc, err := time.LoadLocation(tz); err == nil {
			f.TZ = loc.String()
		}
	}
	loc := f.tzLoc()
	rng := q.Get("range")
	if kind == "calls" {
		switch rng {
		case "24h", "7d", "30d", "90d", "custom", "all":
		default:
			rng = "all"
		}
	} else {
		switch rng {
		case "24h", "7d", "30d", "90d", "custom":
		default:
			rng = "7d"
		}
	}
	f.Range = rng

	switch rng {
	case "all":
		// unbounded
	case "custom":
		from, okFrom := parseAdminTime(q.Get("from"), loc)
		to, okTo := parseAdminTime(q.Get("to"), loc)
		if !okFrom || !okTo || !to.After(from) || (kind == "usage" && to.Sub(from) > maxAdminUsageRange) {
			if kind == "calls" {
				f.Range = "all"
			} else {
				f.Range = "7d"
				f.Bounded = true
				f.From, f.To = presetWindow("7d", now)
			}
			break
		}
		f.Bounded = true
		f.From = from
		f.To = to
	default:
		f.Bounded = true
		f.From, f.To = presetWindow(rng, now)
	}
	if f.Bounded {
		f.Bucket = deriveAdminBucket(f.From, f.To)
	}
	return f
}

// tzLoc resolves the filter's display timezone, defaulting to UTC.
func (f adminFilter) tzLoc() *time.Location {
	if f.TZ != "" {
		if loc, err := time.LoadLocation(f.TZ); err == nil {
			return loc
		}
	}
	return time.UTC
}

// deriveAdminBucket picks the bucket granularity from the window span; users
// no longer choose it. Short windows get hours, medium days, long weeks.
func deriveAdminBucket(from, to time.Time) string {
	switch span := to.Sub(from); {
	case span <= 48*time.Hour:
		return "hour"
	case span <= 62*24*time.Hour:
		return "day"
	default:
		return "week"
	}
}

func parseOutcome(s string) string {
	switch strings.TrimSpace(s) {
	case "ok", "error", "canceled", "no_response":
		return s
	default:
		return ""
	}
}

func presetWindow(rng string, now time.Time) (time.Time, time.Time) {
	var dur time.Duration
	switch rng {
	case "24h":
		dur = 24 * time.Hour
	case "30d":
		dur = 30 * 24 * time.Hour
	case "90d":
		dur = 90 * 24 * time.Hour
	default:
		dur = 7 * 24 * time.Hour
	}
	return now.Add(-dur), now
}

// parseAdminTime parses a custom range endpoint. Offset-bearing RFC3339 is
// honored as given; naive layouts are interpreted in loc (the selected tz).
func parseAdminTime(s string, loc *time.Location) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC(), true
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), true
	}
	layouts := []string{
		"2006-01-02T15:04:05",
		"2006-01-02T15:04",
		"2006-01-02 15:04:05",
		"2006-01-02",
	}
	for _, layout := range layouts {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

func (f adminFilter) window() adminWindow {
	return adminWindow{Range: f.Range, Bucket: f.Bucket, From: f.From, To: f.To}
}

func (f adminFilter) values(kind string) url.Values {
	v := url.Values{}
	switch {
	case f.Range == "":
	case kind == "calls" && f.Range == "all":
	case kind == "usage" && f.Range == "7d":
	default:
		v.Set("range", f.Range)
	}
	if f.Range == "custom" && f.Bounded {
		if !f.From.IsZero() {
			v.Set("from", f.From.UTC().Format(time.RFC3339))
		}
		if !f.To.IsZero() {
			v.Set("to", f.To.UTC().Format(time.RFC3339))
		}
	}
	if f.TZ != "" && f.TZ != "UTC" {
		v.Set("tz", f.TZ)
	}
	if f.Model != "" {
		v.Set("model", f.Model)
	}
	if f.Provider != "" {
		v.Set("provider", f.Provider)
	}
	if f.APIKey != "" {
		v.Set("api_key", f.APIKey)
	}
	if f.Outcome != "" {
		v.Set("outcome", f.Outcome)
	}
	return v
}

func (f adminFilter) path(kind, base string) string {
	enc := f.values(kind).Encode()
	if enc == "" {
		return base
	}
	return base + "?" + enc
}

func (f adminFilter) forUsage() adminFilter {
	g := f
	if g.Range == "all" || g.Range == "" {
		g.Range = "7d"
		g.Bounded = true
		g.From = time.Time{}
		g.To = time.Time{}
	}
	return g
}

func mergeFilterView(data map[string]any, f adminFilter, kind string, opts filterOptions) {
	ranges := []string{"24h", "7d", "30d", "90d"}
	if kind == "calls" {
		ranges = append([]string{"all"}, ranges...)
	}
	ranges = append(ranges, "custom")
	rangeOpts := make([]selectOption, 0, len(ranges))
	for _, rng := range ranges {
		rangeOpts = append(rangeOpts, selectOption{Value: rng, Label: rng, On: f.Range == rng})
	}
	data["RangeOpts"] = rangeOpts
	data["ModelOpts"] = mergeOption(opts.Models, f.Model)
	data["ProviderOpts"] = mergeOption(opts.Providers, f.Provider)
	data["KeyOpts"] = mergeOption(opts.Keys, f.APIKey)
	tz := f.TZ
	if tz == "" {
		tz = "UTC"
	}
	data["TZone"] = tz
	data["TZExplicit"] = f.TZ != ""
	// RangeParam mirrors values(): omit the range hidden input when it is the
	// kind's default so the dim-filter form does not pin it explicitly.
	rangeParam := f.Range
	if rangeParam == "" || (kind == "usage" && rangeParam == "7d") || (kind == "calls" && rangeParam == "all") {
		rangeParam = ""
	}
	data["RangeParam"] = rangeParam
	if f.Bounded {
		loc := f.tzLoc()
		data["FromLocal"] = f.From.In(loc).Format("2006-01-02T15:04")
		data["ToLocal"] = f.To.In(loc).Format("2006-01-02T15:04")
		data["FromRFC"] = f.From.UTC().Format(time.RFC3339)
		data["ToRFC"] = f.To.UTC().Format(time.RFC3339)
	} else {
		data["FromLocal"] = ""
		data["ToLocal"] = ""
		data["FromRFC"] = ""
		data["ToRFC"] = ""
	}
}

func mergeOption(opts []string, extra string) []string {
	if extra == "" {
		return opts
	}
	for _, o := range opts {
		if o == extra {
			return opts
		}
	}
	return append([]string{extra}, opts...)
}

func pagerURL(f adminFilter, page int) string {
	v := f.values("calls")
	if page > 1 {
		v.Set("page", strconv.Itoa(page))
	}
	enc := v.Encode()
	if enc == "" {
		return "/admin/calls"
	}
	return "/admin/calls?" + enc
}

// pagerItem is one slot of the calls pager: either a numbered page link or an
// ellipsis gap.
type pagerItem struct {
	Num    int
	URL    template.URL
	Active bool
	Gap    bool
}

// pagerItems builds a compact numbered window around page: the first and last
// page plus current ±2, with ellipsis gaps between runs.
func pagerItems(f adminFilter, page, totalPages int) []pagerItem {
	show := map[int]bool{1: true, totalPages: true}
	for p := page - 2; p <= page+2; p++ {
		if p >= 1 && p <= totalPages {
			show[p] = true
		}
	}
	var items []pagerItem
	prev := 0
	for p := 1; p <= totalPages; p++ {
		if !show[p] {
			continue
		}
		if prev > 0 && p-prev > 1 {
			items = append(items, pagerItem{Gap: true})
		}
		// template.URL: built by pagerURL from our own params, same as ListQuery.
		items = append(items, pagerItem{Num: p, URL: template.URL(pagerURL(f, p)), Active: p == page})
		prev = p
	}
	return items
}
