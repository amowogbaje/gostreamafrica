package catalog

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"streamafrica/api/internal/auth"
)

func (f *fx) cachedService(t *testing.T) *Service {
	t.Helper()
	// every test gets its own cache namespace, so tests (each with its own database schema) can
	// never see each other's cached pages
	return NewService(f.store, NewRedisCache(redisTestClient(t), "test-"+randHex(5)), quietLogger(), time.Now, Config{HomeTTL: time.Minute, GenresTTL: time.Minute})
}

func TestCache_HomeIsCachedUntilTitlesChange(t *testing.T) {
	f := newFx(t)
	svc := f.cachedService(t)
	ctx := context.Background()
	f.title("one", nil)
	f.title("two", func(s *tspec) { s.published = f.day(-1) })

	first, _, err := svc.HomeJSON(ctx, Viewer{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(first), `"one"`) || strings.Contains(string(first), `"three"`) {
		t.Fatalf("setup: %s", first)
	}

	// the database changes, but the cached page is served: that is the point of the cache
	f.title("three", func(s *tspec) { s.published = f.day(5) })
	second, _, _ := svc.HomeJSON(ctx, Viewer{})
	if string(second) != string(first) {
		t.Fatalf("expected the cached page, got a fresh one: %s", second)
	}

	// explicit invalidation makes the change visible immediately
	if err := svc.TitlesChanged(ctx); err != nil {
		t.Fatal(err)
	}
	third, _, _ := svc.HomeJSON(ctx, Viewer{})
	if !strings.Contains(string(third), `"three"`) {
		t.Fatalf("invalidation did not refresh the home page: %s", third)
	}

	// and it is cached again afterwards
	f.exec(`UPDATE titles SET status = 'unpublished' WHERE slug = 'three'`)
	if again, _, _ := svc.HomeJSON(ctx, Viewer{}); !strings.Contains(string(again), `"three"`) {
		t.Fatal("the refreshed page should be cached again")
	}
	_ = svc.TitlesChanged(ctx)
	if last, _, _ := svc.HomeJSON(ctx, Viewer{}); strings.Contains(string(last), `"three"`) {
		t.Fatal("an unpublished title must disappear after invalidation")
	}
}

func TestCache_GenresInvalidateWithTitles(t *testing.T) {
	f := newFx(t)
	svc := f.cachedService(t)
	ctx := context.Background()
	f.title("g1", func(s *tspec) { s.genres = []string{"drama"} })
	before, _ := svc.GenresJSON(ctx)
	if !strings.Contains(string(before), `"slug":"drama","name":"Drama","title_count":1`) {
		t.Fatalf("setup: %s", before)
	}
	f.title("g2", func(s *tspec) { s.genres = []string{"drama"} })
	if cached, _ := svc.GenresJSON(ctx); string(cached) != string(before) {
		t.Fatal("genres should be served from the cache")
	}
	_ = svc.TitlesChanged(ctx)
	after, _ := svc.GenresJSON(ctx)
	if !strings.Contains(string(after), `"slug":"drama","name":"Drama","title_count":2`) {
		t.Fatalf("genre counts not refreshed: %s", after)
	}
}

func TestCache_ContinueWatchingIsNeverCached(t *testing.T) {
	f := newFx(t)
	svc := f.cachedService(t)
	ctx := context.Background()
	id := f.title("film", nil)
	v := f.as(f.viewerA, auth.RoleViewer)
	body, _, _ := svc.HomeJSON(ctx, v) // warms the public part
	if strings.Contains(string(body), "continue-watching") {
		t.Fatal("nothing watched yet")
	}
	if _, err := svc.SaveProgress(ctx, v, ProgressUpdate{TitleID: id, PositionSeconds: 10, DurationSeconds: 100}); err != nil {
		t.Fatal(err)
	}
	body, _, _ = svc.HomeJSON(ctx, v) // the public part is cached, the personal row must still appear
	if !strings.Contains(string(body), `"key":"continue-watching"`) {
		t.Fatalf("a personal row must be live, not cached: %s", body)
	}
	// and it must not leak into the shared cached copy
	anon, _, _ := svc.HomeJSON(ctx, Viewer{})
	if strings.Contains(string(anon), "continue-watching") {
		t.Fatal("a personal row leaked into the shared cache")
	}
}

func TestRedisCache_EpochSemantics(t *testing.T) {
	c := NewRedisCache(redisTestClient(t), "test-"+randHex(5))
	ctx := context.Background()
	name := "t-" + randHex(4)

	if _, _, hit, err := c.Load(ctx, name); err != nil || hit {
		t.Fatalf("cold load: hit=%v err=%v", hit, err)
	}
	_, epoch, _, _ := c.Load(ctx, name)
	if err := c.Store(ctx, epoch, name, []byte(`{"a":1}`), time.Minute); err != nil {
		t.Fatal(err)
	}
	if v, e, hit, _ := c.Load(ctx, name); !hit || string(v) != `{"a":1}` || e != epoch {
		t.Fatalf("hit: %q epoch=%d hit=%v", v, e, hit)
	}

	// A slow reader loaded the epoch BEFORE an invalidation and tries to store AFTER it.
	// Its write must land under the old epoch and never be visible.
	_, staleEpoch, _, _ := c.Load(ctx, "other-"+name)
	if err := c.Invalidate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.Store(ctx, staleEpoch, "other-"+name, []byte("STALE"), time.Minute); err != nil {
		t.Fatal(err)
	}
	if v, _, hit, _ := c.Load(ctx, "other-"+name); hit {
		t.Fatalf("stale write became visible after invalidation: %q", v)
	}
	if _, _, hit, _ := c.Load(ctx, name); hit {
		t.Fatal("old entries must be unreachable after invalidation")
	}

	// TTL
	_, e2, _, _ := c.Load(ctx, "ttl-"+name)
	_ = c.Store(ctx, e2, "ttl-"+name, []byte("x"), 1*time.Second)
	time.Sleep(1300 * time.Millisecond)
	if _, _, hit, _ := c.Load(ctx, "ttl-"+name); hit {
		t.Fatal("entry outlived its TTL")
	}
}

type brokenCache struct{}

func (brokenCache) Load(context.Context, string) ([]byte, int64, bool, error) {
	return nil, 0, false, errors.New("redis down")
}
func (brokenCache) Store(context.Context, int64, string, []byte, time.Duration) error {
	return errors.New("redis down")
}
func (brokenCache) Invalidate(context.Context) error { return errors.New("redis down") }

func TestCache_OutageNeverFailsRequests(t *testing.T) {
	f := newFx(t)
	f.title("still-works", nil)
	svc := NewService(f.store, brokenCache{}, quietLogger(), time.Now, Config{})
	body, _, err := svc.HomeJSON(f.ctx, Viewer{})
	if err != nil || !strings.Contains(string(body), "still-works") {
		t.Fatalf("home with the cache down: %v %s", err, body)
	}
	if g, err := svc.GenresJSON(f.ctx); err != nil || len(g) == 0 {
		t.Fatalf("genres with the cache down: %v", err)
	}
	if err := svc.TitlesChanged(f.ctx); err == nil {
		t.Fatal("TitlesChanged must report a failed invalidation so callers can retry")
	}
}

func TestCache_ColdBurstHitsTheDatabaseOnce(t *testing.T) {
	f := newFx(t)
	f.title("burst", nil)
	var calls int
	var mu sync.Mutex
	counting := &countingStore{PGStore: f.store, onHome: func() { mu.Lock(); calls++; mu.Unlock() }}
	svc := NewService(counting, NewRedisCache(redisTestClient(t), "test-"+randHex(5)), quietLogger(), time.Now, Config{})
	_ = svc.TitlesChanged(f.ctx) // start from a cold epoch
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := svc.HomeJSON(f.ctx, Viewer{}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if calls != 1 {
		t.Fatalf("20 concurrent cold requests ran the home queries %d times, want 1", calls)
	}
}

type countingStore struct {
	*PGStore
	onHome func()
}

func (c *countingStore) HomePublic(ctx context.Context, since time.Time, perRow, langRows int) (homePublic, error) {
	c.onHome()
	time.Sleep(50 * time.Millisecond)
	return c.PGStore.HomePublic(ctx, since, perRow, langRows)
}
