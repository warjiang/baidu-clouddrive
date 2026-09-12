package cli

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/spf13/cobra"
)

type operation struct {
	source    fileEntry
	dest      location
	localBase string
	remove    bool
}

func runFiles(cmd *cobra.Command, cfg *config, o *fileOptions, name string, args []string) error {
	source, err := parseLocation(args[0])
	if err != nil {
		return err
	}
	var dest location
	if name != "rm" {
		dest, err = parseLocation(args[1])
		if err != nil {
			return err
		}
		if !source.remote && !dest.remote {
			return usage("local-to-local operations are unsupported")
		}
	} else if !source.remote {
		return usage("rm requires a bd:// URI")
	}
	if source.stream || dest.stream {
		if name != "cp" || o.recursive {
			return usage("stdin/stdout are supported only by non-recursive cp")
		}
		if cmd.Flags().Changed("expected-size") && !source.stream {
			return usage("--expected-size requires stdin as source")
		}
		return runStream(cmd, cfg, o, source, dest)
	}
	if cmd.Flags().Changed("expected-size") {
		return usage("--expected-size requires stdin as source")
	}
	if source.remote && dest.remote && source.name == dest.name {
		return usage("source and destination must not be identical")
	}
	if !source.remote && !o.followLinks {
		info, err := os.Lstat(source.name)
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return withCode(2, errors.New("source symlink not followed"))
		}
	}
	src, err := cfg.stat(cmd.Context(), source, o.pageSize)
	if err != nil {
		return err
	}
	if src.dir && source.remote && dest.remote && overlaps(source.name, dest.name, "/") {
		return usage("recursive source and destination must not overlap")
	}
	if src.dir && !o.recursive {
		return usage("source is a directory; use --recursive")
	}
	if source.slash && !src.dir {
		return usage("a source ending in / must be a directory")
	}
	if name == "sync" && !src.dir {
		return usage("sync requires a source directory")
	}
	if o.recursive && !src.dir {
		return usage("--recursive requires a source directory")
	}
	files, skipped, err := cfg.scan(cmd.Context(), source, o.recursive, o.followLinks, name == "mv", o.pageSize)
	if err != nil {
		return err
	}
	var plan []operation
	if name == "rm" {
		for _, f := range files {
			if !f.dir && included(o.filters, f.rel) {
				plan = append(plan, operation{source: f, remove: true})
			}
		}
		return executePlan(cmd, cfg, o, name, plan, nil, skipped)
	}
	dst, statErr := cfg.stat(cmd.Context(), dest, o.pageSize)
	exists := statErr == nil
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	if exists && !dst.dir && (src.dir || dest.slash) {
		return usage("destination is not a directory")
	}
	dirDest := src.dir || dest.slash || (exists && dst.dir)
	localBase := ""
	if !dest.remote {
		localBase = filepath.Dir(dest.name)
		if dirDest {
			localBase = dest.name
		}
	}
	destFiles := map[string]fileEntry{}
	if name == "sync" && exists {
		scanned, destSkipped, err := cfg.scan(cmd.Context(), dest, true, false, false, o.pageSize)
		if err != nil {
			return err
		}
		skipped = append(skipped, destSkipped...)
		for _, f := range scanned {
			destFiles[f.rel] = f
		}
	}
	sourceNames := map[string]bool{}
	caseNames := map[string]string{}
	serialTargets := false
	for _, f := range files {
		if f.dir {
			continue
		}
		sourceNames[f.rel] = true
		if !included(o.filters, f.rel) {
			continue
		}
		destRel := f.rel
		if name == "sync" && !dest.remote && exists {
			if _, exact := destFiles[destRel]; !exact {
				destRel, err = localSyncName(dest, destRel)
				if err != nil {
					return err
				}
				sourceNames[destRel] = true
			}
		}
		target := dest
		if dirDest {
			target, err = dest.join(f.rel)
			if err != nil {
				return err
			}
		}
		if source.remote && target.remote && f.loc.name == target.name {
			return usage("source and destination are the same file")
		}
		var current fileEntry
		var targetExists bool
		if name == "sync" {
			current, targetExists = destFiles[destRel]
		} else {
			current, err = cfg.stat(cmd.Context(), target, o.pageSize)
			targetExists = err == nil
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		if targetExists && current.dir {
			return fmt.Errorf("file destination is a directory: %s", target.String())
		}
		if o.noOverwrite && targetExists {
			skipped = append(skipped, target.String()+": destination exists")
			continue
		}
		if name == "sync" && targetExists && !needsSync(f, current, o) {
			continue
		}
		if !target.remote {
			if err := checkLocalTarget(localBase, target.name); err != nil {
				return err
			}
			if o.caseConflict != "ignore" || o.concurrency > 1 {
				conflict, err := caseCollision(target.name, caseNames, o.caseConflict == "skip")
				if err != nil {
					return err
				}
				if conflict != "" {
					message := fmt.Sprintf("case collision: %s and %s", target.name, conflict)
					switch o.caseConflict {
					case "error":
						return errors.New(message)
					case "skip":
						skipped = append(skipped, message)
						continue
					case "warn":
						fmt.Fprintln(cmd.ErrOrStderr(), "warning:", message)
					}
					serialTargets = true
				}
			}
		}
		plan = append(plan, operation{source: f, dest: target, localBase: localBase})
	}
	var cleanup []operation
	if name == "sync" && o.delete {
		// Iterate the sorted scan order, not a map, for stable previews.
		var names []string
		for relative := range destFiles {
			names = append(names, relative)
		}
		slices.Sort(names)
		for _, relative := range names {
			f := destFiles[relative]
			if !f.dir && !sourceNames[relative] && included(o.filters, relative) {
				cleanup = append(cleanup, operation{source: f, localBase: localBase, remove: true})
			}
		}
	}
	if serialTargets && o.concurrency > 1 && !o.dryrun {
		// ponytail: serialize the whole plan on case collisions; schedule
		// independent target groups only if mixed-case workloads need it.
		options := *o
		options.concurrency = 1
		o = &options
		fmt.Fprintln(cmd.ErrOrStderr(), "warning: case-colliding download targets require serial transfers")
	}
	return executePlan(cmd, cfg, o, name, plan, cleanup, skipped)
}

// localSyncName finds the scanned spelling of an existing local path. Resolve
// each component so exact hard-link names remain distinct, even under aliases
// of their parent directories. Never infer filesystem equality from case alone.
func localSyncName(base location, relative string) (string, error) {
	if _, err := base.join(relative); err != nil {
		return "", err
	}
	parent := base.name
	parts := strings.Split(relative, "/")
	for i, part := range parts {
		// ponytail: scan only unmatched paths; cache directory listings if large
		// case-mismatched syncs make repeated directory reads expensive.
		entries, err := os.ReadDir(parent)
		if errors.Is(err, os.ErrNotExist) {
			return relative, nil
		}
		if err != nil {
			return "", err
		}
		exact := slices.ContainsFunc(entries, func(e os.DirEntry) bool { return e.Name() == part })
		if !exact {
			wanted, err := os.Lstat(filepath.Join(parent, part))
			if errors.Is(err, os.ErrNotExist) {
				return relative, nil
			}
			if err != nil {
				return "", err
			}
			for _, entry := range entries {
				if !strings.EqualFold(entry.Name(), part) {
					continue
				}
				info, err := entry.Info()
				if err != nil {
					return "", err
				}
				if os.SameFile(wanted, info) {
					parts[i] = entry.Name()
					break
				}
			}
		}
		parent = filepath.Join(parent, parts[i])
	}
	return strings.Join(parts, "/"), nil
}

func overlaps(a, b, separator string) bool {
	return a == b || strings.HasPrefix(a, strings.TrimSuffix(b, separator)+separator) ||
		strings.HasPrefix(b, strings.TrimSuffix(a, separator)+separator)
}

func needsSync(source, dest fileEntry, o *fileOptions) bool {
	if source.size != dest.size {
		return true
	}
	if o.sizeOnly {
		return false
	}
	if source.loc.remote && !dest.loc.remote {
		if o.exactTimestamps {
			return !source.mtime.Equal(dest.mtime)
		}
		// AWS download sync's comparison is intentionally asymmetric.
		return dest.mtime.Truncate(time.Second).After(source.mtime.Truncate(time.Second))
	}
	// Netdisk stores whole seconds. Comparing local nanoseconds would cause
	// uploads in the server's creation second to repeat indefinitely.
	return source.mtime.Truncate(time.Second).After(dest.mtime.Truncate(time.Second))
}

func executePlan(cmd *cobra.Command, cfg *config, o *fileOptions, name string, plan, cleanup []operation, skipped []string) error {
	parent := cmd.Context()
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	cmd.SetContext(ctx)
	defer cmd.SetContext(parent)
	workers := max(1, min(o.concurrency, len(plan)))
	if o.dryrun {
		workers = 1
	}
	if workers > 1 {
		options := *o
		options.progressMultiline = true
		o = &options
		out, diagnostics := cmd.OutOrStdout(), cmd.ErrOrStderr()
		var outputMu sync.Mutex
		cmd.SetOut(&lockedWriter{mu: &outputMu, writer: out})
		cmd.SetErr(&lockedWriter{mu: &outputMu, writer: diagnostics})
		defer cmd.SetOut(out)
		defer cmd.SetErr(diagnostics)
	}
	for _, message := range skipped {
		fmt.Fprintln(cmd.ErrOrStderr(), "warning:", message)
	}
	var failed error
	var stateMu sync.Mutex
	execute := func(op operation) error {
		if err := cmd.Context().Err(); err != nil {
			return err
		}
		if !o.dryrun {
			var err error
			if op.remove {
				err = removeEntry(cmd.Context(), cfg, op.source, op.localBase, o.pageSize)
			} else {
				progressName := ""
				if len(plan) > 1 {
					progressName = op.dest.base()
				}
				err = transferEntry(cmd, cfg, o, name, op, progressName)
			}
			if err != nil {
				stateMu.Lock()
				defer stateMu.Unlock()
				if o.noOverwrite && isDestinationConflict(err) {
					message := op.dest.String() + ": destination appeared during transfer"
					skipped = append(skipped, message)
					fmt.Fprintln(cmd.ErrOrStderr(), "warning:", message)
					return nil
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "failed: %s: %v\n", op.source.loc.String(), err)
				if failed == nil {
					failed = err
				}
				return err
			}
		}
		printAction(cmd, o, name, op)
		return nil
	}
	next := 0
	work := func() {
		for {
			stateMu.Lock()
			if ctx.Err() != nil || next == len(plan) {
				stateMu.Unlock()
				return
			}
			op := plan[next]
			next++
			stateMu.Unlock()
			if err := execute(op); errors.Is(err, context.Canceled) || ExitCode(err) == 253 {
				cancel(err)
				return
			}
		}
	}
	if workers == 1 {
		work()
	} else {
		var group sync.WaitGroup
		for range workers {
			group.Go(work)
		}
		group.Wait()
	}
	if err := context.Cause(ctx); err != nil {
		return err
	}
	if failed == nil && len(skipped) == 0 {
		for _, op := range cleanup {
			if err := execute(op); err != nil {
				break
			}
		}
	} else if len(cleanup) != 0 {
		fmt.Fprintln(cmd.ErrOrStderr(), "warning: sync cleanup suppressed because files failed or were skipped")
	}
	if failed != nil {
		return failed
	}
	if len(skipped) != 0 {
		return withCode(2, fmt.Errorf("%d file(s) skipped", len(skipped)))
	}
	return cmd.Context().Err()
}

func isDestinationConflict(err error) bool {
	return errors.Is(err, os.ErrExist) || errors.Is(err, netdiskError(-8))
}

type lockedWriter struct {
	mu     *sync.Mutex
	writer io.Writer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writer.Write(p)
}

func printAction(cmd *cobra.Command, o *fileOptions, name string, op operation) {
	if o.quiet || o.onlyErrors || op.dest.stream {
		return
	}
	verb := "copy"
	if op.remove {
		verb = "delete"
	} else if name == "mv" {
		verb = "move"
	} else if !op.source.loc.remote {
		verb = "upload"
	} else if !op.dest.remote {
		verb = "download"
	}
	prefix := ""
	if o.dryrun {
		prefix = "(dryrun) "
	}
	action := fmt.Sprintf("%s%s: %s", prefix, verb, op.source.loc.String())
	if !op.remove {
		action += " to " + op.dest.String()
	}
	fmt.Fprintln(cmd.OutOrStdout(), action)
}

func transferEntry(cmd *cobra.Command, cfg *config, o *fileOptions, name string, op operation, progressName string) error {
	if name == "mv" && !op.source.loc.remote {
		if err := checkMoveAncestors(op.source.loc.name); err != nil {
			return err
		}
	}
	if err := verifyEntry(cmd.Context(), cfg, op.source, o.pageSize); err != nil {
		return err
	}
	progress := newProgress(cmd, o, op.source.size, progressName)
	progress.start()
	defer progress.finish()
	if op.dest.remote {
		if err := cfg.ensureRemoteDir(cmd.Context(), path.Dir(op.dest.name), o.pageSize); err != nil {
			return err
		}
		var err error
		if op.source.loc.remote {
			err = cfg.manage(cmd.Context(), "copy", op.source.loc.name, op.dest.name, o.noOverwrite)
		} else {
			err = uploadFile(cmd.Context(), cfg, op.source.loc.name, op.dest.name, o.noOverwrite, o.partConcurrency, progress.add)
		}
		if err != nil {
			return err
		}
		// Confirm exact destination and size before a move may remove its source.
		confirmed, err := cfg.stat(cmd.Context(), op.dest, o.pageSize)
		if err != nil {
			return err
		}
		if confirmed.dir || confirmed.size != op.source.size {
			return errors.New("destination size was not confirmed")
		}
	} else {
		if err := downloadAtomic(cmd.Context(), cfg, op.source, op.dest.name, op.localBase, o.noOverwrite, progress); err != nil {
			return err
		}
	}
	// Rapid uploads and server-side copies may not report individual bytes.
	progress.complete()
	if name == "mv" {
		if !op.source.loc.remote {
			if err := checkMoveAncestors(op.source.loc.name); err != nil {
				return err
			}
		}
		return removeEntry(cmd.Context(), cfg, op.source, filepath.Dir(op.source.loc.name), o.pageSize)
	}
	return nil
}

func verifyEntry(ctx context.Context, cfg *config, entry fileEntry, pageSize int) error {
	if !entry.loc.remote {
		return unchangedFile(entry.loc.name, entry.info)
	}
	current, err := cfg.statRemote(ctx, entry.loc.name, pageSize)
	if err != nil {
		return err
	}
	if current.ID != entry.disk.ID || current.Size != entry.size ||
		current.modified() != entry.disk.modified() || current.IsDir != 0 {
		return errors.New("remote source changed after planning")
	}
	return nil
}

func removeEntry(ctx context.Context, cfg *config, entry fileEntry, localBase string, pageSize int) error {
	if err := verifyEntry(ctx, cfg, entry, pageSize); err != nil {
		return err
	}
	if entry.loc.remote {
		return cfg.manage(ctx, "delete", entry.loc.name, "", false)
	}
	root, relative, err := localRoot(localBase, entry.loc.name)
	if err != nil {
		return err
	}
	defer root.Close()
	return root.Remove(relative)
}

// localRoot pins an existing ancestor. All derived paths are resolved by os.Root,
// which prevents a concurrently replaced symlink from escaping that ancestor.
func localRoot(base, target string) (*os.Root, string, error) {
	if relative, err := filepath.Rel(base, target); err != nil || !filepath.IsLocal(relative) {
		return nil, "", errors.New("local target escapes destination")
	}
	ancestor := base
	for {
		info, err := os.Stat(ancestor)
		if err == nil {
			if !info.IsDir() {
				return nil, "", errors.New("local destination parent is not a directory")
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, "", err
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return nil, "", err
		}
		ancestor = parent
	}
	root, err := os.OpenRoot(ancestor)
	if err != nil {
		return nil, "", err
	}
	relative, err := filepath.Rel(ancestor, target)
	if err != nil || !filepath.IsLocal(relative) {
		root.Close()
		return nil, "", errors.New("invalid local target")
	}
	return root, relative, nil
}

func checkLocalTarget(base, target string) error {
	root, relative, err := localRoot(base, target)
	if err != nil {
		return err
	}
	defer root.Close()
	// Do not replace symlinks or special files, even links remaining inside root.
	parts := strings.Split(relative, string(filepath.Separator))
	for i := range parts {
		info, err := root.Lstat(filepath.Join(parts[:i+1]...))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return fmt.Errorf("unsafe local destination: %s", target)
		}
		if i < len(parts)-1 && !info.IsDir() {
			return fmt.Errorf("destination parent is a file: %s", target)
		}
		if i == len(parts)-1 && info.IsDir() {
			return fmt.Errorf("destination is a directory: %s", target)
		}
	}
	return nil
}

func downloadAtomic(ctx context.Context, cfg *config, entry fileEntry, target, base string, noOverwrite bool, progress *transferProgress) error {
	if err := checkLocalTarget(base, target); err != nil {
		return err
	}
	root, relative, err := localRoot(base, target)
	if err != nil {
		return err
	}
	defer root.Close()
	parent := filepath.Dir(relative)
	if err := root.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	temp := filepath.Join(parent, fmt.Sprintf(".bdpan-%x.tmp", randomBytes()))
	f, err := root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer root.Remove(temp)
	err = cfg.downloadFile(ctx, entry.disk, f, progress.options.partConcurrency, progress.add)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := root.Chtimes(temp, entry.mtime, entry.mtime); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if noOverwrite {
		// Link is an atomic "create only if absent"; a precheck + Rename is not.
		return root.Link(temp, relative)
	}
	return root.Rename(temp, relative)
}

func randomBytes() []byte {
	// crypto/rand.Read cannot fail on supported Go platforms.
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return b
}

func caseCollision(target string, seen map[string]string, skipConflicts bool) (string, error) {
	var conflict string
	pending := make(map[string]string)
	for name := target; ; name = filepath.Dir(name) {
		folded := strings.Map(func(r rune) rune {
			smallest := r
			for next := unicode.SimpleFold(r); next != r; next = unicode.SimpleFold(next) {
				smallest = min(smallest, next)
			}
			return smallest
		}, name)
		if old, ok := seen[folded]; ok && old != name {
			conflict = old
		}
		pending[folded] = name
		parent := filepath.Dir(name)
		if parent == name {
			break
		}
		entries, err := os.ReadDir(parent)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		for _, entry := range entries {
			if entry.Name() != filepath.Base(name) && strings.EqualFold(entry.Name(), filepath.Base(name)) {
				conflict = filepath.Join(parent, entry.Name())
			}
		}
	}
	// A skipped path must not reserve children or replace accepted ancestors.
	if conflict == "" || !skipConflicts {
		maps.Copy(seen, pending)
	}
	return conflict, nil
}

type transferProgress struct {
	mu        sync.Mutex
	cmd       *cobra.Command
	options   *fileOptions
	filename  string
	total     int64
	completed int64
	displayed int64
	last      time.Time
	width     int
	written   bool
	traffic   int64
	samples   []progressSample
	verified  bool
	phase     string
	stop      chan struct{}
	stopped   chan struct{}
}

type progressSample struct {
	at      time.Time
	traffic int64
}

func newProgress(cmd *cobra.Command, o *fileOptions, total int64, filename string) *transferProgress {
	now := time.Now()
	return &transferProgress{cmd: cmd, options: o, filename: filename, total: total, last: now,
		phase: "preparing", samples: []progressSample{{at: now}}}
}

func (p *transferProgress) suppressed() bool {
	return p.options.noProgress || p.options.quiet || p.options.onlyErrors
}

func (p *transferProgress) start() {
	if p.suppressed() {
		return
	}
	p.stop, p.stopped = make(chan struct{}), make(chan struct{})
	go func() {
		defer close(p.stopped)
		ticker := time.NewTicker(time.Duration(max(1, p.options.progressFrequency)) * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-p.stop:
				return
			case now := <-ticker.C:
				p.mu.Lock()
				if now.Sub(p.last) >= time.Duration(p.options.progressFrequency)*time.Second {
					p.write(now)
				}
				p.mu.Unlock()
			}
		}
	}()
}

func (p *transferProgress) add(n int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.completed += n
	p.traffic += max(0, n)
	if n < 0 {
		p.phase = "retrying"
	} else if n > 0 {
		p.phase = "transferring"
	}
	if p.completed >= p.total {
		p.phase = "verifying"
	}
	now := time.Now()
	if p.suppressed() ||
		now.Sub(p.last) < time.Duration(p.options.progressFrequency)*time.Second {
		return
	}
	p.write(now)
}

func (p *transferProgress) write(now time.Time) {
	percent := 100.0
	if p.total > 0 {
		percent = min(100, 100*float64(p.completed)/float64(p.total))
	}
	if !p.verified {
		percent = min(99.9, percent)
	}
	rate := 0.0
	cutoff := now.Add(-5 * time.Second)
	for len(p.samples) > 1 && !p.samples[1].at.After(cutoff) {
		p.samples = p.samples[1:]
	}
	sample := p.samples[0]
	if elapsed := now.Sub(sample.at).Seconds(); elapsed > 0 {
		rate = float64(p.traffic-sample.traffic) / elapsed
	}
	p.samples = append(p.samples, progressSample{now, p.traffic})
	eta := "--"
	if p.verified {
		eta = "0s"
	} else if rate > 0 && p.completed < p.total {
		// Round up: an unfinished file must not display ETA 0s.
		eta = (time.Duration(math.Ceil(float64(p.total-p.completed)/rate)) * time.Second).String()
	}
	line := fmt.Sprintf("%.1f%% | %s/%s | %s/s | ETA %s",
		percent, displaySize(p.completed, true), displaySize(p.total, true),
		displaySize(int64(rate), true), eta)
	if p.filename != "" {
		// Keep terminal controls escaped, but omit the surrounding quotes.
		quoted := fmt.Sprintf("%q", p.filename)
		line = quoted[1:len(quoted)-1] + ": " + line
	}
	if !p.verified {
		line += " | " + p.phase
	}
	if p.options.progressMultiline {
		fmt.Fprintln(p.cmd.ErrOrStderr(), line)
	} else {
		// Clear any tail left by a shorter speed/ETA, without requiring ANSI.
		fmt.Fprint(p.cmd.ErrOrStderr(), "\r", line, strings.Repeat(" ", max(0, p.width-len(line))))
	}
	p.last, p.written, p.displayed, p.width = now, true, p.completed, len(line)
}

func (p *transferProgress) complete() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.completed, p.verified = p.total, true
}

func (p *transferProgress) finish() {
	if p.stop != nil {
		close(p.stop)
		<-p.stopped
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.written && (p.displayed != p.completed || p.verified) {
		p.write(time.Now())
	}
	if p.written && !p.options.progressMultiline {
		fmt.Fprintln(p.cmd.ErrOrStderr())
	}
}

type progressWriter struct {
	writer   io.Writer
	progress *transferProgress
}

func (w *progressWriter) Write(b []byte) (int, error) {
	n, err := w.writer.Write(b)
	w.progress.add(int64(n))
	return n, err
}

func runStream(cmd *cobra.Command, cfg *config, o *fileOptions, source, dest location) error {
	if source.stream {
		if !dest.remote || dest.slash || dest.name == "/" {
			return usage("stdin upload requires an explicit remote filename")
		}
		current, err := cfg.stat(cmd.Context(), dest, o.pageSize)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil && current.dir {
			return usage("stdin destination must be a file")
		}
		if err == nil && o.noOverwrite {
			return withCode(2, errors.New("destination exists"))
		}
		op := operation{source: fileEntry{loc: source}, dest: dest}
		if !included(o.filters, "-") {
			return nil
		}
		if o.dryrun {
			printAction(cmd, o, "cp", op)
			return nil
		}
		f, err := os.CreateTemp("", "bdpan-stdin-*")
		if err != nil {
			return err
		}
		defer os.Remove(f.Name())
		copyErr := copyStdin(cmd.Context(), f, cmd.InOrStdin())
		closeErr := f.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		entry, err := cfg.stat(cmd.Context(), location{name: f.Name()}, o.pageSize)
		if err != nil {
			return err
		}
		op.source = entry
		if err := transferEntry(cmd, cfg, o, "cp", op, ""); err != nil {
			if o.noOverwrite && isDestinationConflict(err) {
				return withCode(2, fmt.Errorf("%s: destination appeared during transfer: %w", dest.String(), err))
			}
			return err
		}
		op.source.loc = source
		printAction(cmd, o, "cp", op)
		return nil
	}
	if !source.remote || !dest.stream {
		return usage("stdout download requires a remote source")
	}
	entry, err := cfg.stat(cmd.Context(), source, o.pageSize)
	if err != nil {
		return err
	}
	if entry.dir {
		return usage("stdout download requires a file")
	}
	if !included(o.filters, source.base()) || o.dryrun {
		return nil
	}
	progress := newProgress(cmd, o, entry.size, "")
	progress.start()
	defer progress.finish()
	err = cfg.download(cmd.Context(), entry.disk, &progressWriter{writer: cmd.OutOrStdout(), progress: progress})
	if err == nil {
		progress.complete()
	}
	return err
}

func copyStdin(ctx context.Context, dst io.Writer, src io.Reader) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	src, cleanup, err := prepareStdin(src)
	if err != nil {
		return err
	}
	defer cleanup()
	if closer, ok := src.(io.Closer); ok {
		closed := make(chan struct{})
		stop := context.AfterFunc(ctx, func() {
			// Closing pipes interrupts an in-flight Read, unlike contextReader.
			_ = closer.Close()
			close(closed)
		})
		defer func() {
			if !stop() {
				<-closed
			}
		}()
	}
	_, err = io.Copy(dst, contextReader{ctx, src})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}
