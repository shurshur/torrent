package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// File I/O and command-line helpers

type torrentDoc struct {
	Path      string
	Mode      os.FileMode
	Top       *bValue
	Info      *bValue
	RawInfo   []byte
	InfoDirty bool
}

func loadTorrentDoc(path string) (*torrentDoc, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	top, err := parseBencode(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if top.kind != kindDict {
		return nil, fmt.Errorf("%s: metainfo is not a dictionary", path)
	}
	info := top.d.get("info")
	if info == nil || info.kind != kindDict {
		return nil, fmt.Errorf("%s: metainfo has no info dictionary", path)
	}
	rawInfo, err := findRawInfo(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(rawInfo) == 0 {
		rawInfo = encodeBencode(info)
	}
	mode := os.FileMode(0644)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	return &torrentDoc{
		Path:    path,
		Mode:    mode,
		Top:     top,
		Info:    info,
		RawInfo: rawInfo,
	}, nil
}

func (d *torrentDoc) encoded() ([]byte, error) {
	info := d.Top.d.get("info")
	if info == nil || info.kind != kindDict {
		return nil, errors.New("torrent must contain an info dictionary")
	}
	d.Info = info
	return encodeTop(d.Top, d.RawInfo, d.InfoDirty), nil
}

func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	if mode == 0 {
		mode = 0644
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".torrent-tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err == nil {
		return nil
	} else {
		// Windows does not replace an existing file with os.Rename.
		if removeErr := os.Remove(path); removeErr != nil {
			return err
		}
		return os.Rename(tmpName, path)
	}
}

func writeOutput(data []byte, inputPath, output string, inPlace bool, mode os.FileMode) (string, error) {
	if inPlace {
		if err := writeAtomic(inputPath, data, mode); err != nil {
			return "", err
		}
		return inputPath, nil
	}
	if output == "" || output == "-" {
		if _, err := os.Stdout.Write(data); err != nil {
			return "", err
		}
		return "-", nil
	}
	if err := writeAtomic(output, data, mode); err != nil {
		return "", err
	}
	return output, nil
}

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

type optString struct {
	value string
	set   bool
}

func (o *optString) String() string { return o.value }
func (o *optString) Set(v string) error {
	o.value = v
	o.set = true
	return nil
}

func boolFlags(names ...string) map[string]bool {
	m := map[string]bool{"h": true, "help": true}
	for _, n := range names {
		m[n] = true
	}
	return m
}

// reorderFlags implements GNU-style interspersed flags: options may appear
// before or after positional arguments.
func reorderFlags(args []string, isBool map[string]bool) []string {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if len(a) > 1 && a[0] == '-' {
			flags = append(flags, a)
			name := strings.TrimLeft(a, "-")
			if eq := strings.IndexByte(name, '='); eq >= 0 {
				continue
			}
			if isBool[name] {
				continue
			}
			if i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		positional = append(positional, a)
	}
	return append(flags, positional...)
}

func parseFlags(fs *flag.FlagSet, args []string, isBool map[string]bool) ([]string, error) {
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s %s\n\n", programName, fs.Name())
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorderFlags(args, isBool)); err != nil {
		return nil, err
	}
	return fs.Args(), nil
}

func formatSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func formatDate(unix int64) string {
	return time.Unix(unix, 0).UTC().Format("2006-01-02 15:04:05 UTC")
}
