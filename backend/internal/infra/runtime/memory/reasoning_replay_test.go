package memory

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestReasoningReplayStoreSetGetDelete(t *testing.T) {
	store := NewReasoningReplayStore(8)
	ctx := context.Background()
	now := time.Now().UTC()
	items := [][]byte{[]byte(`{"type":"reasoning","encrypted_content":"abcdefghijklmnopqrstuvwxyz"}`)}
	if err := store.Set(ctx, "grok", "sess", items, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, ok, err := store.Get(ctx, "grok", "sess", now, time.Hour)
	if err != nil || !ok || len(got) != 1 {
		t.Fatalf("get = %v ok=%v err=%v", got, ok, err)
	}
	if err := store.Delete(ctx, "grok", "sess"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.Get(ctx, "grok", "sess", now, time.Hour); err != nil || ok {
		t.Fatalf("after delete ok=%v err=%v", ok, err)
	}
}

func TestReasoningReplayStoreEvictsOldest(t *testing.T) {
	store := NewReasoningReplayStore(2)
	ctx := context.Background()
	now := time.Now().UTC()
	_ = store.Set(ctx, "m", "a", [][]byte{[]byte("1")}, now.Add(time.Hour))
	time.Sleep(2 * time.Millisecond)
	_ = store.Set(ctx, "m", "b", [][]byte{[]byte("2")}, now.Add(time.Hour))
	time.Sleep(2 * time.Millisecond)
	_ = store.Set(ctx, "m", "c", [][]byte{[]byte("3")}, now.Add(time.Hour))
	if _, ok, _ := store.Get(ctx, "m", "a", now, time.Hour); ok {
		t.Fatal("oldest entry should be evicted")
	}
	if _, ok, _ := store.Get(ctx, "m", "c", now, time.Hour); !ok {
		t.Fatal("newest entry should remain")
	}
}

func TestReasoningReplayStoreHitRestoresConfiguredTTL(t *testing.T) {
	store := NewReasoningReplayStore(8)
	ctx := context.Background()
	storedAt := time.Now()
	if err := store.Set(ctx, "grok", "sess", [][]byte{[]byte("state")}, storedAt.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.Get(ctx, "grok", "sess", storedAt.Add(30*time.Minute), 2*time.Hour); err != nil || !ok {
		t.Fatalf("first get ok=%v err=%v", ok, err)
	}
	if _, ok, err := store.Get(ctx, "grok", "sess", storedAt.Add(90*time.Minute), 2*time.Hour); err != nil || !ok {
		t.Fatalf("sliding ttl was not restored: ok=%v err=%v", ok, err)
	}
}

func TestReasoningReplayStoreStaysUnderByteBudget(t *testing.T) {
	store := NewReasoningReplayStore(10240)
	store.maxBytes = 1 << 20
	ctx := context.Background()
	payload := make([]byte, 64<<10)
	expires := time.Now().Add(time.Hour)
	for i := range 64 {
		_ = store.Set(ctx, "m", fmt.Sprintf("s-%d", i), [][]byte{payload}, expires)
		if store.bytes > store.maxBytes {
			t.Fatalf("after %d inserts bytes=%d exceeds budget %d", i+1, store.bytes, store.maxBytes)
		}
	}
	if _, ok, _ := store.Get(ctx, "m", "s-0", time.Now(), time.Hour); ok {
		t.Fatal("oldest entry should be evicted to stay under the byte budget")
	}
	if _, ok, _ := store.Get(ctx, "m", "s-63", time.Now(), time.Hour); !ok {
		t.Fatal("newest entry should remain")
	}
}

func TestReasoningReplayStoreEvictsExpiredEntries(t *testing.T) {
	store := NewReasoningReplayStore(8)
	ctx := context.Background()
	now := time.Now()
	_ = store.Set(ctx, "m", "expired", [][]byte{[]byte("1")}, now.Add(-time.Second))
	_ = store.Set(ctx, "m", "live", [][]byte{make([]byte, 1000)}, now.Add(time.Hour))
	_ = store.Set(ctx, "m", "live", [][]byte{[]byte("replaced")}, now.Add(time.Hour))
	if _, ok := store.values[reasoningReplayMapKey("m", "expired")]; ok {
		t.Fatal("expired entry should be evicted by the next insert")
	}
	if _, ok, _ := store.Get(ctx, "m", "live", now.Add(2*time.Hour), time.Hour); ok {
		t.Fatal("entry past its expiry should not be returned")
	}
	if len(store.values) != 0 || store.order.Len() != 0 || store.bytes != 0 {
		t.Fatalf("store should be empty: entries=%d list=%d bytes=%d", len(store.values), store.order.Len(), store.bytes)
	}
}

func TestReasoningReplayStoreGetRefreshesEvictionOrder(t *testing.T) {
	store := NewReasoningReplayStore(2)
	ctx := context.Background()
	expires := time.Now().Add(time.Hour)
	_ = store.Set(ctx, "m", "a", [][]byte{[]byte("1")}, expires)
	time.Sleep(2 * time.Millisecond)
	_ = store.Set(ctx, "m", "b", [][]byte{[]byte("2")}, expires)
	time.Sleep(2 * time.Millisecond)
	if _, ok, _ := store.Get(ctx, "m", "a", time.Now(), time.Hour); !ok {
		t.Fatal("a should be cached")
	}
	time.Sleep(2 * time.Millisecond)
	_ = store.Set(ctx, "m", "c", [][]byte{[]byte("3")}, expires)
	if _, ok, _ := store.Get(ctx, "m", "b", time.Now(), time.Hour); ok {
		t.Fatal("least recently used entry should be evicted at the count cap")
	}
	for _, key := range []string{"a", "c"} {
		if _, ok, _ := store.Get(ctx, "m", key, time.Now(), time.Hour); !ok {
			t.Fatalf("%s should remain", key)
		}
	}
}
