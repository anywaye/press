package builds


import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anywaye/press/internal/store"
)

type claimStoreFake struct {
	mu       sync.Mutex
	rows     map[string]Build
	order    []string
	receipts map[string]ReceiptSource
}

func newClaimStoreFake() *claimStoreFake {
	return &claimStoreFake{rows: map[string]Build{}, receipts: map[string]ReceiptSource{}}
}

func (f *claimStoreFake) Insert(_ context.Context, b Build) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.rows[b.ID]; exists {
		return errors.New("builds: build id already exists")
	}
	f.rows[b.ID] = b
	f.order = append(f.order, b.ID)
	return nil
}

func (f *claimStoreFake) Get(_ context.Context, id string) (Build, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.rows[id]
	if !ok {
		return Build{}, errors.New("builds: no such build")
	}
	return b, nil
}

func (f *claimStoreFake) Transition(_ context.Context, id string, next State) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.rows[id]
	if !ok {
		return errors.New("builds: no such build")
	}
	if err := b.Transition(next); err != nil {
		return err
	}
	f.rows[id] = b
	return nil
}

func (f *claimStoreFake) Claim(_ context.Context) (Claimed, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range f.order {
		b := f.rows[id]
		if b.State != StateQueued {
			continue
		}
		b.State = StateRunning
		f.rows[id] = b
		return Claimed{Build: b, Receipt: f.receipts[b.ReceiptID], Found: true}, nil
	}
	return Claimed{}, nil
}

type countingRunner struct {
	calls atomic.Int64
	fail  error
}

func (r *countingRunner) Build(_ context.Context, engineDir, _ string) error {
	r.calls.Add(1)
	if r.fail != nil {
		return r.fail
	}
	for _, kind := range []string{"docs", "notes"} {
		p := filepath.Join(engineDir, "dist", kind)
		if err := os.MkdirAll(p, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(p, "index.html"), []byte("<html>built</html>"), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func newClaimLoopTest(t *testing.T, runner *countingRunner) (*Loop, *claimStoreFake) {
	t.Helper()
	recs := newClaimStoreFake()
	b, err := New("b1", "acme", store.KindDocs, "r1", time.Unix(0, 0))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := recs.Insert(context.Background(), b); err != nil {
		t.Fatalf("insert: %v", err)
	}
	key := "tenants/acme/ingest/r1.tar.gz"
	recs.receipts["r1"] = ReceiptSource{ID: "r1", Tenant: "acme", Kind: store.KindDocs, ObjectKey: key}
	dir := t.TempDir()
	w := NewWorker(
		fakeStoreReader{objects: map[string][]byte{key: tarball(t, "index.md", "hello")}},
		TarExtractor{}, &fakeEngine{dir: dir}, runner, &fakeUploader{}, recs)
	l := &Loop{Claims: recs, Work: w, EngineRef: "press-engine-v1", Interval: time.Millisecond}
	return l, recs
}

func TestOnceClaimsTheQueuedBuildAndRecordsSucceeded(t *testing.T) {
	l, recs := newClaimLoopTest(t, &countingRunner{})
	claimed, err := l.Once(context.Background())
	if err != nil {
		t.Fatalf("once: %v", err)
	}
	if !claimed {
		t.Fatal("Once reported no build with one queued")
	}
	stored, _ := recs.Get(context.Background(), "b1")
	if stored.State != StateSucceeded {
		t.Fatalf("stored state = %q, want succeeded", stored.State)
	}
	claimed, err = l.Once(context.Background())
	if err != nil || claimed {
		t.Fatalf("second Once = (%v, %v); want (false, nil)", claimed, err)
	}
}

func TestOnceAFailedRunRecordsFailed(t *testing.T) {
	l, recs := newClaimLoopTest(t, &countingRunner{fail: errors.New("npm ci failed")})
	claimed, err := l.Once(context.Background())
	if err == nil || !claimed {
		t.Fatalf("Once = (%v, %v); want (true, the build reason)", claimed, err)
	}
	stored, _ := recs.Get(context.Background(), "b1")
	if stored.State != StateFailed {
		t.Fatalf("stored state = %q, want failed", stored.State)
	}
}

func TestOnceAnEmptyQueueIsANoop(t *testing.T) {
	runner := &countingRunner{}
	recs := newClaimStoreFake()
	w := NewWorker(fakeStoreReader{}, TarExtractor{}, &fakeEngine{}, runner, &fakeUploader{}, recs)
	l := &Loop{Claims: recs, Work: w, EngineRef: "ref"}
	claimed, err := l.Once(context.Background())
	if err != nil || claimed {
		t.Fatalf("Once = (%v, %v); want (false, nil)", claimed, err)
	}
	if n := runner.calls.Load(); n != 0 {
		t.Fatalf("the runner ran %d times on an empty queue", n)
	}
}

func TestConcurrentClaimsHandABuildOutOnce(t *testing.T) {
	l, _ := newClaimLoopTest(t, &countingRunner{})
	const racers = 16
	var won atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := l.Claims.Claim(context.Background())
			if err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			if c.Found {
				won.Add(1)
				if c.Build.ID != "b1" || c.Build.State != StateRunning {
					t.Errorf("claimed build = %+v; want b1 running", c.Build)
				}
			}
		}()
	}
	wg.Wait()
	if n := won.Load(); n != 1 {
		t.Fatalf("%d claimers won the build, want exactly 1", n)
	}
}

func TestTwoLoopsRacingBuildTheBuildOnce(t *testing.T) {
	recs := newClaimStoreFake()
	b, _ := New("b1", "acme", store.KindDocs, "r1", time.Unix(0, 0))
	_ = recs.Insert(context.Background(), b)
	key := "tenants/acme/ingest/r1.tar.gz"
	recs.receipts["r1"] = ReceiptSource{ID: "r1", Tenant: "acme", Kind: store.KindDocs, ObjectKey: key}

	runner := &countingRunner{}
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]bool, 2)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w := NewWorker(
				fakeStoreReader{objects: map[string][]byte{key: tarball(t, "index.md", "hello")}},
				TarExtractor{}, &fakeEngine{dir: t.TempDir()}, runner, &fakeUploader{}, recs)
			l := &Loop{Claims: recs, Work: w, EngineRef: "ref"}
			<-start
			claimed, err := l.Once(context.Background())
			if err != nil {
				t.Errorf("loop %d: %v", i, err)
				return
			}
			results[i] = claimed
		}(i)
	}
	close(start)
	wg.Wait()
	if n := runner.calls.Load(); n != 1 {
		t.Fatalf("the build ran %d times, want exactly 1", n)
	}
	if !(results[0] != results[1]) {
		t.Fatalf("both loops reported claimed=%v; exactly one must win", results[0])
	}
	stored, _ := recs.Get(context.Background(), "b1")
	if stored.State != StateSucceeded {
		t.Fatalf("stored state = %q, want succeeded", stored.State)
	}
}

func TestRunClaimedRefusesABuildTheLoopNeverHandedOver(t *testing.T) {
	w, recs, _, _, _ := newWorkerTest(t)
	b, _ := New("b1", "acme", store.KindDocs, "r1", time.Unix(0, 0))
	_ = recs.Insert(context.Background(), b)
	receipt := ReceiptSource{ID: "r1", Tenant: "acme", Kind: store.KindDocs, ObjectKey: "k"}
	if _, err := w.RunClaimed(context.Background(), b, receipt, "ref"); err == nil {
		t.Fatal("a build that was never claimed ran anyway")
	}
	if stored := recs.rows["b1"]; stored.State != StateQueued {
		t.Fatalf("stored state = %q after a refused run", stored.State)
	}
}

