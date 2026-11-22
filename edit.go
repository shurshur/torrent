package main

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Editing paths and values

type pathElem struct {
	key     string
	index   int
	isIndex bool
}

func parsePath(path string) ([]pathElem, error) {
	if path == "" {
		return nil, errors.New("empty key path")
	}
	var out []pathElem
	i := 0
	for i < len(path) {
		switch path[i] {
		case '.':
			if i == 0 || i+1 >= len(path) || path[i+1] == '.' {
				return nil, fmt.Errorf("invalid key path %q", path)
			}
			i++
		case '[':
			if i+1 < len(path) && path[i+1] == '"' {
				j := i + 2
				for j < len(path) {
					if path[j] == '\\' {
						j += 2
						continue
					}
					if path[j] == '"' {
						break
					}
					j++
				}
				if j >= len(path) {
					return nil, fmt.Errorf("unterminated quoted key in %q", path)
				}
				key, err := strconv.Unquote(path[i+1 : j+1])
				if err != nil {
					return nil, fmt.Errorf("invalid quoted key in %q: %w", path, err)
				}
				if j+1 >= len(path) || path[j+1] != ']' {
					return nil, fmt.Errorf("expected ] after quoted key in %q", path)
				}
				out = append(out, pathElem{key: key})
				i = j + 2
				continue
			}
			j := i + 1
			for j < len(path) && path[j] != ']' {
				j++
			}
			if j >= len(path) {
				return nil, fmt.Errorf("unterminated index in %q", path)
			}
			idxStr := path[i+1 : j]
			if idxStr == "" {
				return nil, fmt.Errorf("empty index in %q", path)
			}
			idx, err := strconv.Atoi(idxStr)
			if err != nil || idx < 0 {
				return nil, fmt.Errorf("invalid index %q in %q", idxStr, path)
			}
			out = append(out, pathElem{index: idx, isIndex: true})
			i = j + 1
		default:
			var sb strings.Builder
			for i < len(path) && path[i] != '.' && path[i] != '[' {
				if path[i] == '\\' {
					if i+1 >= len(path) {
						return nil, fmt.Errorf("trailing escape in %q", path)
					}
					i++
				}
				sb.WriteByte(path[i])
				i++
			}
			if sb.Len() == 0 {
				return nil, fmt.Errorf("empty key segment in %q", path)
			}
			out = append(out, pathElem{key: sb.String()})
		}
	}
	return out, nil
}

func getPath(root *bValue, elems []pathElem) (*bValue, error) {
	cur := root
	for _, e := range elems {
		if e.isIndex {
			if cur.kind != kindList {
				return nil, errors.New("cannot index a non-list value")
			}
			if e.index >= len(cur.l) {
				return nil, fmt.Errorf("list index %d is out of range", e.index)
			}
			cur = cur.l[e.index]
			continue
		}
		if cur.kind != kindDict {
			return nil, errors.New("cannot use a key on a non-dictionary value")
		}
		cur = cur.d.get(e.key)
		if cur == nil {
			return nil, nil
		}
	}
	return cur, nil
}

func newContainerFor(next pathElem) *bValue {
	if next.isIndex {
		return &bValue{kind: kindList}
	}
	return newDictValue()
}

func setPath(root *bValue, elems []pathElem, val *bValue) (bool, error) {
	if len(elems) == 0 {
		return false, errors.New("empty key path")
	}
	cur := root
	for i, e := range elems {
		last := i == len(elems)-1
		if e.isIndex {
			if cur.kind != kindList {
				return false, fmt.Errorf("cannot index non-list value at %s", pathString(elems[:i+1]))
			}
			if e.index > len(cur.l) || (e.index == len(cur.l) && !last) {
				return false, fmt.Errorf("list index %d is out of range at %s", e.index, pathString(elems[:i+1]))
			}
			if last {
				if e.index == len(cur.l) {
					cur.l = append(cur.l, val)
					return true, nil
				}
				if bEqual(cur.l[e.index], val) {
					return false, nil
				}
				cur.l[e.index] = val
				return true, nil
			}
			cur = cur.l[e.index]
			continue
		}

		if cur.kind != kindDict {
			return false, fmt.Errorf("cannot use key %q on a non-dictionary value at %s", e.key, pathString(elems[:i+1]))
		}
		child := cur.d.get(e.key)
		if last {
			if child != nil && bEqual(child, val) {
				return false, nil
			}
			cur.d.setString(e.key, val)
			return true, nil
		}
		if child == nil {
			child = newContainerFor(elems[i+1])
			cur.d.setString(e.key, child)
		}
		cur = child
	}
	return false, nil
}

func unsetPath(root *bValue, elems []pathElem) (bool, error) {
	if len(elems) == 0 {
		return false, errors.New("empty key path")
	}
	cur := root
	for i, e := range elems {
		last := i == len(elems)-1
		if e.isIndex {
			if cur.kind != kindList {
				return false, fmt.Errorf("cannot index non-list value at %s", pathString(elems[:i+1]))
			}
			if e.index >= len(cur.l) {
				return false, nil
			}
			if last {
				cur.l = append(cur.l[:e.index], cur.l[e.index+1:]...)
				return true, nil
			}
			cur = cur.l[e.index]
			continue
		}
		if cur.kind != kindDict {
			return false, fmt.Errorf("cannot use key %q on a non-dictionary value at %s", e.key, pathString(elems[:i+1]))
		}
		if last {
			return cur.d.del(e.key), nil
		}
		cur = cur.d.get(e.key)
		if cur == nil {
			return false, nil
		}
	}
	return false, nil
}

func appendPath(root *bValue, elems []pathElem, val *bValue) (bool, error) {
	if len(elems) == 0 {
		return false, errors.New("empty key path")
	}
	if elems[len(elems)-1].isIndex {
		return false, errors.New("--append requires a key path, not a list index")
	}
	cur := root
	for i, e := range elems {
		last := i == len(elems)-1
		if e.isIndex {
			if cur.kind != kindList {
				return false, fmt.Errorf("cannot index non-list value at %s", pathString(elems[:i+1]))
			}
			if e.index >= len(cur.l) {
				return false, fmt.Errorf("list index %d is out of range at %s", e.index, pathString(elems[:i+1]))
			}
			cur = cur.l[e.index]
			continue
		}
		if cur.kind != kindDict {
			return false, fmt.Errorf("cannot use key %q on a non-dictionary value at %s", e.key, pathString(elems[:i+1]))
		}
		if last {
			list := cur.d.get(e.key)
			if list == nil {
				list = &bValue{kind: kindList}
				cur.d.setString(e.key, list)
			}
			if list.kind != kindList {
				return false, fmt.Errorf("%s is not a list", pathString(elems))
			}
			list.l = append(list.l, val)
			return true, nil
		}
		child := cur.d.get(e.key)
		if child == nil {
			child = newContainerFor(elems[i+1])
			cur.d.setString(e.key, child)
		}
		cur = child
	}
	return false, nil
}

func pathString(elems []pathElem) string {
	var sb strings.Builder
	for i, e := range elems {
		if e.isIndex {
			fmt.Fprintf(&sb, "[%d]", e.index)
			continue
		}
		if i > 0 {
			sb.WriteByte('.')
		}
		sb.WriteString(e.key)
	}
	return sb.String()
}

func splitKV(s string) (string, string, error) {
	i := strings.IndexByte(s, '=')
	if i <= 0 {
		return "", "", fmt.Errorf("expected PATH=VALUE, got %q", s)
	}
	return s[:i], s[i+1:], nil
}

func parseAutoValue(s string) (*bValue, error) {
	switch {
	case strings.HasPrefix(s, "str:"):
		return newBytes([]byte(strings.TrimPrefix(s, "str:"))), nil
	case strings.HasPrefix(s, "int:"):
		v, err := strconv.ParseInt(strings.TrimPrefix(s, "int:"), 10, 64)
		if err != nil {
			return nil, err
		}
		return newInt(v), nil
	case strings.HasPrefix(s, "b64:"):
		b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(s, "b64:"))
		if err != nil {
			return nil, err
		}
		return newBytes(b), nil
	case strings.HasPrefix(s, "hex:"):
		b, err := hex.DecodeString(strings.TrimPrefix(s, "hex:"))
		if err != nil {
			return nil, err
		}
		return newBytes(b), nil
	case strings.HasPrefix(s, "file:"):
		b, err := os.ReadFile(strings.TrimPrefix(s, "file:"))
		if err != nil {
			return nil, err
		}
		return newBytes(b), nil
	case strings.HasPrefix(s, "json:"):
		return parseJSONValue([]byte(strings.TrimPrefix(s, "json:")))
	case strings.HasPrefix(s, "bencode:"):
		return parseBencode([]byte(strings.TrimPrefix(s, "bencode:")))
	default:
		return newBytes([]byte(s)), nil
	}
}

func tiersValue(tiers [][]string) *bValue {
	list := &bValue{kind: kindList}
	for _, tier := range tiers {
		tierList := &bValue{kind: kindList}
		for _, u := range tier {
			tierList.l = append(tierList.l, newString(u))
		}
		list.l = append(list.l, tierList)
	}
	return list
}

type editOptions struct {
	sets            stringList
	setInts         stringList
	setJSONs        stringList
	setBencodes     stringList
	unsets          stringList
	appends         stringList
	trackers        stringList
	removeTrackers  stringList
	announce        optString
	comment         optString
	source          optString
	output          string
	inPlace         bool
	dryRun          bool
	verbose         bool
	quiet           bool
	private         bool
	public          bool
	unsetAnnounce   bool
	unsetComment    bool
	unsetSource     bool
	replaceTrackers bool
	clearTrackers   bool
	noAutoAnnounce  bool
	ignoreMissing   bool
}

func cmdEdit(args []string) error {
	fs := flag.NewFlagSet("edit [options] FILE", flag.ContinueOnError)
	var o editOptions
	fs.Var(&o.sets, "set", "set PATH=VALUE (repeatable; default value type is string)")
	fs.Var(&o.setInts, "set-int", "set PATH=INTEGER (repeatable)")
	fs.Var(&o.setJSONs, "set-json", "set PATH=JSON (repeatable)")
	fs.Var(&o.setBencodes, "set-bencode", "set PATH=BENCODE (repeatable)")
	fs.Var(&o.unsets, "unset", "remove key or list element at PATH (repeatable)")
	fs.Var(&o.appends, "append", "append VALUE to the list at PATH (repeatable)")
	fs.Var(&o.trackers, "tracker", "add tracker URL to announce-list (repeatable)")
	fs.Var(&o.removeTrackers, "remove-tracker", "remove tracker URL (repeatable)")
	fs.Var(&o.announce, "announce", "set top-level announce URL")
	fs.Var(&o.comment, "comment", "set top-level comment")
	fs.Var(&o.source, "source", "set info.source")
	fs.StringVar(&o.output, "output", "", "write to FILE (use - for stdout)")
	fs.StringVar(&o.output, "o", "", "write to FILE (shorthand)")
	fs.BoolVar(&o.inPlace, "in-place", false, "modify the input file in place")
	fs.BoolVar(&o.inPlace, "i", false, "modify the input file in place (shorthand)")
	fs.BoolVar(&o.dryRun, "dry-run", false, "do not write anything")
	fs.BoolVar(&o.verbose, "verbose", false, "show applied operations")
	fs.BoolVar(&o.quiet, "quiet", false, "do not print a summary")
	fs.BoolVar(&o.private, "private", false, "set info.private to 1")
	fs.BoolVar(&o.public, "public", false, "remove info.private")
	fs.BoolVar(&o.unsetAnnounce, "unset-announce", false, "remove top-level announce")
	fs.BoolVar(&o.unsetComment, "unset-comment", false, "remove top-level comment")
	fs.BoolVar(&o.unsetSource, "unset-source", false, "remove info.source")
	fs.BoolVar(&o.replaceTrackers, "replace-trackers", false, "replace announce-list instead of appending")
	fs.BoolVar(&o.noAutoAnnounce, "no-auto-announce", false, "do not derive announce from --tracker")
	fs.BoolVar(&o.ignoreMissing, "ignore-missing", false, "ignore --unset paths that do not exist")
	help := fs.Bool("help", false, "show help")
	isBool := boolFlags("in-place", "i", "dry-run", "verbose", "quiet", "private", "public",
		"unset-announce", "unset-comment", "unset-source", "replace-trackers",
		"no-auto-announce", "ignore-missing", "help")
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
		return errors.New("edit: exactly one torrent file is required")
	}
	if o.private && o.public {
		return errors.New("edit: --private and --public are mutually exclusive")
	}
	if o.inPlace && o.output != "" {
		return errors.New("edit: --in-place and --output are mutually exclusive")
	}

	doc, err := loadTorrentDoc(paths[0])
	if err != nil {
		return err
	}
	res, err := applyEdit(doc, &o)
	if err != nil {
		return err
	}
	if o.verbose {
		for _, op := range res.Operations {
			fmt.Fprintln(os.Stderr, op)
		}
	}
	if !o.quiet {
		if res.OldV1 != "" {
			fmt.Fprintf(os.Stderr, "info hash v1: %s", res.OldV1)
			if res.NewV1 != res.OldV1 {
				fmt.Fprintf(os.Stderr, " -> %s (changed)", res.NewV1)
			} else {
				fmt.Fprintf(os.Stderr, " (unchanged)")
			}
			fmt.Fprintln(os.Stderr)
		}
		if res.OldV2 != "" {
			fmt.Fprintf(os.Stderr, "info hash v2: %s", res.OldV2)
			if res.NewV2 != res.OldV2 {
				fmt.Fprintf(os.Stderr, " -> %s (changed)", res.NewV2)
			} else {
				fmt.Fprintf(os.Stderr, " (unchanged)")
			}
			fmt.Fprintln(os.Stderr)
		}
		if o.verbose {
			for _, warning := range res.Warnings {
				fmt.Fprintf(os.Stderr, "warning: %s\n", warning)
			}
		}
	}

	if o.dryRun {
		if !o.quiet {
			fmt.Fprintln(os.Stderr, "dry run: nothing written")
		}
		return nil
	}
	if o.output == "" && !o.inPlace && isTerminal(os.Stdout) {
		return errors.New("edit: use --output FILE, --in-place, or redirect stdout")
	}
	written, err := writeOutput(res.Encoded, paths[0], o.output, o.inPlace, doc.Mode)
	if err != nil {
		return err
	}
	if !o.quiet {
		if written == "-" {
			fmt.Fprintln(os.Stderr, "written to stdout")
		} else {
			fmt.Fprintf(os.Stderr, "wrote %s\n", written)
		}
	}
	return nil
}

func cloneTiers(tiers [][]string) [][]string {
	out := make([][]string, len(tiers))
	for i, tier := range tiers {
		out[i] = append([]string(nil), tier...)
	}
	return out
}

func removeTrackerURL(tiers [][]string, url string) [][]string {
	var out [][]string
	for _, tier := range tiers {
		var kept []string
		for _, u := range tier {
			if u != url {
				kept = append(kept, u)
			}
		}
		if len(kept) > 0 {
			out = append(out, kept)
		}
	}
	return out
}

func firstTracker(tiers [][]string) string {
	for _, tier := range tiers {
		for _, u := range tier {
			if u != "" {
				return u
			}
		}
	}
	return ""
}
