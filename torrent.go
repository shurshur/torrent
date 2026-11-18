package main

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Torrent analysis

type fileEntry struct {
	Path       []string
	Length     int64
	Attr       string
	Padding    bool
	Offset     int64
	PiecesRoot []byte
	SHA1       []byte
}

type trackerSet struct {
	Announce        string
	HasAnnounce     bool
	Tiers           [][]string
	InvalidEntries  int
	InvalidAnnounce bool
}

type torrentAnalysis struct {
	Top      *bValue
	Info     *bValue
	Name     string
	NameRaw  []byte
	PieceLen int64

	MetaVersion int64
	V1          bool
	V2          bool
	Future      bool
	SingleFile  bool

	Pieces      []byte
	V1Files     []fileEntry
	V2Files     []fileEntry
	PieceLayers *bValue

	TotalSize        int64
	TotalWithPadding int64
	PieceCountV1     int
	PieceCountV2     int

	RawInfo []byte

	Private      *bool
	Source       string
	CreationDate *int64
	CreatedBy    string
	Comment      string
	Encoding     string
	URLList      []string

	Trackers trackerSet
	Warnings []string
}

var knownTopKeys = map[string]bool{
	"announce":      true,
	"announce-list": true,
	"info":          true,
	"piece layers":  true,
	"creation date": true,
	"created by":    true,
	"comment":       true,
	"encoding":      true,
	"url-list":      true,
}

var knownInfoKeys = map[string]bool{
	"name":         true,
	"piece length": true,
	"pieces":       true,
	"length":       true,
	"files":        true,
	"file tree":    true,
	"meta version": true,
	"private":      true,
	"source":       true,
	"attr":         true,
	"sha1":         true,
	"symlink path": true,
}

func cleanText(b []byte) string {
	return strings.ToValidUTF8(string(b), "\uFFFD")
}

func valueBytes(v *bValue) []byte {
	if v == nil || v.kind != kindBytes {
		return nil
	}
	return v.b
}

func valueString(v *bValue) string {
	return cleanText(valueBytes(v))
}

func valueInt(v *bValue) (int64, bool) {
	if v == nil || v.kind != kindInt {
		return 0, false
	}
	return v.i, true
}

func analyzeTorrent(top *bValue, rawInfo []byte) (*torrentAnalysis, error) {
	if top == nil || top.kind != kindDict {
		return nil, errors.New("metainfo is not a dictionary")
	}
	info := top.d.get("info")
	if info == nil || info.kind != kindDict {
		return nil, errors.New("metainfo has no info dictionary")
	}
	a := &torrentAnalysis{
		Top:      top,
		Info:     info,
		RawInfo:  append([]byte(nil), rawInfo...),
		PieceLen: 0,
	}
	a.NameRaw = valueBytes(info.d.get("name"))
	a.Name = cleanText(a.NameRaw)
	if pl, ok := valueInt(info.d.get("piece length")); ok {
		a.PieceLen = pl
	}
	if mv, ok := valueInt(info.d.get("meta version")); ok {
		a.MetaVersion = mv
	} else {
		a.MetaVersion = 1
	}
	if a.MetaVersion > 2 {
		a.Future = true
	}

	hasFileTree := info.d.get("file tree") != nil
	hasPieces := info.d.get("pieces") != nil
	hasV1Files := info.d.get("files") != nil || info.d.get("length") != nil
	a.V1 = hasPieces || hasV1Files
	a.V2 = hasFileTree || a.MetaVersion >= 2

	if a.V1 {
		files, single, err := parseV1Files(info)
		if err != nil {
			return nil, err
		}
		a.V1Files = files
		a.SingleFile = single
		a.Pieces = valueBytes(info.d.get("pieces"))
	}
	if a.V2 {
		files, err := parseV2Files(info)
		if err != nil {
			return nil, err
		}
		a.V2Files = files
		a.PieceLayers = top.d.get("piece layers")
	}
	if a.V1 && a.V2 {
		mergeV1V2(a)
	}
	if a.V2 && !a.V1 && len(a.V2Files) == 1 && len(a.V2Files[0].Path) == 1 && bytes.Equal([]byte(a.V2Files[0].Path[0]), a.NameRaw) {
		// BEP 52 represents a single-file v2 torrent as {name: {length...}}.
		a.SingleFile = true
	}

	a.computeTotals()
	a.Trackers = parseTrackers(top)

	a.Source = valueString(info.d.get("source"))
	if pv := info.d.get("private"); pv != nil && pv.kind == kindInt {
		b := pv.i != 0
		a.Private = &b
	}
	if cd, ok := valueInt(top.d.get("creation date")); ok {
		a.CreationDate = &cd
	}
	a.CreatedBy = valueString(top.d.get("created by"))
	a.Comment = valueString(top.d.get("comment"))
	a.Encoding = valueString(top.d.get("encoding"))
	a.URLList = parseStringOrList(top.d.get("url-list"))

	a.warnings()
	return a, nil
}

func parseV1Files(info *bValue) ([]fileEntry, bool, error) {
	name := valueString(info.d.get("name"))

	if lv := info.d.get("length"); lv != nil {
		if lv.kind != kindInt {
			return nil, true, errors.New("info.length is not an integer")
		}
		path := []string{}
		if name != "" {
			path = []string{name}
		}
		fe := fileEntry{
			Path:   path,
			Length: lv.i,
			Attr:   valueString(info.d.get("attr")),
			SHA1:   valueBytes(info.d.get("sha1")),
		}
		fe.Padding = strings.Contains(fe.Attr, "p")
		return []fileEntry{fe}, true, nil
	}

	filesVal := info.d.get("files")
	if filesVal == nil {
		return nil, false, nil
	}
	if filesVal.kind != kindList {
		return nil, false, errors.New("info.files is not a list")
	}
	out := make([]fileEntry, 0, len(filesVal.l))
	var offset int64
	for i, fv := range filesVal.l {
		if fv.kind != kindDict {
			return nil, false, fmt.Errorf("info.files[%d] is not a dictionary", i)
		}
		length, ok := valueInt(fv.d.get("length"))
		if !ok {
			return nil, false, fmt.Errorf("info.files[%d].length is missing or invalid", i)
		}
		attr := valueString(fv.d.get("attr"))
		pathVal := fv.d.get("path")
		var path []string
		if pathVal == nil || pathVal.kind != kindList {
			// BEP 47 allows padding and symlink entries to omit path.
			if !strings.Contains(attr, "p") && !strings.Contains(attr, "l") && length != 0 {
				return nil, false, fmt.Errorf("info.files[%d].path is missing or invalid", i)
			}
		} else {
			path = make([]string, 0, len(pathVal.l))
			for _, pv := range pathVal.l {
				if pv.kind != kindBytes {
					return nil, false, fmt.Errorf("info.files[%d].path contains a non-string component", i)
				}
				path = append(path, string(pv.b))
			}
		}
		fe := fileEntry{
			Path:   path,
			Length: length,
			Attr:   attr,
			Offset: offset,
			SHA1:   valueBytes(fv.d.get("sha1")),
		}
		fe.Padding = strings.Contains(fe.Attr, "p")
		out = append(out, fe)
		offset += length
	}
	return out, false, nil
}

func parseV2Files(info *bValue) ([]fileEntry, error) {
	tree := info.d.get("file tree")
	if tree == nil {
		return nil, nil
	}
	if tree.kind != kindDict {
		return nil, errors.New("info.file tree is not a dictionary")
	}
	var out []fileEntry
	var walk func(d *bValue, prefix []string, depth int) error
	walk = func(d *bValue, prefix []string, depth int) error {
		if depth > 256 {
			return errors.New("file tree is too deep")
		}
		for _, key := range d.d.keys {
			child := d.d.get(string(key))
			if child.kind != kindDict {
				return fmt.Errorf("file tree entry %q is not a dictionary", cleanText(key))
			}
			if len(key) == 0 {
				length, ok := valueInt(child.d.get("length"))
				if !ok {
					continue
				}
				fe := fileEntry{
					Path:       append([]string(nil), prefix...),
					Length:     length,
					Attr:       valueString(child.d.get("attr")),
					PiecesRoot: valueBytes(child.d.get("pieces root")),
					SHA1:       valueBytes(child.d.get("sha1")),
				}
				fe.Padding = strings.Contains(fe.Attr, "p")
				out = append(out, fe)
				continue
			}
			next := append(append([]string(nil), prefix...), string(key))
			if err := walk(child, next, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(tree, nil, 0); err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool {
		return lessPath(out[i].Path, out[j].Path)
	})
	return out, nil
}

func mergeV1V2(a *torrentAnalysis) {
	v2ByPath := make(map[string]*fileEntry, len(a.V2Files))
	v2PathSet := make(map[string]bool, len(a.V2Files))
	for i := range a.V2Files {
		key := strings.Join(a.V2Files[i].Path, "/")
		v2ByPath[key] = &a.V2Files[i]
		v2PathSet[key] = true
	}
	v1PathSet := make(map[string]bool, len(a.V1Files))
	for i := range a.V1Files {
		if a.V1Files[i].Padding {
			continue
		}
		key := strings.Join(a.V1Files[i].Path, "/")
		v1PathSet[key] = true
		if v2, ok := v2ByPath[key]; ok {
			a.V1Files[i].PiecesRoot = v2.PiecesRoot
			if len(a.V1Files[i].SHA1) == 0 {
				a.V1Files[i].SHA1 = v2.SHA1
			}
			delete(v2ByPath, key)
		}
	}
	for key := range v2ByPath {
		a.Warnings = append(a.Warnings, fmt.Sprintf("v2 file %q is not present in the v1 file list", cleanText([]byte(key))))
	}
	for key := range v1PathSet {
		if !v2PathSet[key] {
			a.Warnings = append(a.Warnings, fmt.Sprintf("v1 file %q is not present in the v2 file tree", cleanText([]byte(key))))
		}
	}
}

func (a *torrentAnalysis) computeTotals() {
	if a.V1 {
		for _, f := range a.V1Files {
			a.TotalWithPadding += f.Length
			if !f.Padding {
				a.TotalSize += f.Length
			}
		}
		a.PieceCountV1 = len(a.Pieces) / 20
	} else {
		for _, f := range a.V2Files {
			a.TotalSize += f.Length
		}
		a.TotalWithPadding = a.TotalSize
	}
	if a.PieceLen > 0 {
		for _, f := range a.V2Files {
			if f.Length > 0 {
				a.PieceCountV2 += int((f.Length + a.PieceLen - 1) / a.PieceLen)
			}
		}
	}
}

func parseStringOrList(v *bValue) []string {
	if v == nil {
		return nil
	}
	switch v.kind {
	case kindBytes:
		return []string{cleanText(v.b)}
	case kindList:
		var out []string
		for _, item := range v.l {
			if item.kind == kindBytes {
				out = append(out, cleanText(item.b))
			}
		}
		return out
	}
	return nil
}

func parseTrackers(top *bValue) trackerSet {
	var ts trackerSet
	if av := top.d.get("announce"); av != nil {
		if av.kind == kindBytes {
			ts.Announce = cleanText(av.b)
			ts.HasAnnounce = true
		} else {
			ts.InvalidAnnounce = true
		}
	}
	al := top.d.get("announce-list")
	if al == nil {
		return ts
	}
	if al.kind != kindList {
		ts.InvalidEntries++
		return ts
	}
	for _, tierVal := range al.l {
		switch tierVal.kind {
		case kindList:
			var tier []string
			for _, uv := range tierVal.l {
				if uv.kind != kindBytes {
					ts.InvalidEntries++
					continue
				}
				if len(uv.b) > 0 {
					tier = append(tier, cleanText(uv.b))
				}
			}
			if len(tier) > 0 {
				ts.Tiers = append(ts.Tiers, tier)
			}
		case kindBytes:
			// Some non-conforming torrents use a flat list of URLs.
			if len(tierVal.b) > 0 {
				ts.Tiers = append(ts.Tiers, []string{cleanText(tierVal.b)})
			}
		default:
			ts.InvalidEntries++
		}
	}
	return ts
}

func (a *torrentAnalysis) warnings() {
	if len(a.NameRaw) == 0 {
		a.Warnings = append(a.Warnings, "info.name is missing or empty")
	}
	if !a.Trackers.HasAnnounce && len(a.Trackers.Tiers) == 0 {
		a.Warnings = append(a.Warnings, "no announce or announce-list (trackerless torrent; DHT/PEX may still work)")
	} else if !a.Trackers.HasAnnounce && len(a.Trackers.Tiers) > 0 {
		// Perfectly valid: tracker URLs can be only in announce-list.
	}
	if a.Trackers.InvalidAnnounce {
		a.Warnings = append(a.Warnings, "announce is not a bencoded string")
	}
	if a.Trackers.InvalidEntries > 0 {
		a.Warnings = append(a.Warnings, fmt.Sprintf("announce-list contains %d invalid entr%s", a.Trackers.InvalidEntries, plural(a.Trackers.InvalidEntries)))
	}
	if a.MetaVersion > 2 {
		a.Warnings = append(a.Warnings, fmt.Sprintf("unsupported meta version %d; output may be incomplete", a.MetaVersion))
	}
	if a.V2 && a.Info.d.get("meta version") == nil {
		a.Warnings = append(a.Warnings, "v2 torrent has no info.meta version")
	}
	if a.V2 && a.Info.d.get("file tree") == nil {
		a.Warnings = append(a.Warnings, "v2 torrent has no info.file tree")
	}
	if a.V1 && a.Info.d.get("pieces") == nil {
		a.Warnings = append(a.Warnings, "v1 torrent has no info.pieces")
	}
	if a.V1 && len(a.Pieces)%20 != 0 {
		a.Warnings = append(a.Warnings, fmt.Sprintf("info.pieces length %d is not divisible by 20", len(a.Pieces)))
	}
	if a.PieceLen <= 0 {
		a.Warnings = append(a.Warnings, "info.piece length is missing or invalid")
	} else if a.V2 {
		if a.PieceLen < blockSize || a.PieceLen&(a.PieceLen-1) != 0 {
			a.Warnings = append(a.Warnings, fmt.Sprintf("v2 piece length %d is not a power of two >= %d", a.PieceLen, blockSize))
		}
	}
	if a.V1 && a.PieceLen > 0 {
		want := int((a.TotalWithPadding + a.PieceLen - 1) / a.PieceLen)
		if want != a.PieceCountV1 {
			a.Warnings = append(a.Warnings, fmt.Sprintf("v1 piece count is %d, expected %d from file lengths", a.PieceCountV1, want))
		}
	}
	for _, f := range a.allFiles() {
		for _, part := range f.Path {
			if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "/\x00") {
				a.Warnings = append(a.Warnings, fmt.Sprintf("unsafe or unusual path component %q in %q", cleanText([]byte(part)), cleanText([]byte(strings.Join(f.Path, "/")))))
				break
			}
		}
	}
	a.validateV2()
}

func (a *torrentAnalysis) allFiles() []fileEntry {
	if a.V1 {
		return a.V1Files
	}
	return a.V2Files
}

func (a *torrentAnalysis) displayFiles() []fileEntry {
	if a.V1 {
		return a.V1Files
	}
	return a.V2Files
}

func (a *torrentAnalysis) validateV2() {
	if !a.V2 {
		return
	}
	layerSet := make(map[string]bool)
	if a.PieceLayers != nil && a.PieceLayers.kind == kindDict {
		for _, k := range a.PieceLayers.d.keys {
			layerSet[string(k)] = true
		}
	}
	roots := make(map[string]bool)
	for _, f := range a.V2Files {
		path := cleanText([]byte(strings.Join(f.Path, "/")))
		if f.Length == 0 {
			if len(f.PiecesRoot) > 0 {
				a.Warnings = append(a.Warnings, fmt.Sprintf("empty v2 file %q has a pieces root", path))
			}
			continue
		}
		if len(f.PiecesRoot) != sha256.Size {
			a.Warnings = append(a.Warnings, fmt.Sprintf("v2 file %q has a missing or invalid pieces root", path))
			continue
		}
		roots[string(f.PiecesRoot)] = true
		if a.PieceLen <= 0 || f.Length <= a.PieceLen {
			continue
		}
		if a.PieceLayers == nil || a.PieceLayers.kind != kindDict {
			a.Warnings = append(a.Warnings, fmt.Sprintf("v2 file %q is larger than one piece but there is no piece layers dictionary", path))
			continue
		}
		layer := a.PieceLayers.d.get(string(f.PiecesRoot))
		if layer == nil || layer.kind != kindBytes {
			a.Warnings = append(a.Warnings, fmt.Sprintf("v2 file %q has no piece layer", path))
			continue
		}
		want := int((f.Length+a.PieceLen-1)/a.PieceLen) * sha256.Size
		if len(layer.b) != want {
			a.Warnings = append(a.Warnings, fmt.Sprintf("v2 file %q piece layer is %d bytes, expected %d", path, len(layer.b), want))
		}
	}
	for root := range layerSet {
		if !roots[root] {
			a.Warnings = append(a.Warnings, fmt.Sprintf("piece layer for unknown file root %s", hex.EncodeToString([]byte(root))))
		}
	}
}

func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

func (a *torrentAnalysis) versions() []string {
	var out []string
	if a.V1 {
		out = append(out, "v1")
	}
	if a.V2 {
		out = append(out, "v2")
	}
	return out
}

func (a *torrentAnalysis) infoHashes(rawInfo []byte) (string, string) {
	if len(rawInfo) == 0 {
		rawInfo = encodeBencode(a.Info)
	}
	v1, v2 := "", ""
	if a.V1 {
		h := sha1.Sum(rawInfo)
		v1 = hex.EncodeToString(h[:])
	}
	if a.V2 {
		h := sha256.Sum256(rawInfo)
		v2 = hex.EncodeToString(h[:])
	}
	return v1, v2
}

func (a *torrentAnalysis) allTrackers() []string {
	seen := make(map[string]bool)
	var out []string
	add := func(u string) {
		if u == "" || seen[u] {
			return
		}
		seen[u] = true
		out = append(out, u)
	}
	if a.Trackers.HasAnnounce {
		add(a.Trackers.Announce)
	}
	for _, tier := range a.Trackers.Tiers {
		for _, u := range tier {
			add(u)
		}
	}
	return out
}

func (a *torrentAnalysis) jsonMetadata(file string, includeExtra, includeRaw bool) map[string]any {
	obj := make(map[string]any)
	obj["file"] = file
	obj["name"] = cleanText(a.NameRaw)
	obj["version"] = a.versions()
	obj["v1"] = a.V1
	obj["v2"] = a.V2
	obj["meta_version"] = a.MetaVersion
	obj["single_file"] = a.SingleFile
	obj["piece_length"] = a.PieceLen
	obj["total_size"] = a.TotalSize
	if a.TotalWithPadding != a.TotalSize {
		obj["total_size_with_padding"] = a.TotalWithPadding
	}

	v1hash, v2hash := a.infoHashes(a.RawInfo)
	if v1hash != "" {
		obj["info_hash_v1"] = v1hash
		obj["piece_count_v1"] = a.PieceCountV1
	}
	if v2hash != "" {
		obj["info_hash_v2"] = v2hash
		obj["piece_count_v2"] = a.PieceCountV2
	}
	if a.V1 {
		obj["piece_count"] = a.PieceCountV1
	} else if a.V2 {
		obj["piece_count"] = a.PieceCountV2
	}

	if a.Private != nil {
		obj["private"] = *a.Private
	}
	if a.Source != "" {
		obj["source"] = a.Source
	}
	if a.CreationDate != nil {
		obj["creation_date"] = *a.CreationDate
		obj["creation_date_rfc3339"] = time.Unix(*a.CreationDate, 0).UTC().Format(time.RFC3339)
	}
	if a.CreatedBy != "" {
		obj["created_by"] = a.CreatedBy
	}
	if a.Comment != "" {
		obj["comment"] = a.Comment
	}
	if a.Encoding != "" {
		obj["encoding"] = a.Encoding
	}
	if len(a.URLList) > 0 {
		obj["url_list"] = a.URLList
	}

	allTrackers := a.allTrackers()
	if allTrackers == nil {
		allTrackers = []string{}
	}
	tiers := a.Trackers.Tiers
	if tiers == nil {
		tiers = [][]string{}
	}
	trackers := map[string]any{
		"tiers": tiers,
		"all":   allTrackers,
	}
	if a.Trackers.HasAnnounce {
		trackers["announce"] = a.Trackers.Announce
	} else {
		trackers["announce"] = nil
	}
	obj["trackers"] = trackers

	files := make([]any, 0, len(a.displayFiles()))
	padCount := 0
	for _, f := range a.displayFiles() {
		m := map[string]any{
			"path":   cleanText([]byte(strings.Join(f.Path, "/"))),
			"length": f.Length,
		}
		if f.Padding {
			padCount++
			m["padding"] = true
		}
		if f.Attr != "" {
			m["attr"] = f.Attr
		}
		if a.V1 && !f.Padding {
			m["offset"] = f.Offset
		}
		if len(f.SHA1) > 0 {
			if len(f.SHA1) == sha1.Size {
				m["sha1"] = hex.EncodeToString(f.SHA1)
			} else {
				m["sha1"] = map[string]any{"$base64": base64.StdEncoding.EncodeToString(f.SHA1)}
			}
		}
		if a.V2 && len(f.PiecesRoot) == sha256.Size {
			m["pieces_root"] = hex.EncodeToString(f.PiecesRoot)
		}
		files = append(files, m)
	}
	obj["files"] = files
	obj["file_count"] = len(files) - padCount
	obj["padding_file_count"] = padCount

	if a.PieceLayers != nil && a.PieceLayers.kind == kindDict {
		hashes := 0
		for _, k := range a.PieceLayers.d.keys {
			if v := a.PieceLayers.d.get(string(k)); v != nil && v.kind == kindBytes {
				hashes += len(v.b) / sha256.Size
			}
		}
		obj["piece_layers"] = map[string]any{
			"count":  a.PieceLayers.d.length(),
			"hashes": hashes,
		}
	}

	if includeExtra {
		extra := map[string]any{}
		topExtra := map[string]any{}
		for _, k := range a.Top.d.keys {
			if !knownTopKeys[string(k)] {
				topExtra[jsonKeyName(k)] = bValueToJSON(a.Top.d.get(string(k)))
			}
		}
		infoExtra := map[string]any{}
		for _, k := range a.Info.d.keys {
			if !knownInfoKeys[string(k)] {
				infoExtra[jsonKeyName(k)] = bValueToJSON(a.Info.d.get(string(k)))
			}
		}
		if len(topExtra) > 0 {
			extra["top"] = topExtra
		}
		if len(infoExtra) > 0 {
			extra["info"] = infoExtra
		}
		if len(extra) > 0 {
			obj["extra"] = extra
		}
	}

	if len(a.Warnings) > 0 {
		obj["warnings"] = a.Warnings
	} else {
		obj["warnings"] = []string{}
	}

	if includeRaw {
		obj["bencode"] = bValueToJSON(a.Top)
	}
	return obj
}
