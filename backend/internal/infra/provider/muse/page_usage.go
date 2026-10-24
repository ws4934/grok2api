package muse

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// readUsage opens Settings from the dock menu, then asks the app's
// subscription action for the weekly usage; the dialog's Usage text is the
// fallback. Clicks right after load can be ignored, so each step is retried
// until the next element shows.
func (p *page) readUsage(ctx context.Context, location *time.Location) (usage, error) {
	type point struct{ X, Y int }
	deadline := time.Now().Add(20 * time.Second)
	for {
		var text string
		if err := p.js(ctx, "dialogText", &text); err != nil {
			return usage{}, err
		}
		if text != "" {
			break
		}
		var open bool
		if err := p.js(ctx, "menuOpen", &open); err != nil {
			return usage{}, err
		}
		target := "dockMore"
		if open {
			target = "settingsItem"
		}
		var pt *point
		if err := p.js(ctx, target, &pt); err != nil {
			return usage{}, err
		}
		if pt != nil {
			if err := p.click(ctx, pt.X, pt.Y); err != nil {
				return usage{}, err
			}
		}
		if time.Now().After(deadline) {
			return usage{}, errors.New("muse settings dialog did not open")
		}
		if err := sleep(ctx, 700*time.Millisecond); err != nil {
			return usage{}, err
		}
	}
	deadline = time.Now().Add(15 * time.Second)
	for {
		// Some sessions never render the Usage row (seen from a datacenter
		// IP); the action behind it still answers.
		var sub struct {
			Used     *float64 `json:"used"`
			ResetsAt int64    `json:"resetsAt"`
			Plan     string   `json:"plan"`
			Error    string   `json:"error"`
		}
		if err := p.js(ctx, "subscription", &sub); err == nil && sub.Used != nil && *sub.Used >= 0 && *sub.Used <= 100 {
			u := usage{Plan: sub.Plan, UsedPercent: *sub.Used}
			if sub.ResetsAt > 0 {
				u.ResetAt = time.Unix(sub.ResetsAt, 0).UTC()
			}
			return u, nil
		} else if err == nil && sub.Error != "" {
			p.log.Debug("muse subscription action unavailable", "error", sub.Error)
		}
		var text string
		if err := p.js(ctx, "dialogText", &text); err != nil {
			return usage{}, err
		}
		if u, err := parseUsage(text, time.Now().In(location)); err == nil {
			return u, nil
		}
		if time.Now().After(deadline) {
			return usage{}, errors.New("muse settings showed no weekly usage")
		}
		if err := sleep(ctx, 250*time.Millisecond); err != nil {
			return usage{}, err
		}
	}
}

var (
	usagePlan    = regexp.MustCompile(`(?i)^.{1,60}?(?:\s+plan|計劃|计划|方案)$`)
	usageResetEN = regexp.MustCompile(`(?i)weekly limit resets? on ([^\n]+)`)
	usageResetZH = regexp.MustCompile(`(?:每(?:週|周|星期)|週|周)(?:上限|限額|限额|額度|额度)(?:會|会|將|将)?(?:於|于|在)\s*([^\n]+?)\s*(?:重設|重置)`)
	usageExtra   = regexp.MustCompile(`(?i)^(?:(?:additional|extra)\s+tokens|(?:額外|额外)(?:的)?(?:符元|符記|代幣|代币|令牌|憑證|凭证|tokens))`)
	usageUsedA   = regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)\s*%\s*(?:used\b|已(?:使)?用)`)
	usageUsedB   = regexp.MustCompile(`已(?:使)?用\s*(\d+(?:\.\d+)?)\s*%`)
	dateEN       = regexp.MustCompile(`(?i)\b(jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)[a-z]*\.?\s+(\d{1,2})(?:\D+(\d{4}))?`)
	dateZH       = regexp.MustCompile(`(?:(\d{4})\s*年\s*)?(\d{1,2})\s*月\s*(\d{1,2})`)
	monthIndex   = map[string]time.Month{"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6, "jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12}
)

// parseUsage reads the Settings dialog text, in English or Chinese:
//
//	Usage / Free plan / Weekly limit resets on Oct 31 / 2% used
//
// A missing weekly percentage is an error, never 0%.
func parseUsage(text string, now time.Time) (usage, error) {
	text = strings.Map(func(r rune) rune {
		switch {
		case r >= '０' && r <= '９':
			return '0' + (r - '０')
		case r == '％':
			return '%'
		}
		return r
	}, text)
	var lines []string
	for _, l := range strings.Split(text, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	// Keep to the Usage section: account details come before it.
	for i, l := range lines {
		if k := strings.ToLower(l); k == "usage" || k == "用量" || k == "使用量" {
			lines = lines[i+1:]
			break
		}
	}
	for i, l := range lines {
		if k := strings.ToLower(l); k == "language" || k == "语言" || k == "語言" {
			lines = lines[:i]
			break
		}
	}
	var u usage
	for _, l := range lines {
		if usagePlan.MatchString(l) {
			u.Plan = l
			break
		}
	}
	found := false
	for _, l := range lines {
		if usageExtra.MatchString(l) {
			break // the extra-token pack has its own percentage
		}
		m := usageUsedA.FindStringSubmatch(l)
		if m == nil {
			m = usageUsedB.FindStringSubmatch(l)
		}
		if m == nil {
			continue
		}
		v, err := strconv.ParseFloat(m[1], 64)
		if err != nil || v < 0 || v > 100 {
			continue
		}
		u.UsedPercent, found = v, true
		break
	}
	if !found {
		return usage{}, errors.New("no weekly usage percentage")
	}
	section := strings.Join(lines, "\n")
	m := usageResetEN.FindStringSubmatch(section)
	if m == nil {
		m = usageResetZH.FindStringSubmatch(section)
	}
	if m != nil {
		u.ResetAt = resetDate(m[1], now)
	}
	return u, nil
}

// resetDate turns "Oct 31" or "10月31日" into that day 00:00 UTC, in the
// current year unless that is more than a day past (then the next year).
func resetDate(s string, now time.Time) time.Time {
	var year, day int
	var month time.Month
	if m := dateEN.FindStringSubmatch(s); m != nil {
		month = monthIndex[strings.ToLower(m[1])]
		day, _ = strconv.Atoi(m[2])
		year, _ = strconv.Atoi(m[3])
	} else if m := dateZH.FindStringSubmatch(s); m != nil {
		year, _ = strconv.Atoi(m[1])
		mon, _ := strconv.Atoi(m[2])
		month = time.Month(mon)
		day, _ = strconv.Atoi(m[3])
	} else {
		return time.Time{}
	}
	explicit := year != 0
	if !explicit {
		year = now.Year()
	}
	// The date is local to the page's timezone, which now carries.
	t := time.Date(year, month, day, 0, 0, 0, 0, now.Location())
	if t.Month() != month || t.Day() != day {
		return time.Time{} // Feb 30 and friends
	}
	if !explicit && t.Before(now.Add(-24*time.Hour)) {
		t = t.AddDate(1, 0, 0)
	}
	return t
}
