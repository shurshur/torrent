package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ---------------------------------------------------------------------------
// Repair missing v2 piece layers from local data

type repairResult struct {
	Input       string `json:"input"`
	Output      string `json:"output,omitempty"`
	Version     string `json:"version,omitempty"`
	State       string `json:"state,omitempty"`
	Warning     string `json:"warning,omitempty"`
	Error       string `json:"error,omitempty"`
	LayersTotal int    `json:"layers_total,omitempty"`
	LayersAdded int    `json:"layers_added,omitempty"`
	Message     string `json:"-"`
}

type pieceLayerNeed struct {
	file     fileEntry
	root     []byte
	expected int
	oldLayer []byte
	hasOld   bool
}

func cmdRepair(args []string) error {
	fs := flag.NewFlagSet("repair [options] FILE...", flag.ContinueOnError)
	out := fs.String("out", "", "output file or directory (default: <input>.repaired.torrent)")
	fs.StringVar(out, "o", "", "output file or directory (shorthand)")
	fs.StringVar(out, "output", "", "output file or directory (alias)")
	force := fs.Bool("force", false, "overwrite an existing output file")
	fs.BoolVar(force, "f", false, "overwrite an existing output file (shorthand)")
	dataDir := fs.String("data", ".", "directory that contains the torrent's top-level name")
	jsonOut := fs.Bool("json", false, "output JSON")
	fs.BoolVar(jsonOut, "j", false, "output JSON (shorthand)")
	quiet := fs.Bool("quiet", false, "suppress progress and success messages")
	fs.BoolVar(quiet, "q", false, "suppress progress and success messages (shorthand)")
	recompute := fs.Bool("recompute", false, "recompute all piece layers, not only missing ones")
	help := fs.Bool("help", false, "show help")
	isBool := boolFlags("force", "f", "json", "j", "quiet", "q", "recompute", "help")
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
		return errors.New("repair: at least one torrent file is required")
	}
	if *jsonOut && *out == "-" {
		return errors.New("repair: --out - cannot be combined with --json")
	}
	if len(paths) > 1 {
		if *out == "-" {
			return errors.New("repair: --out - can only be used with a single input file")
		}
		if *out != "" && strings.HasSuffix(strings.ToLower(*out), ".torrent") {
			return errors.New("repair: for multiple input files --out must be a directory")
		}
	}

	results := make([]repairResult, 0, len(paths))
	hadError := false
	for i, input := range paths {
		res := repairOne(input, *dataDir, *out, len(paths), *force, *recompute, *quiet, i)
		results = append(results, res)
		if res.Error != "" {
			hadError = true
			fmt.Fprintf(os.Stderr, "[%d/%d] %s: %s\n", i+1, len(paths), input, res.Error)
			continue
		}
		if *jsonOut {
			continue
		}
		if res.Output != "" && res.Output != "-" {
			fmt.Fprintln(os.Stdout, res.Output)
		}
		if !*quiet {
			if res.Output == "-" {
				fmt.Fprintf(os.Stderr, "[%d/%d] %s: written to stdout\n", i+1, len(paths), input)
			} else if res.Output != "" {
				fmt.Fprintf(os.Stderr, "[%d/%d] %s: saved: %s (%d/%d layers added)\n", i+1, len(paths), input, res.Output, res.LayersAdded, res.LayersTotal)
			} else {
				fmt.Fprintf(os.Stderr, "[%d/%d] %s: %s\n", i+1, len(paths), input, res.Message)
			}
			if res.Warning != "" {
				fmt.Fprintf(os.Stderr, "[%d/%d] %s: warning: %s\n", i+1, len(paths), input, res.Warning)
			}
		}
	}

	if *jsonOut {
		data, err := json.MarshalIndent(results, "", "  ")
		if err != nil {
			return err
		}
		data = append(data, '\n')
		if _, err := os.Stdout.Write(data); err != nil {
			return err
		}
	}
	if hadError {
		return &exitError{code: 1}
	}
	return nil
}

func repairOne(input, dataDir, out string, count int, force, recompute, quiet bool, index int) repairResult {
	res := repairResult{Input: input}
	doc, err := loadTorrentDoc(input)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	a, err := analyzeTorrent(doc.Top, doc.RawInfo)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.Version = analysisVersion(a)
	if !a.V2 {
		res.State = "found"
		res.Message = "v1, piece layers are not required"
		return res
	}
	if a.PieceLen < blockSize || a.PieceLen&(a.PieceLen-1) != 0 {
		res.Error = fmt.Sprintf("invalid v2 piece length %d", a.PieceLen)
		return res
	}

	layersPresent := a.PieceLayers != nil && a.PieceLayers.kind == kindDict
	layers := newDictValue()
	oldLayers := make(map[string][]byte)
	var warnings []string
	if layersPresent {
		for _, key := range a.PieceLayers.d.keys {
			v := a.PieceLayers.d.get(string(key))
			layers.d.set(key, v)
			if v != nil && v.kind == kindBytes {
				oldLayers[string(key)] = v.b
			}
		}
	} else if a.PieceLayers != nil {
		warnings = append(warnings, "existing piece layers value is not a dictionary; it was replaced")
	}

	var needs []pieceLayerNeed
	seenRoots := make(map[string]bool)
	rootLengths := make(map[string]int64)
	for _, f := range a.V2Files {
		if f.Length == 0 {
			continue
		}
		if len(f.PiecesRoot) != sha256.Size {
			if f.Length > a.PieceLen {
				res.Error = fmt.Sprintf("%s: invalid or missing pieces root in info; cannot repair without changing the info hash", cleanText([]byte(strings.Join(f.Path, "/"))))
				return res
			}
			warnings = append(warnings, fmt.Sprintf("%s: invalid or missing pieces root", cleanText([]byte(strings.Join(f.Path, "/")))))
			continue
		}
		rootKey := string(f.PiecesRoot)
		if prevLen, ok := rootLengths[rootKey]; ok && prevLen != f.Length {
			res.Error = fmt.Sprintf("pieces root %s is used by files with different lengths", hex.EncodeToString(f.PiecesRoot))
			return res
		}
		rootLengths[rootKey] = f.Length
		if f.Length <= a.PieceLen || seenRoots[rootKey] {
			continue
		}
		seenRoots[rootKey] = true
		res.LayersTotal++
		expected := int((f.Length+a.PieceLen-1)/a.PieceLen) * sha256.Size
		old, hasOld := oldLayers[rootKey]
		if hasOld && len(old) == expected && !recompute {
			continue
		}
		needs = append(needs, pieceLayerNeed{
			file:     f,
			root:     append([]byte(nil), f.PiecesRoot...),
			expected: expected,
			oldLayer: old,
			hasOld:   hasOld,
		})
	}
	if len(warnings) > 0 {
		res.Warning = strings.Join(warnings, "; ")
	}

	changed := !layersPresent
	if len(needs) == 0 && !changed {
		res.State = "found"
		res.Message = "piece layers already complete"
		return res
	}

	if !quiet && len(needs) > 0 {
		fmt.Fprintf(os.Stderr, "[%d/%d] %s: computing %d piece layer(s)...\n", index+1, count, input, len(needs))
	}
	for i, need := range needs {
		path, err := v2DataPath(a, dataDir, need.file)
		if err != nil {
			res.Error = fmt.Sprintf("%s: %v", cleanText([]byte(strings.Join(need.file.Path, "/"))), err)
			return res
		}
		display := cleanText([]byte(strings.Join(need.file.Path, "/")))
		if !quiet {
			fmt.Fprintf(os.Stderr, "[%d/%d] %s: [%d/%d] hashing %s\n", index+1, count, input, i+1, len(needs), path)
		}
		st, err := os.Stat(path)
		if err != nil {
			res.Error = fmt.Sprintf("%s: %v", display, err)
			return res
		}
		if st.Size() != need.file.Length {
			res.Error = fmt.Sprintf("%s: size is %d, expected %d", display, st.Size(), need.file.Length)
			return res
		}
		h, err := v2FileHashes(path, a.PieceLen)
		if err != nil {
			res.Error = fmt.Sprintf("%s: %v", display, err)
			return res
		}
		if h.Length != need.file.Length {
			res.Error = fmt.Sprintf("%s: read %d bytes, expected %d", display, h.Length, need.file.Length)
			return res
		}
		if !h.HasRoot || !bytes.Equal(h.Root[:], need.root) {
			actual := ""
			if h.HasRoot {
				actual = hex.EncodeToString(h.Root[:])
			}
			res.Error = fmt.Sprintf("%s: pieces root mismatch (torrent %s, data %s); data does not match the torrent",
				display, hex.EncodeToString(need.root), actual)
			return res
		}
		if len(h.Layer) != need.expected {
			res.Error = fmt.Sprintf("%s: computed piece layer is %d bytes, expected %d", display, len(h.Layer), need.expected)
			return res
		}
		if need.hasOld && !bytes.Equal(need.oldLayer, h.Layer) {
			warnings = append(warnings, fmt.Sprintf("replaced incorrect piece layer for %s", display))
		}
		if !need.hasOld || !bytes.Equal(need.oldLayer, h.Layer) {
			layers.d.set(need.root, newBytes(h.Layer))
			res.LayersAdded++
			changed = true
		}
	}
	if len(warnings) > 0 {
		res.Warning = strings.Join(warnings, "; ")
	}
	if !changed {
		res.State = "found"
		res.Message = "piece layers already complete"
		return res
	}
	if doc.InfoDirty {
		res.Error = "internal error: info was modified while repairing piece layers"
		return res
	}
	doc.Top.d.setString("piece layers", layers)
	encoded, err := doc.encoded()
	if err != nil {
		res.Error = err.Error()
		return res
	}
	outputPath, err := repairOutputPath(input, out, count, force)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	written, err := writeOutput(encoded, "", outputPath, false, doc.Mode)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.Output = written
	res.State = "found"
	if res.LayersAdded == 0 {
		res.Message = "created empty piece layers dictionary"
	} else {
		res.Message = fmt.Sprintf("repaired %d piece layer(s)", res.LayersAdded)
	}
	return res
}

func analysisVersion(a *torrentAnalysis) string {
	switch {
	case a.V1 && a.V2:
		return "hybrid"
	case a.V2:
		return "v2"
	case a.V1:
		return "v1"
	default:
		return "unknown"
	}
}

func repairOutputPath(input, out string, count int, force bool) (string, error) {
	if out == "-" {
		if count > 1 {
			return "", errors.New("--out - can only be used with a single input file")
		}
		return "-", nil
	}
	base := filepath.Base(input)
	if strings.HasSuffix(strings.ToLower(base), ".torrent") {
		base = base[:len(base)-len(".torrent")]
	}
	stem := base + ".repaired.torrent"

	var path string
	if out == "" {
		path = filepath.Join(filepath.Dir(input), stem)
	} else if count > 1 || strings.HasSuffix(out, string(os.PathSeparator)) || isExistingDir(out) {
		if err := os.MkdirAll(out, 0755); err != nil {
			return "", err
		}
		path = filepath.Join(out, stem)
	} else {
		path = out
	}
	if !force {
		if _, err := os.Stat(path); err == nil {
			return "", fmt.Errorf("output file %s already exists (use --force)", path)
		} else if !os.IsNotExist(err) {
			return "", err
		}
	}
	return path, nil
}

func isExistingDir(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}
