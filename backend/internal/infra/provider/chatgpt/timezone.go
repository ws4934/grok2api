package chatgpt

import (
	"context"
	"time"
)

type openAITimezoneSnapshot struct {
	location *time.Location
	at       time.Time
	egress   [32]byte
}

func (s openAITimezoneSnapshot) offset() int {
	_, seconds := s.at.In(s.location).Zone()
	return -seconds / 60
}
func (s openAITimezoneSnapshot) date() string {
	local := s.at.In(s.location)
	return local.Format("Mon Jan 02 2006 15:04:05 GMT-0700 (MST)")
}
func openAITimezoneFrom(ctx context.Context) openAITimezoneSnapshot {
	if s, ok := ctx.Value(openAITimezoneKey{}).(openAITimezoneSnapshot); ok {
		return s
	}
	return openAITimezoneSnapshot{location: fallbackOpenAILocation, at: time.Now()}
}
func currentOpenAITimezone(ctx context.Context) openAITimezoneSnapshot {
	s := openAITimezoneFrom(ctx)
	s.at = time.Now()
	return s
}

func applyOpenAITimezone(ctx context.Context, payload map[string]any) {
	s := currentOpenAITimezone(ctx)
	payload["timezone"] = s.location.String()
	payload["timezone_offset_min"] = s.offset()
}

type openAITimezoneKey struct{}

var fallbackOpenAILocation = time.UTC

func (o *OpenAIImage) withTimezone(ctx context.Context, account wireAccount) context.Context {
	profile, err := accountBrowserProfile(account.Fields)
	if err != nil {
		return ctx
	}
	location, err := time.LoadLocation(profile.TimezoneIANA)
	if err != nil {
		location = time.UTC
	}
	return context.WithValue(ctx, openAITimezoneKey{}, openAITimezoneSnapshot{location: location, at: time.Now()})
}
