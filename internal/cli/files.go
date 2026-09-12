package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

func withCode(code int, err error) error {
	if err == nil {
		return nil
	}
	return &exitError{code, err}
}

func usage(format string, args ...any) error { return withCode(252, fmt.Errorf(format, args...)) }

// ExitCode maps file-operation errors without changing the auth API's errors.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	if errors.Is(err, context.Canceled) {
		return 130
	}
	var coded *exitError
	if errors.As(err, &coded) {
		return coded.code
	}
	return 1
}

type location struct {
	name   string
	remote bool
	slash  bool
	stream bool
}

func parseLocation(raw string) (location, error) {
	loc := location{slash: strings.HasSuffix(raw, "/")}
	if raw == "-" {
		loc.name, loc.stream = "-", true
		return loc, nil
	}
	if strings.HasPrefix(raw, "bd://") {
		loc.remote = true
		loc.name = "/" + strings.TrimSuffix(strings.TrimPrefix(raw, "bd://"), "/")
		if !validRemotePath(loc.name) {
			return loc, usage("invalid remote path: %q (use bd://apps/name/file)", raw)
		}
		return loc, nil
	}
	if raw == "" || strings.Contains(raw, "://") {
		return loc, usage("expected a local path or bd:// URI")
	}
	loc.slash = os.IsPathSeparator(raw[len(raw)-1])
	var err error
	loc.name, err = filepath.Abs(raw)
	return loc, err
}

func validRemotePath(name string) bool {
	return strings.HasPrefix(name, "/") && path.Clean(name) == name &&
		!strings.ContainsAny(name, "\\\x00\r\n")
}

func (l location) String() string {
	if l.remote {
		return "bd:/" + l.name
	}
	return l.name
}

func (l location) base() string {
	if l.remote {
		return path.Base(l.name)
	}
	return filepath.Base(l.name)
}

func (l location) join(relative string) (location, error) {
	l.slash = false
	if l.remote {
		l.name = path.Join(l.name, relative)
		if !validRemotePath(l.name) {
			return l, usage("invalid destination path")
		}
	} else {
		// Remote names are untrusted; reject separators and special Windows
		// names as well, so a plan has the same meaning on every supported OS.
		for _, part := range strings.Split(relative, "/") {
			stem := strings.ToUpper(strings.SplitN(part, ".", 2)[0])
			reserved := stem == "CON" || stem == "PRN" || stem == "AUX" || stem == "NUL" ||
				(len(stem) == 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) &&
					stem[3] >= '1' && stem[3] <= '9')
			if part == "" || part == "." || part == ".." || reserved ||
				strings.ContainsAny(part, "\\:\x00") || strings.HasSuffix(part, ".") || strings.HasSuffix(part, " ") {
				return l, fmt.Errorf("unsafe local filename: %q", part)
			}
		}
		native := filepath.FromSlash(relative)
		if !filepath.IsLocal(native) {
			return l, errors.New("destination escapes local directory")
		}
		l.name = filepath.Join(l.name, native)
	}
	return l, nil
}

type fileEntry struct {
	loc   location
	rel   string
	size  int64
	mtime time.Time
	dir   bool
	info  os.FileInfo
	disk  remoteFile
}

func (cfg *config) stat(ctx context.Context, loc location, pageSize int) (fileEntry, error) {
	e := fileEntry{loc: loc}
	if loc.remote {
		f, err := cfg.statRemote(ctx, loc.name, pageSize)
		if err != nil {
			return e, err
		}
		e.disk, e.size, e.dir, e.mtime = f, f.Size, f.IsDir == 1, time.Unix(f.modified(), 0)
	} else {
		info, err := os.Stat(loc.name)
		if err != nil {
			return e, err
		}
		e.info, e.size, e.dir, e.mtime = info, info.Size(), info.IsDir(), info.ModTime()
	}
	return e, nil
}

type filterRule struct {
	include bool
	match   *regexp.Regexp
}

// Both flag values append to one slice, preserving interleaved flag order.
type filterFlag struct {
	rules   *[]filterRule
	include bool
}

func (f *filterFlag) String() string { return "" }
func (f *filterFlag) Type() string   { return "pattern" }
func (f *filterFlag) Set(pattern string) error {
	re, err := globRegex(pattern)
	if err != nil {
		return err
	}
	*f.rules = append(*f.rules, filterRule{f.include, re})
	return nil
}

func globRegex(pattern string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("(?s)^")
	runes := []rune(pattern)
	for i := 0; i < len(runes); i++ {
		switch runes[i] {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteByte('.')
		case '[':
			end := i + 1
			if end < len(runes) && runes[end] == '!' {
				end++
			}
			if end < len(runes) && runes[end] == ']' {
				end++
			}
			for end < len(runes) && runes[end] != ']' {
				end++
			}
			if end == len(runes) {
				b.WriteString(`\[`)
				continue
			}
			b.WriteByte('[')
			start := i + 1
			if runes[start] == '!' {
				b.WriteByte('^')
				start++
			}
			for j := start; j < end; j++ {
				if strings.ContainsRune(`\]^`, runes[j]) || (runes[j] == '-' && (j == start || j == end-1)) {
					b.WriteByte('\\')
				}
				b.WriteRune(runes[j])
			}
			b.WriteByte(']')
			i = end
		default:
			b.WriteString(regexp.QuoteMeta(string(runes[i])))
		}
	}
	b.WriteByte('$')
	return regexp.Compile(b.String())
}

func included(rules []filterRule, relative string) bool {
	include := true
	for _, rule := range rules {
		if rule.match.MatchString(relative) {
			include = rule.include
		}
	}
	return include
}

type fileOptions struct {
	concurrency, partConcurrency                      int
	recursive, dryrun, quiet, onlyErrors              bool
	noOverwrite, followLinks, noFollowLinks           bool
	noProgress, progressMultiline                     bool
	sizeOnly, exactTimestamps, delete, human, summary bool
	pageSize, progressFrequency                       int
	expectedSize                                      int64
	caseConflict                                      string
	filters                                           []filterRule
}

// scan visits directories even when their names are excluded: a later include
// can select descendants. A failed scan never yields a partially usable plan.
func (cfg *config) scan(ctx context.Context, base location, recursive, followLinks, moving bool, pageSize int) ([]fileEntry, []string, error) {
	if moving && !base.remote {
		if err := checkMoveAncestors(base.name); err != nil {
			return nil, nil, err
		}
	}
	var files []fileEntry
	var skipped []string
	active := map[string]bool{}
	var visit func(location, string) error
	visit = func(loc location, rel string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !loc.remote {
			link, err := os.Lstat(loc.name)
			if err != nil {
				return err
			}
			if link.Mode()&os.ModeSymlink != 0 {
				if !followLinks {
					skipped = append(skipped, loc.String()+": symlink not followed")
					return nil
				}
				target, err := os.Stat(loc.name)
				if err != nil {
					skipped = append(skipped, loc.String()+": broken symlink")
					return nil
				}
				if moving && target.IsDir() {
					return fmt.Errorf("cannot move through a directory symlink: %s", loc.String())
				}
			}
		}
		e, err := cfg.stat(ctx, loc, pageSize)
		if err != nil {
			return err
		}
		e.rel = rel
		if !e.dir {
			if !loc.remote && !e.info.Mode().IsRegular() {
				skipped = append(skipped, loc.String()+": not a regular file")
				return nil
			}
			if rel == "" {
				e.rel = loc.base()
			}
			files = append(files, e)
			return nil
		}
		if !recursive && rel != "" {
			files = append(files, e)
			return nil
		}
		if rel != "" {
			files = append(files, e)
		}
		if loc.remote {
			children, err := cfg.listRemote(ctx, loc.name, pageSize)
			if err != nil {
				return err
			}
			for _, child := range children {
				next := location{name: child.Path, remote: true}
				childRel := path.Join(rel, path.Base(child.Path))
				if child.IsDir == 1 && recursive {
					if err := visit(next, childRel); err != nil {
						return err
					}
				} else {
					files = append(files, fileEntry{loc: next, rel: childRel, size: child.Size,
						dir: child.IsDir == 1, mtime: time.Unix(child.modified(), 0), disk: child})
				}
			}
			return nil
		}
		real, err := filepath.EvalSymlinks(loc.name)
		if err != nil {
			return err
		}
		if active[real] {
			return fmt.Errorf("symlink directory loop: %s", loc.String())
		}
		active[real] = true
		defer delete(active, real)
		children, err := os.ReadDir(loc.name)
		if err != nil {
			return err
		}
		for _, child := range children {
			next := location{name: filepath.Join(loc.name, child.Name())}
			if err := visit(next, path.Join(rel, child.Name())); err != nil {
				return err
			}
		}
		return nil
	}
	err := visit(base, "")
	if err != nil {
		return nil, skipped, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })
	return files, skipped, nil
}

func checkMoveAncestors(name string) error {
	for parent := filepath.Dir(name); ; parent = filepath.Dir(parent) {
		info, err := os.Lstat(parent)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("cannot move through a directory symlink: %s", parent)
		}
		if filepath.Dir(parent) == parent {
			return nil
		}
	}
}
