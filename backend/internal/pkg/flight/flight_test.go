package flight

import (
	"context"
	"strings"
	"testing"
	"time"

	"golang.org/x/sync/singleflight"
)

func TestDoReportsAPanickingLoadAsAnError(t *testing.T) {
	var group singleflight.Group
	_, err := Do(context.Background(), &group, "key", time.Second, func(context.Context) (int, error) {
		panic("boom")
	})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
}
