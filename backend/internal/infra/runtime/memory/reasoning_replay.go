package memory

import (
	"container/list"
	"context"
	"sync"
	"time"
)

// reasoningReplayMaxBytes 是单实例回放缓存的总字节上限，与条目数上限同时生效。
const reasoningReplayMaxBytes = 128 << 20

type reasoningReplayEntry struct {
	key       string
	items     [][]byte
	size      int
	expiresAt time.Time
	storedAt  time.Time
}

// ReasoningReplayStore 提供单实例有界推理回放缓存。
type ReasoningReplayStore struct {
	mu         sync.Mutex
	maxSize    int
	evictBatch int
	maxBytes   int
	bytes      int
	values     map[string]*list.Element
	order      *list.List // 按最近访问排序，front 最旧
	ttlSlide   bool
}

// NewReasoningReplayStore 创建内存推理回放仓储；maxSize 为全局条目上限。
func NewReasoningReplayStore(maxSize int) *ReasoningReplayStore {
	if maxSize < 1 {
		maxSize = 10240
	}
	evictBatch := maxSize / 80
	if evictBatch < 1 {
		evictBatch = 1
	}
	if evictBatch > 128 {
		evictBatch = 128
	}
	return &ReasoningReplayStore{maxSize: maxSize, evictBatch: evictBatch, maxBytes: reasoningReplayMaxBytes, values: make(map[string]*list.Element, maxSize), order: list.New(), ttlSlide: true}
}

func reasoningReplayMapKey(model, sessionKey string) string {
	return model + "\x00" + sessionKey
}

func cloneReplayItems(items [][]byte) [][]byte {
	if len(items) == 0 {
		return nil
	}
	cloned := make([][]byte, 0, len(items))
	for _, item := range items {
		cloned = append(cloned, append([]byte(nil), item...))
	}
	return cloned
}

func (s *ReasoningReplayStore) Get(_ context.Context, model, sessionKey string, now time.Time, ttl time.Duration) ([][]byte, bool, error) {
	if model == "" || sessionKey == "" {
		return nil, false, nil
	}
	key := reasoningReplayMapKey(model, sessionKey)
	s.mu.Lock()
	defer s.mu.Unlock()
	elem, ok := s.values[key]
	if !ok {
		return nil, false, nil
	}
	entry := elem.Value.(*reasoningReplayEntry)
	if !now.Before(entry.expiresAt) {
		s.removeLocked(elem)
		return nil, false, nil
	}
	if s.ttlSlide {
		if ttl <= 0 {
			ttl = entry.expiresAt.Sub(entry.storedAt)
			if ttl <= 0 {
				ttl = time.Hour
			}
		}
		entry.expiresAt = now.Add(ttl)
		entry.storedAt = now
		s.order.MoveToBack(elem)
	}
	return cloneReplayItems(entry.items), true, nil
}

func (s *ReasoningReplayStore) Set(_ context.Context, model, sessionKey string, items [][]byte, expiresAt time.Time) error {
	if model == "" || sessionKey == "" || len(items) == 0 || expiresAt.IsZero() {
		return nil
	}
	key := reasoningReplayMapKey(model, sessionKey)
	now := time.Now()
	entry := &reasoningReplayEntry{key: key, items: cloneReplayItems(items), size: len(key), expiresAt: expiresAt, storedAt: now}
	for _, item := range entry.items {
		entry.size += len(item)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if elem, exists := s.values[key]; exists {
		s.removeLocked(elem)
	}
	s.evictLocked(now, entry.size)
	s.values[key] = s.order.PushBack(entry)
	s.bytes += entry.size
	return nil
}

func (s *ReasoningReplayStore) Delete(_ context.Context, model, sessionKey string) error {
	if model == "" || sessionKey == "" {
		return nil
	}
	key := reasoningReplayMapKey(model, sessionKey)
	s.mu.Lock()
	if elem, ok := s.values[key]; ok {
		s.removeLocked(elem)
	}
	s.mu.Unlock()
	return nil
}

// evictLocked 为即将写入的 incoming 字节腾出空间：先清过期项，再按条目数和字节上限淘汰最旧项。
// ponytail: 只从最旧端清过期项；TTL 调小后较新但先过期的条目要等轮到队首或被上限淘汰（Get 不会返回它们）。
func (s *ReasoningReplayStore) evictLocked(now time.Time, incoming int) {
	for elem := s.order.Front(); elem != nil && !now.Before(elem.Value.(*reasoningReplayEntry).expiresAt); elem = s.order.Front() {
		s.removeLocked(elem)
	}
	if len(s.values) >= s.maxSize {
		for range s.evictBatch {
			s.removeLocked(s.order.Front())
		}
	}
	for s.order.Len() > 0 && s.bytes+incoming > s.maxBytes {
		s.removeLocked(s.order.Front())
	}
}

func (s *ReasoningReplayStore) removeLocked(elem *list.Element) {
	entry := s.order.Remove(elem).(*reasoningReplayEntry)
	delete(s.values, entry.key)
	s.bytes -= entry.size
}
