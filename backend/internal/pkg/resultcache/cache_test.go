package resultcache

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCacheExpiresAndEvictsOldestEntry(t *testing.T) {
	now := time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC)
	cache := New[string, int](2, time.Minute)
	cache.Set("first", 1, now)
	cache.Set("second", 2, now.Add(time.Second))
	cache.Set("third", 3, now.Add(2*time.Second))

	if _, ok := cache.Get("first", now.Add(3*time.Second)); ok {
		t.Fatal("oldest cache entry was not evicted")
	}
	if value, ok := cache.Get("third", now.Add(3*time.Second)); !ok || value != 3 {
		t.Fatalf("third entry = %d, %v", value, ok)
	}
	if _, ok := cache.Get("second", now.Add(2*time.Minute)); ok {
		t.Fatal("expired cache entry was returned")
	}
}

func TestCacheDeleteInvalidatesStoredValue(t *testing.T) {
	now := time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC)
	cache := New[string, int](2, time.Minute)
	cache.Set("account-risk", 1, now)
	cache.Delete("account-risk")

	if _, ok := cache.Get("account-risk", now); ok {
		t.Fatal("deleted cache entry was returned")
	}
}

func TestCacheCoalescesConcurrentLoads(t *testing.T) {
	now := time.Now()
	cache := New[string, int](2, time.Minute)
	var calls atomic.Int32
	start := make(chan struct{})
	var wait sync.WaitGroup
	results := make([]int, 2)
	wait.Add(2)
	for index := range results {
		go func() {
			defer wait.Done()
			<-start
			value, err := cache.Load(context.Background(), "shared", now, func() (int, error) {
				calls.Add(1)
				time.Sleep(10 * time.Millisecond)
				return 42, nil
			})
			if err != nil {
				t.Errorf("load: %v", err)
			}
			results[index] = value
		}()
	}
	close(start)
	wait.Wait()
	if calls.Load() != 1 || results[0] != 42 || results[1] != 42 {
		t.Fatalf("calls = %d, results = %#v", calls.Load(), results)
	}
}

// The leader's cancellation must not fail a waiter whose own context is live.
func TestCacheWaiterLoadsAgainWhenTheLeaderIsCanceled(t *testing.T) {
	cache := New[string, int](4, time.Second)
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	loading := make(chan struct{})
	leaderDone := make(chan error, 1)
	go func() {
		_, err := cache.Load(leaderCtx, "k", time.Now(), func() (int, error) {
			close(loading)
			<-leaderCtx.Done()
			return 0, leaderCtx.Err()
		})
		leaderDone <- err
	}()
	<-loading
	waiterDone := make(chan error, 1)
	var value int
	go func() {
		var err error
		value, err = cache.Load(context.Background(), "k", time.Now(), func() (int, error) { return 42, nil })
		waiterDone <- err
	}()
	time.Sleep(10 * time.Millisecond) // the waiter joins the in-flight load
	cancelLeader()
	if err := <-leaderDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader err = %v", err)
	}
	if err := <-waiterDone; err != nil || value != 42 {
		t.Fatalf("waiter = %d, %v", value, err)
	}
}

func TestCacheWaiterHonorsCancellation(t *testing.T) {
	cache := New[string, int](2, time.Minute)
	loaderStarted := make(chan struct{})
	releaseLoader := make(chan struct{})
	loaderDone := make(chan struct{})
	go func() {
		defer close(loaderDone)
		_, _ = cache.Load(context.Background(), "shared", time.Now(), func() (int, error) {
			close(loaderStarted)
			<-releaseLoader
			return 42, nil
		})
	}()
	<-loaderStarted

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := cache.Load(ctx, "shared", time.Now(), func() (int, error) {
		t.Fatal("canceled waiter unexpectedly became the loader")
		return 0, nil
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("load error = %v, want context canceled", err)
	}

	close(releaseLoader)
	<-loaderDone
}
