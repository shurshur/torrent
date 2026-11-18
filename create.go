package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Creation

type createFile struct {
	abs    string
	rel    []string
	length int64
}

type createV1Entry struct {
	abs    string
	rel    []string
	length int64
	pad    bool
}

func pathKey(parts []string) string {
	return strings.Join(parts, "/")
}

// lessPath compares torrent paths component by component.  This is the order
// used by a bencoded file tree, and v1/hybrid file lists must follow it.
func lessPath(a, b []string) bool {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return len(a) < len(b)
}

func baseName(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	return filepath.Base(filepath.Clean(path))
}

func collectCreateFiles(paths []string, outputPath, nameOverride string) ([]createFile, string, bool, error) {
	var out []createFile
	var name string
	single := false

	var outputAbs string
	if outputPath != "" && outputPath != "-" {
		if abs, err := filepath.Abs(outputPath); err == nil {
			outputAbs = abs
		}
	}

	walk := func(root string, prefix []string) error {
		return filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if p == root {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("symlinks are not supported: %s", p)
			}
			if d.IsDir() {
				return nil
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("unsupported file type: %s", p)
			}
			if outputAbs != "" {
				if abs, err := filepath.Abs(p); err == nil && abs == outputAbs {
					return nil
				}
			}
			relPath, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			parts := strings.Split(filepath.ToSlash(relPath), "/")
			rel := append(append([]string(nil), prefix...), parts...)
			out = append(out, createFile{abs: p, rel: rel, length: info.Size()})
			return nil
		})
	}

	switch len(paths) {
	case 1:
		p := paths[0]
		st, err := os.Lstat(p)
		if err != nil {
			return nil, "", false, err
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return nil, "", false, fmt.Errorf("symlinks are not supported: %s", p)
		}
		base := baseName(p)
		name = base
		if nameOverride != "" {
			name = nameOverride
		}
		if st.IsDir() {
			if err := walk(p, nil); err != nil {
				return nil, "", false, err
			}
		} else if st.Mode().IsRegular() {
			single = true
			out = append(out, createFile{abs: p, rel: []string{name}, length: st.Size()})
		} else {
			return nil, "", false, fmt.Errorf("unsupported file type: %s", p)
		}
	case 0:
		return nil, "", false, errors.New("no input path")
	default:
		name = "torrent"
		if nameOverride != "" {
			name = nameOverride
		}
		for _, p := range paths {
			st, err := os.Lstat(p)
			if err != nil {
				return nil, "", false, err
			}
			if st.Mode()&os.ModeSymlink != 0 {
				return nil, "", false, fmt.Errorf("symlinks are not supported: %s", p)
			}
			base := baseName(p)
			if st.IsDir() {
				if err := walk(p, []string{base}); err != nil {
					return nil, "", false, err
				}
			} else if st.Mode().IsRegular() {
				out = append(out, createFile{abs: p, rel: []string{base}, length: st.Size()})
			} else {
				return nil, "", false, fmt.Errorf("unsupported file type: %s", p)
			}
		}
	}

	if len(out) == 0 {
		return nil, "", false, errors.New("no files found")
	}
	sort.Slice(out, func(i, j int) bool { return lessPath(out[i].rel, out[j].rel) })
	seen := make(map[string]bool, len(out))
	for _, f := range out {
		k := pathKey(f.rel)
		if seen[k] {
			return nil, "", false, fmt.Errorf("duplicate torrent path %q", k)
		}
		seen[k] = true
		for _, part := range f.rel {
			if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "/\\\x00") {
				return nil, "", false, fmt.Errorf("unsafe torrent path component %q", cleanText([]byte(part)))
			}
		}
	}
	return out, name, single, nil
}

func createV1Entries(files []createFile, pieceLen int64, hybrid, single bool) []createV1Entry {
	if single {
		return []createV1Entry{{abs: files[0].abs, rel: files[0].rel, length: files[0].length}}
	}
	entries := make([]createV1Entry, 0, len(files)*2)
	var offset int64
	for _, f := range files {
		if hybrid && f.length > 0 && offset%pieceLen != 0 {
			padLen := pieceLen - offset%pieceLen
			entries = append(entries, createV1Entry{
				rel:    []string{".pad", strconv.FormatInt(padLen, 10)},
				length: padLen,
				pad:    true,
			})
			offset += padLen
		}
		entries = append(entries, createV1Entry{abs: f.abs, rel: f.rel, length: f.length})
		offset += f.length
	}
	return entries
}

func hashV1Entries(entries []createV1Entry, pieceLen int64) ([]byte, error) {
	ph := newPieceHasher(pieceLen)
	for _, e := range entries {
		if e.pad {
			ph.WriteZeros(e.length)
			continue
		}
		if err := feedFileExact(ph, e.abs, e.length); err != nil {
			return nil, err
		}
	}
	return ph.Finish(), nil
}

func buildV1FileList(entries []createV1Entry) *bValue {
	list := &bValue{kind: kindList}
	for _, e := range entries {
		d := newDictValue()
		d.d.setString("length", newInt(e.length))
		if e.pad {
			d.d.setString("attr", newString("p"))
		}
		pathList := &bValue{kind: kindList}
		for _, part := range e.rel {
			pathList.l = append(pathList.l, newString(part))
		}
		d.d.setString("path", pathList)
		list.l = append(list.l, d)
	}
	return list
}

func buildV2FileTree(files []createFile, hashes map[string]v2Hashes) *bValue {
	root := newDictValue()
	for _, f := range files {
		h := hashes[pathKey(f.rel)]
		cur := root
		for i, part := range f.rel {
			if i == len(f.rel)-1 {
				props := newDictValue()
				props.d.setString("length", newInt(f.length))
				if h.HasRoot {
					props.d.setString("pieces root", newBytes(h.Root[:]))
				}
				leaf := newDictValue()
				leaf.d.set([]byte{}, props)
				cur.d.set([]byte(part), leaf)
				continue
			}
			next := cur.d.get(part)
			if next == nil {
				next = newDictValue()
				cur.d.set([]byte(part), next)
			}
			cur = next
		}
	}
	return root
}

func buildPieceLayers(files []createFile, hashes map[string]v2Hashes, pieceLen int64) *bValue {
	layers := newDictValue()
	for _, f := range files {
		h := hashes[pathKey(f.rel)]
		if f.length > pieceLen && len(h.Layer) > 0 {
			layers.d.set(h.Root[:], newBytes(h.Layer))
		}
	}
	return layers
}

func defaultPieceLength(total int64, v2 bool) int64 {
	p := int64(256 * 1024)
	if v2 {
		p = blockSize
	}
	for total/p > 2000 && p < 16*1024*1024 {
		p *= 2
	}
	return p
}

func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty size")
	}
	lower := strings.ToLower(s)
	mult := int64(1)
	for _, suffix := range []struct {
		s string
		m int64
	}{
		{"kib", 1024}, {"kb", 1024}, {"k", 1024},
		{"mib", 1024 * 1024}, {"mb", 1024 * 1024}, {"m", 1024 * 1024},
		{"gib", 1024 * 1024 * 1024}, {"gb", 1024 * 1024 * 1024}, {"g", 1024 * 1024 * 1024},
		{"tib", 1024 * 1024 * 1024 * 1024}, {"tb", 1024 * 1024 * 1024 * 1024}, {"t", 1024 * 1024 * 1024 * 1024},
		{"b", 1},
	} {
		if strings.HasSuffix(lower, suffix.s) {
			mult = suffix.m
			lower = strings.TrimSpace(lower[:len(lower)-len(suffix.s)])
			break
		}
	}
	n, err := strconv.ParseInt(lower, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	if n < 0 || (n > 0 && n > (1<<62)/mult) {
		return 0, fmt.Errorf("size %q is out of range", s)
	}
	return n * mult, nil
}

func dedupeStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func cmdCreate(args []string) error {
	fs := flag.NewFlagSet("create [options] PATH...", flag.ContinueOnError)
	v1 := fs.Bool("v1", false, "create a v1 (BEP 3) torrent")
	v2 := fs.Bool("v2", false, "create a v2 (BEP 52) torrent")
	hybrid := fs.Bool("hybrid", false, "create a hybrid v1+v2 torrent")
	pieceLengthStr := fs.String("piece-length", "", "piece length (for example 262144, 256KiB, 1MiB)")
	fs.StringVar(pieceLengthStr, "p", "", "piece length (shorthand)")
	name := fs.String("name", "", "torrent name (default: input base name)")
	output := fs.String("output", "", "output file (use - for stdout)")
	fs.StringVar(output, "o", "", "output file (shorthand)")
	var announce optString
	fs.Var(&announce, "announce", "top-level announce URL")
	var trackers stringList
	fs.Var(&trackers, "tracker", "tracker URL for announce-list (repeatable)")
	private := fs.Bool("private", false, "set info.private to 1")
	var source optString
	fs.Var(&source, "source", "set info.source")
	var comment optString
	fs.Var(&comment, "comment", "set top-level comment")
	var createdBy optString
	fs.Var(&createdBy, "created-by", "set top-level created by")
	noCreatedBy := fs.Bool("no-created-by", false, "omit created by")
	creationDate := fs.Int64("creation-date", 0, "creation date as a Unix timestamp (default: now)")
	noCreationDate := fs.Bool("no-creation-date", false, "omit creation date")
	var sets stringList
	fs.Var(&sets, "set", "set PATH=VALUE on the new torrent (repeatable)")
	quiet := fs.Bool("quiet", false, "suppress the summary")
	verbose := fs.Bool("verbose", false, "show warnings")
	help := fs.Bool("help", false, "show help")
	isBool := boolFlags("v1", "v2", "hybrid", "private", "no-created-by", "no-creation-date", "quiet", "verbose", "help")
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
	if len(paths) == 0 {
		return errors.New("create: at least one input path is required")
	}
	modeCount := 0
	for _, b := range []bool{*v1, *v2, *hybrid} {
		if b {
			modeCount++
		}
	}
	if modeCount > 1 {
		return errors.New("create: choose only one of --v1, --v2, --hybrid")
	}
	useV1, useV2 := true, false
	if *v2 {
		useV1, useV2 = false, true
	} else if *hybrid {
		useV1, useV2 = true, true
	}

	files, torrentName, single, err := collectCreateFiles(paths, *output, *name)
	if err != nil {
		return err
	}
	if torrentName == "" || torrentName == "." || torrentName == ".." || strings.ContainsAny(torrentName, "/\\\x00") {
		return fmt.Errorf("create: invalid torrent name %q", torrentName)
	}
	var total int64
	for _, f := range files {
		total += f.length
	}
	var pieceLen int64
	if *pieceLengthStr != "" {
		pieceLen, err = parseSize(*pieceLengthStr)
		if err != nil {
			return fmt.Errorf("--piece-length: %w", err)
		}
	} else {
		pieceLen = defaultPieceLength(total, useV2)
	}
	if pieceLen < blockSize || pieceLen&(pieceLen-1) != 0 {
		return fmt.Errorf("piece length must be a power of two and at least %d bytes", blockSize)
	}
	if pieceLen > 64*1024*1024 {
		return fmt.Errorf("piece length %d is too large", pieceLen)
	}

	v1Entries := createV1Entries(files, pieceLen, useV1 && useV2, single)
	var pieces []byte
	if useV1 {
		pieces, err = hashV1Entries(v1Entries, pieceLen)
		if err != nil {
			return err
		}
	}

	v2Hashes := make(map[string]v2Hashes, len(files))
	if useV2 {
		for _, f := range files {
			h, err := v2FileHashes(f.abs, pieceLen)
			if err != nil {
				return err
			}
			if h.Length != f.length {
				return fmt.Errorf("%s changed while the torrent was being created (size %d, expected %d)", f.abs, h.Length, f.length)
			}
			v2Hashes[pathKey(f.rel)] = h
		}
	}

	info := newDictValue()
	info.d.setString("name", newString(torrentName))
	info.d.setString("piece length", newInt(pieceLen))
	if useV2 {
		info.d.setString("meta version", newInt(2))
		info.d.setString("file tree", buildV2FileTree(files, v2Hashes))
	}
	if useV1 {
		info.d.setString("pieces", newBytes(pieces))
		if single {
			info.d.setString("length", newInt(files[0].length))
		} else {
			info.d.setString("files", buildV1FileList(v1Entries))
		}
	}
	if *private {
		info.d.setString("private", newInt(1))
	}
	if source.set {
		info.d.setString("source", newString(source.value))
	}

	top := newDictValue()
	top.d.setString("info", info)
	trackerURLs := dedupeStrings([]string(trackers))
	if announce.set {
		top.d.setString("announce", newString(announce.value))
	} else if len(trackerURLs) > 0 {
		top.d.setString("announce", newString(trackerURLs[0]))
	}
	if len(trackerURLs) > 0 {
		tiers := make([][]string, 0, len(trackerURLs))
		for _, u := range trackerURLs {
			tiers = append(tiers, []string{u})
		}
		top.d.setString("announce-list", tiersValue(tiers))
	}
	if useV2 {
		top.d.setString("piece layers", buildPieceLayers(files, v2Hashes, pieceLen))
	}
	if !*noCreationDate {
		ts := *creationDate
		if ts == 0 {
			ts = time.Now().Unix()
		}
		top.d.setString("creation date", newInt(ts))
	}
	if !*noCreatedBy {
		creator := programName + "/" + version
		if createdBy.set {
			creator = createdBy.value
		}
		top.d.setString("created by", newString(creator))
	}
	if comment.set {
		top.d.setString("comment", newString(comment.value))
	}

	for _, item := range sets {
		pathStr, valStr, err := splitKV(item)
		if err != nil {
			return fmt.Errorf("--set %q: %w", item, err)
		}
		elems, err := parsePath(pathStr)
		if err != nil {
			return err
		}
		val, err := parseAutoValue(valStr)
		if err != nil {
			return fmt.Errorf("--set %s: %w", pathStr, err)
		}
		if _, err := setPath(top, elems, val); err != nil {
			return fmt.Errorf("--set %s: %w", pathStr, err)
		}
	}
	info = top.d.get("info")
	if info == nil || info.kind != kindDict {
		return errors.New("create: info must remain a dictionary")
	}

	encoded := encodeBencode(top)
	rawInfo := encodeBencode(info)
	a, err := analyzeTorrent(top, rawInfo)
	if err != nil {
		return err
	}

	if *output == "" && isTerminal(os.Stdout) {
		return errors.New("create: use --output FILE or redirect stdout")
	}
	written, err := writeOutput(encoded, "", *output, false, 0644)
	if err != nil {
		return err
	}
	if !*quiet {
		v1hash, v2hash := a.infoHashes(rawInfo)
		fmt.Fprintf(os.Stderr, "created: %s\n", written)
		fmt.Fprintf(os.Stderr, "name: %s\n", torrentName)
		fmt.Fprintf(os.Stderr, "version: %s\n", strings.Join(a.versions(), "+"))
		fmt.Fprintf(os.Stderr, "files: %d\n", len(files))
		fmt.Fprintf(os.Stderr, "size: %s (%d bytes)\n", formatSize(a.TotalSize), a.TotalSize)
		fmt.Fprintf(os.Stderr, "piece length: %d (%s)\n", pieceLen, formatSize(pieceLen))
		if v1hash != "" {
			fmt.Fprintf(os.Stderr, "info hash v1: %s\n", v1hash)
		}
		if v2hash != "" {
			fmt.Fprintf(os.Stderr, "info hash v2: %s\n", v2hash)
		}
		if *verbose {
			for _, warning := range a.Warnings {
				fmt.Fprintf(os.Stderr, "warning: %s\n", warning)
			}
		}
	}
	return nil
}
