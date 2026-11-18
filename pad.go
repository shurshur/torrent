package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

// ---------------------------------------------------------------------------
// Padding files

func cmdPad(args []string) error {
	fs := flag.NewFlagSet("pad [options] FILE", flag.ContinueOnError)
	dir := fs.String("dir", ".", "base directory under which the torrent's name is located")
	dryRun := fs.Bool("dry-run", false, "show what would be created")
	force := fs.Bool("force", false, "overwrite existing files")
	quiet := fs.Bool("quiet", false, "suppress per-file output")
	help := fs.Bool("help", false, "show help")
	isBool := boolFlags("dry-run", "force", "quiet", "help")
	paths, err := parseFlags(fs, args, isBool)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *help {
		fs.Usage()
		return nil
	}
	if len(paths) != 1 {
		return errors.New("pad: exactly one torrent file is required")
	}
	doc, err := loadTorrentDoc(paths[0])
	if err != nil {
		return err
	}
	a, err := analyzeTorrent(doc.Top, doc.RawInfo)
	if err != nil {
		return err
	}
	if !a.V1 {
		return errors.New("pad: the torrent has no v1 padding files")
	}
	created, skipped := 0, 0
	for _, f := range a.V1Files {
		if !f.Padding || f.Length <= 0 {
			continue
		}
		if len(f.Path) == 0 {
			return fmt.Errorf("pad: padding file of length %d has no path", f.Length)
		}
		path, err := safeJoinComponents(*dir, append([]string{string(a.NameRaw)}, f.Path...))
		if err != nil {
			return err
		}
		st, statErr := os.Stat(path)
		if statErr == nil {
			if st.IsDir() {
				return fmt.Errorf("%s is a directory", path)
			}
			if st.Size() == f.Length && !*force {
				skipped++
				if !*quiet {
					fmt.Printf("exists: %s (%d bytes)\n", path, f.Length)
				}
				continue
			}
			if !*force {
				return fmt.Errorf("%s exists with size %d, expected %d (use --force to overwrite)", path, st.Size(), f.Length)
			}
		} else if !os.IsNotExist(statErr) {
			return statErr
		}
		if *dryRun {
			fmt.Printf("would create: %s (%d bytes)\n", path, f.Length)
			created++
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return err
		}
		out, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
		if err != nil {
			return err
		}
		if err := out.Truncate(f.Length); err != nil {
			out.Close()
			return err
		}
		if err := out.Close(); err != nil {
			return err
		}
		created++
		if !*quiet {
			fmt.Printf("created: %s (%d bytes)\n", path, f.Length)
		}
	}
	if !*quiet {
		if *dryRun {
			fmt.Printf("dry run: %d padding file(s) to create, %d already present\n", created, skipped)
		} else {
			fmt.Printf("created %d padding file(s), %d already present\n", created, skipped)
		}
	}
	return nil
}
