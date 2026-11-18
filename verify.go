package main

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
)

// ---------------------------------------------------------------------------
// Verification

type verifySection struct {
	OK            bool     `json:"ok"`
	PieceCount    int      `json:"piece_count,omitempty"`
	CheckedPieces int      `json:"checked_pieces,omitempty"`
	CheckedFiles  int      `json:"checked_files,omitempty"`
	Errors        []string `json:"errors"`
}

type verifyResult struct {
	File string         `json:"file"`
	OK   bool           `json:"ok"`
	V1   *verifySection `json:"v1,omitempty"`
	V2   *verifySection `json:"v2,omitempty"`
}

func addVerifyError(s *verifySection, format string, args ...any) {
	const maxErrors = 50
	if len(s.Errors) < maxErrors {
		s.Errors = append(s.Errors, fmt.Sprintf(format, args...))
	} else if len(s.Errors) == maxErrors {
		s.Errors = append(s.Errors, "further errors omitted")
	}
	s.OK = false
}

func verifyV1(a *torrentAnalysis, dataDir string) *verifySection {
	s := &verifySection{OK: true, PieceCount: a.PieceCountV1, Errors: []string{}}
	if a.PieceLen <= 0 {
		addVerifyError(s, "invalid piece length %d", a.PieceLen)
		return s
	}
	expected := a.Pieces
	if len(expected)%sha1.Size != 0 {
		addVerifyError(s, "info.pieces length %d is not divisible by %d", len(expected), sha1.Size)
		expected = expected[:len(expected)/sha1.Size*sha1.Size]
	}
	ph := newPieceHasher(a.PieceLen)
	for _, f := range a.V1Files {
		if f.Padding {
			ph.WriteZeros(f.Length)
			continue
		}
		if f.Length == 0 {
			continue
		}
		s.CheckedFiles++
		path, err := v1DataPath(a, dataDir, f)
		if err != nil {
			addVerifyError(s, "%s: %v", cleanText([]byte(strings.Join(f.Path, "/"))), err)
			ph.WriteZeros(f.Length)
			continue
		}
		if err := feedFileForVerify(ph, path, f.Length); err != nil {
			addVerifyError(s, "%s: %v", cleanText([]byte(strings.Join(f.Path, "/"))), err)
		}
	}
	actual := ph.Finish()
	s.CheckedPieces = len(actual) / sha1.Size
	expCount := len(expected) / sha1.Size
	if s.CheckedPieces != expCount {
		addVerifyError(s, "piece count is %d, expected %d", s.CheckedPieces, expCount)
	}
	n := expCount
	if len(actual)/sha1.Size < n {
		n = len(actual) / sha1.Size
	}
	mismatches := 0
	for i := 0; i < n; i++ {
		if !bytes.Equal(expected[i*sha1.Size:(i+1)*sha1.Size], actual[i*sha1.Size:(i+1)*sha1.Size]) {
			if mismatches < 10 {
				addVerifyError(s, "piece %d does not match", i)
			}
			mismatches++
		}
	}
	if mismatches > 10 {
		addVerifyError(s, "%d pieces do not match in total", mismatches)
	}
	if len(s.Errors) == 0 {
		s.OK = true
	}
	return s
}

func verifyV2(a *torrentAnalysis, dataDir string) *verifySection {
	s := &verifySection{OK: true, PieceCount: a.PieceCountV2, Errors: []string{}}
	if a.PieceLen <= 0 {
		addVerifyError(s, "invalid piece length %d", a.PieceLen)
		return s
	}
	for _, f := range a.V2Files {
		display := cleanText([]byte(strings.Join(f.Path, "/")))
		if f.Length == 0 {
			continue
		}
		path, err := v2DataPath(a, dataDir, f)
		if err != nil {
			addVerifyError(s, "%s: %v", display, err)
			continue
		}
		st, err := os.Stat(path)
		if err != nil {
			addVerifyError(s, "%s: %v", display, err)
			continue
		}
		if st.Size() != f.Length {
			addVerifyError(s, "%s: size is %d, expected %d", display, st.Size(), f.Length)
		}
		h, err := v2FileHashes(path, a.PieceLen)
		if err != nil {
			addVerifyError(s, "%s: %v", display, err)
			continue
		}
		s.CheckedFiles++
		s.CheckedPieces += len(h.PieceRoots)
		if h.Length != f.Length {
			addVerifyError(s, "%s: read %d bytes, expected %d", display, h.Length, f.Length)
		}
		if len(f.PiecesRoot) != sha256.Size {
			addVerifyError(s, "%s: invalid or missing pieces root", display)
		} else if !h.HasRoot || !bytes.Equal(h.Root[:], f.PiecesRoot) {
			addVerifyError(s, "%s: pieces root does not match", display)
		}
		if f.Length > a.PieceLen {
			var expectedLayer []byte
			if a.PieceLayers != nil && a.PieceLayers.kind == kindDict {
				if lv := a.PieceLayers.d.get(string(f.PiecesRoot)); lv != nil && lv.kind == kindBytes {
					expectedLayer = lv.b
				}
			}
			if expectedLayer == nil {
				addVerifyError(s, "%s: piece layer is missing", display)
			} else if !bytes.Equal(h.Layer, expectedLayer) {
				addVerifyError(s, "%s: piece layer does not match", display)
			}
		}
	}
	if len(s.Errors) == 0 {
		s.OK = true
	}
	return s
}

func cmdVerify(args []string) error {
	fs := flag.NewFlagSet("verify [options] FILE...", flag.ContinueOnError)
	dataDir := fs.String("data", ".", "directory that contains the torrent's top-level name")
	jsonOut := fs.Bool("json", false, "output JSON")
	fs.BoolVar(jsonOut, "j", false, "output JSON (shorthand)")
	quiet := fs.Bool("quiet", false, "suppress successful per-file output")
	verbose := fs.Bool("verbose", false, "show more detailed errors")
	help := fs.Bool("help", false, "show help")
	isBool := boolFlags("json", "j", "quiet", "verbose", "help")
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
		return errors.New("verify: at least one torrent file is required")
	}
	var results []verifyResult
	failed := false
	for _, path := range paths {
		doc, err := loadTorrentDoc(path)
		if err != nil {
			return err
		}
		a, err := analyzeTorrent(doc.Top, doc.RawInfo)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		res := verifyResult{File: path, OK: true}
		if a.V1 {
			res.V1 = verifyV1(a, *dataDir)
			if !res.V1.OK {
				res.OK = false
			}
		}
		if a.V2 {
			res.V2 = verifyV2(a, *dataDir)
			if !res.V2.OK {
				res.OK = false
			}
		}
		if !a.V1 && !a.V2 {
			return fmt.Errorf("%s: unsupported torrent (neither v1 nor v2)", path)
		}
		if !res.OK {
			failed = true
		}
		results = append(results, res)
		if !*jsonOut && (!*quiet || !res.OK) {
			printVerifyResult(res, *verbose)
		}
	}
	if *jsonOut {
		var value any
		if len(results) == 1 {
			value = results[0]
		} else {
			value = results
		}
		data, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			return err
		}
		data = append(data, '\n')
		if _, err := os.Stdout.Write(data); err != nil {
			return err
		}
	}
	if failed {
		return &exitError{code: 2, err: errors.New("verification failed")}
	}
	return nil
}

func printVerifyResult(res verifyResult, verbose bool) {
	if res.OK {
		fmt.Printf("%s: OK\n", res.File)
	} else {
		fmt.Printf("%s: FAILED\n", res.File)
	}
	for _, section := range []struct {
		name string
		s    *verifySection
	}{{"v1", res.V1}, {"v2", res.V2}} {
		if section.s == nil {
			continue
		}
		status := "OK"
		if !section.s.OK {
			status = "FAILED"
		}
		extra := ""
		if section.s.CheckedFiles > 0 {
			extra = fmt.Sprintf(", %d files", section.s.CheckedFiles)
		}
		fmt.Printf("  %s: %s (%d pieces%s)\n", section.name, status, section.s.CheckedPieces, extra)
		limit := len(section.s.Errors)
		if !verbose && limit > 10 {
			limit = 10
		}
		for i := 0; i < limit; i++ {
			fmt.Printf("    - %s\n", section.s.Errors[i])
		}
		if limit < len(section.s.Errors) {
			fmt.Printf("    ... (%d more errors)\n", len(section.s.Errors)-limit)
		}
	}
}
