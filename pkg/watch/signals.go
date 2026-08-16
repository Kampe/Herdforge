package watch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// ErrOutsideRoot is returned when a signal path is not contained by the
// repository root. A watcher must never turn a task-provided path into a host
// filesystem watcher.
var ErrOutsideRoot = errors.New("watch: signal path is outside repository root")

// SignalEvent is a coalesced notification for one or more durable signal
// changes. Fallback is true when the notification backend was lost or could
// not be trusted; callers should run their normal reconciliation in that case.
type SignalEvent struct {
	Paths    []string
	Fallback bool
}

// SignalWatcherOptions controls debounce and reconciliation fallback.
type SignalWatcherOptions struct {
	Debounce     time.Duration
	PollInterval time.Duration
	Reconcile    func(context.Context) error
	NewBackend   func() (signalBackend, error)
}

type signalBackend interface {
	Add(string) error
	Close() error
	Events() <-chan fsnotify.Event
	Errors() <-chan error
}

type fsnotifyBackend struct{ *fsnotify.Watcher }

func (b fsnotifyBackend) Events() <-chan fsnotify.Event { return b.Watcher.Events }
func (b fsnotifyBackend) Errors() <-chan error          { return b.Watcher.Errors }

// SignalWatcher watches only the explicitly named repo-local signal paths.
// It is deliberately a wake-up hint, not a source of truth: every fallback
// and every coalesced event is intended to cause the coordinator's existing
// reconciliation path to inspect durable state.
type SignalWatcher struct {
	root       string
	paths      []string
	debounce   time.Duration
	poll       time.Duration
	reconcile  func(context.Context) error
	newBackend func() (signalBackend, error)

	mu      sync.Mutex
	started bool
	backend signalBackend
	wake    chan SignalEvent
	closed  chan struct{}
}

// NewSignalWatcher validates all paths before opening any OS watcher. Paths
// must be relative to root; duplicate logical paths are removed deterministically.
func NewSignalWatcher(root string, paths []string, opts SignalWatcherOptions) (*SignalWatcher, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("watch: repository root is required")
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("watch: resolve repository root: %w", err)
	}
	rootInfo, err := os.Stat(absRoot)
	if err != nil {
		return nil, fmt.Errorf("watch: repository root: %w", err)
	}
	if !rootInfo.IsDir() {
		return nil, errors.New("watch: repository root is not a directory")
	}
	resolvedRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return nil, fmt.Errorf("watch: resolve repository root symlinks: %w", err)
	}

	clean := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, raw := range paths {
		if raw == "" || filepath.IsAbs(raw) {
			return nil, ErrOutsideRoot
		}
		candidate := filepath.Clean(raw)
		if candidate == "." || candidate == ".." || strings.HasPrefix(candidate, ".."+string(filepath.Separator)) {
			return nil, ErrOutsideRoot
		}
		full := filepath.Join(absRoot, candidate)
		if !pathWithin(absRoot, full) {
			return nil, ErrOutsideRoot
		}
		if resolved, ok := resolveExistingPath(full); ok && !pathWithin(resolvedRoot, resolved) {
			return nil, ErrOutsideRoot
		}
		if _, ok := seen[candidate]; !ok {
			seen[candidate] = struct{}{}
			clean = append(clean, candidate)
		}
	}
	if len(clean) == 0 {
		return nil, errors.New("watch: at least one signal path is required")
	}
	debounce := opts.Debounce
	if debounce <= 0 {
		debounce = 50 * time.Millisecond
	}
	poll := opts.PollInterval
	if poll <= 0 {
		poll = 5 * time.Second
	}
	newBackend := opts.NewBackend
	if newBackend == nil {
		newBackend = func() (signalBackend, error) {
			w, err := fsnotify.NewWatcher()
			if err != nil {
				return nil, err
			}
			return fsnotifyBackend{Watcher: w}, nil
		}
	}
	return &SignalWatcher{root: resolvedRoot, paths: clean, debounce: debounce, poll: poll,
		reconcile: opts.Reconcile, newBackend: newBackend, wake: make(chan SignalEvent, 1), closed: make(chan struct{})}, nil
}

func resolveExistingPath(path string) (string, bool) {
	for current := path; ; current = filepath.Dir(current) {
		if _, err := os.Lstat(current); err == nil {
			resolved, err := filepath.EvalSymlinks(current)
			return resolved, err == nil
		} else if !os.IsNotExist(err) || filepath.Dir(current) == current {
			return "", false
		}
	}
}

func pathWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// Events returns a coalescing wake channel. A full channel intentionally drops
// duplicate wakes: reconciliation reads the durable state, so another hint is
// not additional information.
func (w *SignalWatcher) Events() <-chan SignalEvent { return w.wake }

// Start runs until ctx is cancelled. It returns ctx.Err() on normal shutdown.
// Calling Start more than once is rejected, preventing duplicate OS watchers.
func (w *SignalWatcher) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	w.mu.Lock()
	if w.started {
		w.mu.Unlock()
		return errors.New("watch: watcher already started")
	}
	w.started = true
	w.mu.Unlock()

	b, err := w.newBackend()
	if err != nil {
		w.fallback(ctx)
		return w.fallbackLoop(ctx, nil)
	}
	w.mu.Lock()
	w.backend = b
	w.mu.Unlock()
	defer func() {
		_ = b.Close()
		close(w.closed)
	}()
	seenTargets := make(map[string]struct{}, len(w.paths))
	for _, path := range w.paths {
		target := w.watchTarget(path)
		if _, seen := seenTargets[target]; seen {
			continue
		}
		seenTargets[target] = struct{}{}
		if err := b.Add(target); err != nil {
			w.fallback(ctx)
			return w.fallbackLoop(ctx, b)
		}
	}

	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	var pending []string
	flush := func(fallback bool) {
		e := SignalEvent{Paths: append([]string(nil), pending...), Fallback: fallback}
		pending = nil
		select {
		case w.wake <- e:
		default:
		}
	}
	for {
		var timerC <-chan time.Time
		if len(pending) > 0 {
			timerC = timer.C
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-b.Events():
			if !ok {
				w.fallback(ctx)
				return w.fallbackLoop(ctx, b)
			}
			if w.relevant(ev.Name) && ev.Op&fsnotify.Chmod != 0 {
				w.fallback(ctx)
				return w.fallbackLoop(ctx, b)
			}
			if w.relevant(ev.Name) && ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Remove|fsnotify.Rename) != 0 {
				pending = appendUnique(pending, w.relative(ev.Name))
				if !timer.Stop() && timerC != nil {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(w.debounce)
			}
		case _, ok := <-b.Errors():
			if !ok {
				w.fallback(ctx)
				return w.fallbackLoop(ctx, b)
			}
			w.fallback(ctx)
			if len(pending) > 0 {
				flush(true)
			}
			return w.fallbackLoop(ctx, b)
		case <-timerC:
			flush(false)
		}
	}
}

func (w *SignalWatcher) fallback(ctx context.Context) {
	if w.reconcile != nil {
		_ = w.reconcile(ctx)
	}
	select {
	case w.wake <- SignalEvent{Fallback: true}:
	default:
	}
}

func (w *SignalWatcher) fallbackLoop(ctx context.Context, b signalBackend) error {
	t := time.NewTicker(w.poll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if w.reconcile != nil {
				_ = w.reconcile(ctx)
			}
			select {
			case w.wake <- SignalEvent{Fallback: true}:
			default:
			}
		}
	}
}

func (w *SignalWatcher) watchTarget(path string) string {
	full := filepath.Join(w.root, path)
	if info, err := os.Stat(full); err == nil && info.IsDir() {
		return full
	}
	for current := filepath.Dir(full); ; current = filepath.Dir(current) {
		if info, err := os.Stat(current); err == nil && info.IsDir() {
			return current
		}
		if current == w.root || filepath.Dir(current) == current {
			return w.root
		}
	}
}

func (w *SignalWatcher) relevant(name string) bool {
	if !pathWithin(w.root, name) {
		return false
	}
	for _, path := range w.paths {
		target := filepath.Join(w.root, path)
		if name == target || pathWithin(target, name) {
			return true
		}
	}
	return false
}

func (w *SignalWatcher) relative(name string) string {
	rel, err := filepath.Rel(w.root, name)
	if err != nil {
		return name
	}
	return rel
}

func appendUnique(paths []string, path string) []string {
	for _, existing := range paths {
		if existing == path {
			return paths
		}
	}
	return append(paths, path)
}
