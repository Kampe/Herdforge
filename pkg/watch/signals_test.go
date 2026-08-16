package watch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

type fakeSignalBackend struct {
	events      chan fsnotify.Event
	errors      chan error
	added       []string
	addedSignal chan struct{}
	closed      bool
}

func (b *fakeSignalBackend) Add(path string) error {
	b.added = append(b.added, path)
	if b.addedSignal != nil {
		select {
		case b.addedSignal <- struct{}{}:
		default:
		}
	}
	return nil
}
func (b *fakeSignalBackend) Close() error                  { b.closed = true; return nil }
func (b *fakeSignalBackend) Events() <-chan fsnotify.Event { return b.events }
func (b *fakeSignalBackend) Errors() <-chan error          { return b.errors }

func newFakeWatcher(t *testing.T, backend *fakeSignalBackend, opts SignalWatcherOptions) *SignalWatcher {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".herd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if opts.NewBackend == nil {
		opts.NewBackend = func() (signalBackend, error) { return backend, nil }
	}
	w, err := NewSignalWatcher(root, []string{".herd/signals"}, opts)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestSignalWatcherEventDebounceAndPathFiltering(t *testing.T) {
	b := &fakeSignalBackend{events: make(chan fsnotify.Event, 8), errors: make(chan error, 1)}
	w := newFakeWatcher(t, b, SignalWatcherOptions{Debounce: 10 * time.Millisecond, PollInterval: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- w.Start(ctx) }()

	outside := filepath.Join(t.TempDir(), "other")
	b.events <- fsnotify.Event{Name: outside, Op: fsnotify.Write}
	b.events <- fsnotify.Event{Name: filepath.Join(t.TempDir(), "not-our-signal"), Op: fsnotify.Write}
	path := filepath.Join(w.root, ".herd", "signals", "one")
	b.events <- fsnotify.Event{Name: path, Op: fsnotify.Write}
	b.events <- fsnotify.Event{Name: path, Op: fsnotify.Write}
	select {
	case event := <-w.Events():
		if event.Fallback || len(event.Paths) != 1 || event.Paths[0] != filepath.Join(".herd", "signals", "one") {
			t.Fatalf("unexpected event: %+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for debounced event")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Start: %v", err)
	}
	if len(b.added) != 1 || !filepath.IsAbs(b.added[0]) {
		t.Fatalf("unexpected watched targets: %v", b.added)
	}
}

func TestSignalWatcherRejectsHostAndTraversalPaths(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{"/tmp/host", "../outside", "."} {
		if _, err := NewSignalWatcher(root, []string{path}, SignalWatcherOptions{}); !errors.Is(err, ErrOutsideRoot) {
			t.Errorf("path %q error = %v, want ErrOutsideRoot", path, err)
		}
	}
	host := filepath.Join(t.TempDir(), "host")
	if err := os.WriteFile(host, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(host, link); err != nil {
		t.Fatal(err)
	}
	if _, err := NewSignalWatcher(root, []string{"link"}, SignalWatcherOptions{}); !errors.Is(err, ErrOutsideRoot) {
		t.Fatalf("symlink outside root error = %v, want ErrOutsideRoot", err)
	}
}

func TestSignalWatcherRejectsDuplicateStart(t *testing.T) {
	b := &fakeSignalBackend{events: make(chan fsnotify.Event), errors: make(chan error), addedSignal: make(chan struct{}, 1)}
	w := newFakeWatcher(t, b, SignalWatcherOptions{PollInterval: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- w.Start(ctx) }()
	select {
	case <-b.addedSignal:
	case <-time.After(time.Second):
		t.Fatal("watcher did not initialize")
	}
	if err := w.Start(ctx); err == nil {
		t.Fatal("second Start must fail")
	}
	cancel()
	<-done
}

func TestSignalWatcherLossRunsReconciliationFallback(t *testing.T) {
	b := &fakeSignalBackend{events: make(chan fsnotify.Event), errors: make(chan error, 1)}
	reconciled := make(chan struct{}, 2)
	w := newFakeWatcher(t, b, SignalWatcherOptions{
		PollInterval: 10 * time.Millisecond,
		Reconcile:    func(context.Context) error { reconciled <- struct{}{}; return nil },
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- w.Start(ctx) }()
	close(b.events)
	select {
	case event := <-w.Events():
		if !event.Fallback {
			t.Fatalf("loss event = %+v, want fallback", event)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for fallback wake")
	}
	select {
	case <-reconciled:
	case <-time.After(time.Second):
		t.Fatal("watcher loss did not reconcile")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Start: %v", err)
	}
}

func TestSignalWatcherBackendUnavailableUsesPollingFallback(t *testing.T) {
	reconciled := make(chan struct{}, 2)
	w, err := NewSignalWatcher(t.TempDir(), []string{".herd/signals"}, SignalWatcherOptions{
		PollInterval: 10 * time.Millisecond,
		NewBackend:   func() (signalBackend, error) { return nil, errors.New("notifications unavailable") },
		Reconcile:    func(context.Context) error { reconciled <- struct{}{}; return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- w.Start(ctx) }()
	select {
	case event := <-w.Events():
		if !event.Fallback {
			t.Fatalf("unavailable event = %+v, want fallback", event)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for unavailable fallback")
	}
	select {
	case <-reconciled:
	case <-time.After(time.Second):
		t.Fatal("unavailable backend did not reconcile")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Start: %v", err)
	}
}
