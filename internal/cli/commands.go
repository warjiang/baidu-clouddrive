package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

func fileCommands(cfg *config) []*cobra.Command {
	var commands []*cobra.Command
	for _, name := range []string{"ls", "cp", "mv", "rm", "sync"} {
		o := &fileOptions{}
		use := name + " <source> <destination>"
		if name == "ls" {
			use = "ls [bd://path]"
		} else if name == "rm" {
			use = "rm <bd://path>"
		}
		cmd := &cobra.Command{
			Use: use,
			Short: map[string]string{
				"ls": "List remote files", "cp": "Copy local or remote files",
				"mv": "Move files after confirming the destination",
				"rm": "Remove remote files", "sync": "Synchronize directory contents",
			}[name],
			Args: func(cmd *cobra.Command, args []string) error {
				count := 2
				if name == "ls" {
					return withCode(252, cobra.MaximumNArgs(1)(cmd, args))
				}
				if name == "rm" {
					count = 1
				}
				return withCode(252, cobra.ExactArgs(count)(cmd, args))
			},
			RunE: func(cmd *cobra.Command, args []string) error {
				if err := validateOptions(cmd, o); err != nil {
					return err
				}
				if name == "ls" {
					return runList(cmd, cfg, o, args)
				}
				return runFiles(cmd, cfg, o, name, args)
			},
		}
		f := cmd.Flags()
		f.IntVar(&o.pageSize, "page-size", 1000, "Remote listing page size (1-1000)")
		if name != "sync" {
			f.BoolVar(&o.recursive, "recursive", false, "Operate on all files below the source")
		}
		if name == "ls" {
			f.BoolVar(&o.human, "human-readable", false, "Display sizes in binary units")
			f.BoolVar(&o.summary, "summarize", false, "Display total objects and size")
		} else {
			f.BoolVar(&o.dryrun, "dryrun", false, "Preview the plan without writing or reading stdin")
			f.BoolVar(&o.quiet, "quiet", false, "Suppress successful action output and progress")
			f.BoolVar(&o.onlyErrors, "only-show-errors", false, "Suppress successful action output and progress")
			f.Var(&filterFlag{&o.filters, false}, "exclude", "Exclude matching source-relative paths (repeatable, ordered)")
			f.Var(&filterFlag{&o.filters, true}, "include", "Include matching source-relative paths (last match wins)")
		}
		if name == "cp" || name == "mv" || name == "sync" {
			f.IntVar(&o.concurrency, "concurrency", 1, "Maximum concurrent file transfers (positive)")
			f.IntVar(&o.partConcurrency, "part-concurrency", 1, "Maximum concurrent upload/download parts per file (1-32; stdout stays sequential)")
			f.BoolVar(&o.noOverwrite, "no-overwrite", false, "Skip existing destination files")
			f.BoolVar(&o.followLinks, "follow-symlinks", true, "Follow local source symlinks")
			f.BoolVar(&o.noFollowLinks, "no-follow-symlinks", false, "Skip local source symlinks")
			f.BoolVar(&o.noProgress, "no-progress", false, "Suppress progress on stderr")
			f.IntVar(&o.progressFrequency, "progress-frequency", 1, "Minimum seconds between progress updates")
			f.BoolVar(&o.progressMultiline, "progress-multiline", false, "Put each progress update on a new line")
			f.StringVar(&o.caseConflict, "case-conflict", "ignore", "Download case collisions: error, warn, skip, ignore")
		}
		if name == "cp" {
			f.Int64Var(&o.expectedSize, "expected-size", 0, "Advisory stdin size in bytes (stdin is spooled for multipart upload)")
		}
		if name == "sync" {
			o.recursive = true
			f.BoolVar(&o.sizeOnly, "size-only", false, "Compare file sizes only")
			f.BoolVar(&o.exactTimestamps, "exact-timestamps", false, "Downloads require exactly equal timestamps to skip")
			f.BoolVar(&o.delete, "delete", false, "Delete included destination files missing from the source")
		}
		commands = append(commands, cmd)
	}
	for _, name := range []string{"mb", "rb", "presign", "website"} {
		commands = append(commands, &cobra.Command{
			Use: name, Short: "Unsupported: requires S3 bucket or signing semantics",
			RunE: func(_ *cobra.Command, _ []string) error {
				return usage("%s is unsupported: Netdisk has no S3 buckets, presigned URLs, or website hosting", name)
			},
		})
	}
	return commands
}

func validateOptions(cmd *cobra.Command, o *fileOptions) error {
	if cmd.Flags().Lookup("concurrency") != nil && o.concurrency < 1 {
		return usage("--concurrency must be positive")
	}
	if cmd.Flags().Lookup("part-concurrency") != nil && (o.partConcurrency < 1 || o.partConcurrency > 32) {
		return usage("--part-concurrency must be between 1 and 32")
	}
	if o.pageSize < 1 || o.pageSize > 1000 {
		return usage("--page-size must be between 1 and 1000")
	}
	if o.progressFrequency < 0 || (cmd.Name() != "ls" && cmd.Name() != "rm" && o.progressFrequency == 0) {
		return usage("--progress-frequency must be positive")
	}
	if o.expectedSize < 0 {
		return usage("--expected-size cannot be negative")
	}
	if cmd.Flags().Changed("follow-symlinks") && cmd.Flags().Changed("no-follow-symlinks") {
		return usage("--follow-symlinks and --no-follow-symlinks cannot be combined")
	}
	if o.noFollowLinks {
		o.followLinks = false
	}
	switch o.caseConflict {
	case "", "error", "warn", "skip", "ignore":
	default:
		return usage("--case-conflict must be error, warn, skip, or ignore")
	}
	return nil
}

func runList(cmd *cobra.Command, cfg *config, o *fileOptions, args []string) error {
	raw := "bd://"
	if len(args) != 0 {
		raw = args[0]
	}
	loc, err := parseLocation(raw)
	if err != nil {
		return err
	}
	if !loc.remote {
		return usage("ls requires a bd:// URI")
	}
	files, _, err := cfg.scan(cmd.Context(), loc, o.recursive, false, false, o.pageSize)
	if errors.Is(err, os.ErrNotExist) && !loc.slash {
		// Like s3 ls, a non-directory argument may be a filename prefix.
		parent := loc
		parent.name = strings.TrimSuffix(loc.name, loc.base())
		parent.name = strings.TrimSuffix(parent.name, "/")
		if parent.name == "" {
			parent.name = "/"
		}
		candidates, _, scanErr := cfg.scan(cmd.Context(), parent, o.recursive, false, false, o.pageSize)
		err = scanErr
		for _, f := range candidates {
			if strings.HasPrefix(f.loc.name, loc.name) {
				files = append(files, f)
			}
		}
	}
	if err != nil {
		return err
	}
	var total int64
	count := 0
	for _, f := range files {
		if f.dir {
			if !o.recursive {
				fmt.Fprintf(cmd.OutOrStdout(), "                           PRE %s/\n", f.rel)
			}
			continue
		}
		key := f.rel
		if o.recursive {
			key = strings.TrimPrefix(f.loc.name, "/")
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s %10s %s\n", f.mtime.Local().Format("2006-01-02 15:04:05"), displaySize(f.size, o.human), key)
		total += f.size
		count++
	}
	if o.summary {
		fmt.Fprintf(cmd.OutOrStdout(), "\nTotal Objects: %d\n   Total Size: %s\n", count, displaySize(total, o.human))
	}
	return nil
}

func displaySize(size int64, human bool) string {
	if !human {
		return fmt.Sprint(size)
	}
	n := float64(size)
	units := []string{"Bytes", "KiB", "MiB", "GiB", "TiB", "PiB"}
	i := 0
	for n >= 1024 && i < len(units)-1 {
		n /= 1024
		i++
	}
	return fmt.Sprintf("%.1f %s", n, units[i])
}
