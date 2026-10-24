package muse

import (
	"testing"
	"time"
)

func TestParseUsage(t *testing.T) {
	now := time.Date(2026, 10, 24, 15, 0, 0, 0, time.UTC)
	date := func(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }
	cases := []struct {
		name string
		text string
		want usage
	}{
		{"english panel", "Settings\nGeneral\nConnectors\nLog out\nGeneral\nMeta Account\nPassword, security, personal details\nUsage\nFree plan\nWeekly limit resets on Oct 31\n2% used\nUpgrade\nRedeem invite code\nLanguage\nAppearance",
			usage{Plan: "Free plan", UsedPercent: 2, ResetAt: date(2026, 10, 31)}},
		{"extra tokens are not the weekly limit", "Usage\nPro plan\nWeekly limit resets on Sep 30\n37.5% used\nAdditional tokens\nNever expires\n0% used (2B tokens left)",
			usage{Plan: "Pro plan", UsedPercent: 37.5, ResetAt: date(2027, 9, 30)}},
		{"simplified chinese", "设置\n用量\n免费方案\n每周额度将于 10月31日 重置\n已使用 12%\n语言",
			usage{Plan: "免费方案", UsedPercent: 12, ResetAt: date(2026, 10, 31)}},
		{"traditional chinese, full-width digits", "使用量\n免費計劃\n每週上限將於11月2日重設\n５％ 已使用",
			usage{Plan: "免費計劃", UsedPercent: 5, ResetAt: date(2026, 11, 2)}},
		{"reset yesterday stays this year", "Usage\nFree plan\nWeekly limit resets on Oct 24\n100% used",
			usage{Plan: "Free plan", UsedPercent: 100, ResetAt: date(2026, 10, 24)}},
		{"january seen in december", "Usage\nWeekly limit resets on January 3\n1% used",
			usage{UsedPercent: 1, ResetAt: date(2027, 1, 3)}},
		{"no reset line", "Usage\nFree plan\n0% used",
			usage{Plan: "Free plan"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			at := now
			if tc.name == "january seen in december" {
				at = time.Date(2026, 12, 30, 0, 0, 0, 0, time.UTC)
			}
			got, err := parseUsage(tc.text, at)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
	for _, text := range []string{"", "Usage\nFree plan\nWeekly limit resets on Oct 31", "Usage\nFree plan\n250% used"} {
		if got, err := parseUsage(text, now); err == nil {
			t.Errorf("parseUsage(%q) = %+v, want an error", text, got)
		}
	}
}
