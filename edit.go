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
	oldAnalysis, err := analyzeTorrent(doc.Top, doc.RawInfo)
	if err != nil {
		return err
	}
	oldV1, oldV2 := oldAnalysis.infoHashes(doc.RawInfo)

	infoElems := func(elems []pathElem) bool {
		return len(elems) > 0 && !elems[0].isIndex && elems[0].key == "info"
	}
	apply := func(desc string, elems []pathElem, fn func() (bool, error)) error {
		changed, err := fn()
		if err != nil {
			return fmt.Errorf("%s: %w", desc, err)
		}
		if changed && infoElems(elems) {
			doc.InfoDirty = true
		}
		if changed && o.verbose {
			fmt.Fprintf(os.Stderr, "%s\n", desc)
		}
		return nil
	}

	didOp := false
	for _, item := range o.sets {
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
		if err := apply("set "+pathStr, elems, func() (bool, error) { return setPath(doc.Top, elems, val) }); err != nil {
			return err
		}
		didOp = true
	}
	for _, item := range o.setInts {
		pathStr, valStr, err := splitKV(item)
		if err != nil {
			return fmt.Errorf("--set-int %q: %w", item, err)
		}
		elems, err := parsePath(pathStr)
		if err != nil {
			return err
		}
		n, err := strconv.ParseInt(valStr, 10, 64)
		if err != nil {
			return fmt.Errorf("--set-int %s: %w", pathStr, err)
		}
		val := newInt(n)
		if err := apply("set-int "+pathStr, elems, func() (bool, error) { return setPath(doc.Top, elems, val) }); err != nil {
			return err
		}
		didOp = true
	}
	for _, item := range o.setJSONs {
		pathStr, valStr, err := splitKV(item)
		if err != nil {
			return fmt.Errorf("--set-json %q: %w", item, err)
		}
		elems, err := parsePath(pathStr)
		if err != nil {
			return err
		}
		val, err := parseJSONValue([]byte(valStr))
		if err != nil {
			return fmt.Errorf("--set-json %s: %w", pathStr, err)
		}
		if err := apply("set-json "+pathStr, elems, func() (bool, error) { return setPath(doc.Top, elems, val) }); err != nil {
			return err
		}
		didOp = true
	}
	for _, item := range o.setBencodes {
		pathStr, valStr, err := splitKV(item)
		if err != nil {
			return fmt.Errorf("--set-bencode %q: %w", item, err)
		}
		elems, err := parsePath(pathStr)
		if err != nil {
			return err
		}
		val, err := parseBencode([]byte(valStr))
		if err != nil {
			return fmt.Errorf("--set-bencode %s: %w", pathStr, err)
		}
		if err := apply("set-bencode "+pathStr, elems, func() (bool, error) { return setPath(doc.Top, elems, val) }); err != nil {
			return err
		}
		didOp = true
	}
	for _, pathStr := range o.unsets {
		elems, err := parsePath(pathStr)
		if err != nil {
			return err
		}
		if err := apply("unset "+pathStr, elems, func() (bool, error) {
			changed, err := unsetPath(doc.Top, elems)
			if err != nil {
				return false, err
			}
			if !changed && !o.ignoreMissing {
				return false, errors.New("path not found")
			}
			return changed, nil
		}); err != nil {
			return err
		}
		didOp = true
	}
	for _, item := range o.appends {
		pathStr, valStr, err := splitKV(item)
		if err != nil {
			return fmt.Errorf("--append %q: %w", item, err)
		}
		elems, err := parsePath(pathStr)
		if err != nil {
			return err
		}
		val, err := parseAutoValue(valStr)
		if err != nil {
			return fmt.Errorf("--append %s: %w", pathStr, err)
		}
		if err := apply("append "+pathStr, elems, func() (bool, error) { return appendPath(doc.Top, elems, val) }); err != nil {
			return err
		}
		didOp = true
	}

	if len(o.trackers) > 0 || len(o.removeTrackers) > 0 {
		tiers := cloneTiers(parseTrackers(doc.Top).Tiers)
		added := []string(o.trackers)
		if o.replaceTrackers && len(added) > 0 {
			tiers = nil
		}
		for _, u := range added {
			tiers = append(tiers, []string{u})
		}
		for _, remove := range o.removeTrackers {
			tiers = removeTrackerURL(tiers, remove)
		}
		if len(tiers) == 0 {
			doc.Top.d.del("announce-list")
		} else {
			doc.Top.d.setString("announce-list", tiersValue(tiers))
		}
		// If announce itself was removed, replace it with the first remaining tracker.
		if av := doc.Top.d.get("announce"); av != nil && av.kind == kindBytes {
			announceValue := string(av.b)
			for _, remove := range o.removeTrackers {
				if announceValue == remove {
					if first := firstTracker(tiers); first != "" {
						doc.Top.d.setString("announce", newString(first))
					} else {
						doc.Top.d.del("announce")
					}
					break
				}
			}
		}
		if !o.noAutoAnnounce && len(added) > 0 && doc.Top.d.get("announce") == nil {
			doc.Top.d.setString("announce", newString(added[0]))
		}
		didOp = true
	}
	if o.announce.set {
		if doc.Top.d.get("announce") == nil || string(valueBytes(doc.Top.d.get("announce"))) != o.announce.value {
			doc.Top.d.setString("announce", newString(o.announce.value))
		}
		didOp = true
	}
	if o.unsetAnnounce {
		doc.Top.d.del("announce")
		didOp = true
	}
	if o.comment.set {
		doc.Top.d.setString("comment", newString(o.comment.value))
		didOp = true
	}
	if o.unsetComment {
		doc.Top.d.del("comment")
		didOp = true
	}
	if o.source.set {
		info := doc.Top.d.get("info")
		if info == nil || info.kind != kindDict {
			return errors.New("info dictionary is missing")
		}
		if string(valueBytes(info.d.get("source"))) != o.source.value {
			info.d.setString("source", newString(o.source.value))
			doc.InfoDirty = true
		}
		didOp = true
	}
	if o.unsetSource {
		info := doc.Top.d.get("info")
		if info != nil && info.kind == kindDict {
			if info.d.del("source") {
				doc.InfoDirty = true
			}
		}
		didOp = true
	}
	if o.private {
		info := doc.Top.d.get("info")
		if info == nil || info.kind != kindDict {
			return errors.New("info dictionary is missing")
		}
		if pv := info.d.get("private"); pv == nil || pv.kind != kindInt || pv.i != 1 {
			info.d.setString("private", newInt(1))
			doc.InfoDirty = true
		}
		didOp = true
	}
	if o.public {
		info := doc.Top.d.get("info")
		if info != nil && info.kind == kindDict && info.d.del("private") {
			doc.InfoDirty = true
		}
		didOp = true
	}

	if !didOp {
		return errors.New("edit: no operations specified")
	}

	info := doc.Top.d.get("info")
	if info == nil || info.kind != kindDict {
		return errors.New("edit: the torrent must contain an info dictionary")
	}
	doc.Info = info
	encoded, err := doc.encoded()
	if err != nil {
		return err
	}
	analysisBytes := doc.RawInfo
	if doc.InfoDirty {
		analysisBytes = encodeBencode(doc.Info)
	}
	newAnalysis, analyzeErr := analyzeTorrent(doc.Top, analysisBytes)
	if analyzeErr != nil {
		return analyzeErr
	}
	newV1, newV2 := newAnalysis.infoHashes(analysisBytes)

	if !o.quiet {
		if oldV1 != "" {
			fmt.Fprintf(os.Stderr, "info hash v1: %s", oldV1)
			if newV1 != oldV1 {
				fmt.Fprintf(os.Stderr, " -> %s (changed)", newV1)
			} else {
				fmt.Fprintf(os.Stderr, " (unchanged)")
			}
			fmt.Fprintln(os.Stderr)
		}
		if oldV2 != "" {
			fmt.Fprintf(os.Stderr, "info hash v2: %s", oldV2)
			if newV2 != oldV2 {
				fmt.Fprintf(os.Stderr, " -> %s (changed)", newV2)
			} else {
				fmt.Fprintf(os.Stderr, " (unchanged)")
			}
			fmt.Fprintln(os.Stderr)
		}
		if o.verbose && len(newAnalysis.Warnings) > 0 {
			for _, warning := range newAnalysis.Warnings {
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
	written, err := writeOutput(encoded, paths[0], o.output, o.inPlace, doc.Mode)
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
