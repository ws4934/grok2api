package console

import (
	"strings"
	"testing"
)

func TestConsoleImportNamePrefersEmailUnlessNamed(t *testing.T) {
	for _, tc := range []struct{ name, json, want string }{
		{"email", `{"sso_token":"test-sso","email":" owner@example.test "}`, "owner@example.test"},
		{"blank-name", `{"sso_token":"test-sso","email":"owner@example.test","name":" "}`, "owner@example.test"},
		{"custom", `{"sso_token":"test-sso","email":"owner@example.test","name":" Production "}`, "Production"},
		{"anonymous", `{"sso_token":"test-sso"}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seeds, err := parseImportedCredentials([]byte(tc.json))
			if err != nil || len(seeds) != 1 {
				t.Fatalf("seeds=%+v err=%v", seeds, err)
			}
			if tc.want == "" {
				if !strings.HasPrefix(seeds[0].Name, "Grok Console ") {
					t.Fatalf("anonymous name=%q", seeds[0].Name)
				}
			} else if seeds[0].Name != tc.want {
				t.Fatalf("name=%q want=%q", seeds[0].Name, tc.want)
			}
		})
	}
}
