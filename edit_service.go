package main

import (
	"errors"
	"fmt"
	"strconv"
)

// editResult is the outcome of applying edit operations to an in-memory
// torrent document. It deliberately contains no CLI or file-system details so
// that both the CLI and the web UI can use it.
type editResult struct {
	Encoded     []byte
	OldV1       string
	OldV2       string
	NewV1       string
	NewV2       string
	InfoChanged bool
	Warnings    []string
	Operations  []string
}

// applyEdit applies the requested operations to doc and returns the encoded
// result. The caller decides whether to write it to a file, stdout or a web
// session.
func applyEdit(doc *torrentDoc, o *editOptions) (*editResult, error) {
	if o.private && o.public {
		return nil, errors.New("--private and --public are mutually exclusive")
	}
	oldAnalysis, err := analyzeTorrent(doc.Top, doc.RawInfo)
	if err != nil {
		return nil, err
	}
	res := &editResult{}
	res.OldV1, res.OldV2 = oldAnalysis.infoHashes(doc.RawInfo)

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
		if changed {
			res.Operations = append(res.Operations, desc)
		}
		return nil
	}

	didOp := false
	for _, item := range o.sets {
		pathStr, valStr, err := splitKV(item)
		if err != nil {
			return nil, fmt.Errorf("--set %q: %w", item, err)
		}
		elems, err := parsePath(pathStr)
		if err != nil {
			return nil, err
		}
		val, err := parseAutoValue(valStr)
		if err != nil {
			return nil, fmt.Errorf("--set %s: %w", pathStr, err)
		}
		if err := apply("set "+pathStr, elems, func() (bool, error) { return setPath(doc.Top, elems, val) }); err != nil {
			return nil, err
		}
		didOp = true
	}
	for _, item := range o.setInts {
		pathStr, valStr, err := splitKV(item)
		if err != nil {
			return nil, fmt.Errorf("--set-int %q: %w", item, err)
		}
		elems, err := parsePath(pathStr)
		if err != nil {
			return nil, err
		}
		n, err := strconv.ParseInt(valStr, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("--set-int %s: %w", pathStr, err)
		}
		val := newInt(n)
		if err := apply("set-int "+pathStr, elems, func() (bool, error) { return setPath(doc.Top, elems, val) }); err != nil {
			return nil, err
		}
		didOp = true
	}
	for _, item := range o.setJSONs {
		pathStr, valStr, err := splitKV(item)
		if err != nil {
			return nil, fmt.Errorf("--set-json %q: %w", item, err)
		}
		elems, err := parsePath(pathStr)
		if err != nil {
			return nil, err
		}
		val, err := parseJSONValue([]byte(valStr))
		if err != nil {
			return nil, fmt.Errorf("--set-json %s: %w", pathStr, err)
		}
		if err := apply("set-json "+pathStr, elems, func() (bool, error) { return setPath(doc.Top, elems, val) }); err != nil {
			return nil, err
		}
		didOp = true
	}
	for _, item := range o.setBencodes {
		pathStr, valStr, err := splitKV(item)
		if err != nil {
			return nil, fmt.Errorf("--set-bencode %q: %w", item, err)
		}
		elems, err := parsePath(pathStr)
		if err != nil {
			return nil, err
		}
		val, err := parseBencode([]byte(valStr))
		if err != nil {
			return nil, fmt.Errorf("--set-bencode %s: %w", pathStr, err)
		}
		if err := apply("set-bencode "+pathStr, elems, func() (bool, error) { return setPath(doc.Top, elems, val) }); err != nil {
			return nil, err
		}
		didOp = true
	}
	for _, pathStr := range o.unsets {
		elems, err := parsePath(pathStr)
		if err != nil {
			return nil, err
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
			return nil, err
		}
		didOp = true
	}
	for _, item := range o.appends {
		pathStr, valStr, err := splitKV(item)
		if err != nil {
			return nil, fmt.Errorf("--append %q: %w", item, err)
		}
		elems, err := parsePath(pathStr)
		if err != nil {
			return nil, err
		}
		val, err := parseAutoValue(valStr)
		if err != nil {
			return nil, fmt.Errorf("--append %s: %w", pathStr, err)
		}
		if err := apply("append "+pathStr, elems, func() (bool, error) { return appendPath(doc.Top, elems, val) }); err != nil {
			return nil, err
		}
		didOp = true
	}

	if len(o.trackers) > 0 || len(o.removeTrackers) > 0 || o.clearTrackers {
		tiers := cloneTiers(parseTrackers(doc.Top).Tiers)
		added := []string(o.trackers)
		if o.clearTrackers {
			tiers = nil
			added = nil
		} else if o.replaceTrackers && len(added) > 0 {
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
		res.Operations = append(res.Operations, "updated trackers")
		didOp = true
	}
	if o.announce.set {
		if doc.Top.d.get("announce") == nil || string(valueBytes(doc.Top.d.get("announce"))) != o.announce.value {
			doc.Top.d.setString("announce", newString(o.announce.value))
			res.Operations = append(res.Operations, "set announce")
		}
		didOp = true
	}
	if o.unsetAnnounce {
		doc.Top.d.del("announce")
		res.Operations = append(res.Operations, "unset announce")
		didOp = true
	}
	if o.comment.set {
		doc.Top.d.setString("comment", newString(o.comment.value))
		res.Operations = append(res.Operations, "set comment")
		didOp = true
	}
	if o.unsetComment {
		doc.Top.d.del("comment")
		res.Operations = append(res.Operations, "unset comment")
		didOp = true
	}
	if o.source.set {
		info := doc.Top.d.get("info")
		if info == nil || info.kind != kindDict {
			return nil, errors.New("info dictionary is missing")
		}
		if string(valueBytes(info.d.get("source"))) != o.source.value {
			info.d.setString("source", newString(o.source.value))
			doc.InfoDirty = true
			res.Operations = append(res.Operations, "set info.source")
		}
		didOp = true
	}
	if o.unsetSource {
		info := doc.Top.d.get("info")
		if info != nil && info.kind == kindDict {
			if info.d.del("source") {
				doc.InfoDirty = true
				res.Operations = append(res.Operations, "unset info.source")
			}
		}
		didOp = true
	}
	if o.private {
		info := doc.Top.d.get("info")
		if info == nil || info.kind != kindDict {
			return nil, errors.New("info dictionary is missing")
		}
		if pv := info.d.get("private"); pv == nil || pv.kind != kindInt || pv.i != 1 {
			info.d.setString("private", newInt(1))
			doc.InfoDirty = true
			res.Operations = append(res.Operations, "set info.private")
		}
		didOp = true
	}
	if o.public {
		info := doc.Top.d.get("info")
		if info != nil && info.kind == kindDict && info.d.del("private") {
			doc.InfoDirty = true
			res.Operations = append(res.Operations, "unset info.private")
		}
		didOp = true
	}

	if !didOp {
		return nil, errors.New("edit: no operations specified")
	}

	info := doc.Top.d.get("info")
	if info == nil || info.kind != kindDict {
		return nil, errors.New("edit: the torrent must contain an info dictionary")
	}
	doc.Info = info
	encoded, err := doc.encoded()
	if err != nil {
		return nil, err
	}
	analysisBytes := doc.RawInfo
	if doc.InfoDirty {
		analysisBytes = encodeBencode(doc.Info)
	}
	newAnalysis, err := analyzeTorrent(doc.Top, analysisBytes)
	if err != nil {
		return nil, err
	}
	res.NewV1, res.NewV2 = newAnalysis.infoHashes(analysisBytes)
	res.Encoded = encoded
	res.Warnings = newAnalysis.Warnings
	res.InfoChanged = (res.OldV1 != "" && res.OldV1 != res.NewV1) ||
		(res.OldV2 != "" && res.OldV2 != res.NewV2)
	return res, nil
}
