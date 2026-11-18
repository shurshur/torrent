package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

// ---------------------------------------------------------------------------
// JSON export/import

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func cmdExport(args []string) error {
	fs := flag.NewFlagSet("export [options] FILE", flag.ContinueOnError)
	output := fs.String("output", "", "write JSON to FILE (default: stdout)")
	fs.StringVar(output, "o", "", "write JSON to FILE (shorthand)")
	compact := fs.Bool("compact", false, "compact JSON output")
	noRawInfo := fs.Bool("no-raw-info", false, "do not preserve the original raw info bytes")
	quiet := fs.Bool("quiet", false, "suppress the summary")
	help := fs.Bool("help", false, "show help")
	isBool := boolFlags("compact", "no-raw-info", "quiet", "help")
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
		return errors.New("export: exactly one torrent file is required")
	}
	doc, err := loadTorrentDoc(paths[0])
	if err != nil {
		return err
	}
	a, err := analyzeTorrent(doc.Top, doc.RawInfo)
	if err != nil {
		return err
	}
	obj, ok := bValueToJSON(doc.Top).(map[string]any)
	if !ok {
		return errors.New("export: top-level value is not a dictionary")
	}
	meta := map[string]any{
		"format":           "torrenttool-json",
		"version":          1,
		"info_fingerprint": sha256Hex(encodeBencode(doc.Info)),
	}
	v1hash, v2hash := a.infoHashes(doc.RawInfo)
	if v1hash != "" {
		meta["info_hash_v1"] = v1hash
	}
	if v2hash != "" {
		meta["info_hash_v2"] = v2hash
	}
	if !*noRawInfo {
		meta["raw_info"] = base64.StdEncoding.EncodeToString(doc.RawInfo)
	}
	if _, exists := obj["__torrenttool"]; exists {
		fmt.Fprintln(os.Stderr, "warning: input already contains a __torrenttool key; it will be overwritten in the export")
	}
	obj["__torrenttool"] = meta

	var data []byte
	if *compact {
		data, err = json.Marshal(obj)
	} else {
		data, err = json.MarshalIndent(obj, "", "  ")
	}
	if err != nil {
		return err
	}
	data = append(data, '\n')
	written, err := writeOutput(data, "", *output, false, 0644)
	if err != nil {
		return err
	}
	if !*quiet {
		fmt.Fprintf(os.Stderr, "exported %s to %s\n", paths[0], written)
	}
	return nil
}

func cmdImport(args []string) error {
	fs := flag.NewFlagSet("import [options] FILE.json", flag.ContinueOnError)
	output := fs.String("output", "", "write torrent to FILE (use - for stdout)")
	fs.StringVar(output, "o", "", "write torrent to FILE (shorthand)")
	quiet := fs.Bool("quiet", false, "suppress the summary")
	verbose := fs.Bool("verbose", false, "show warnings")
	help := fs.Bool("help", false, "show help")
	isBool := boolFlags("quiet", "verbose", "help")
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
		return errors.New("import: exactly one JSON file is required (use - for stdin)")
	}
	var data []byte
	if paths[0] == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(paths[0])
	}
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var raw any
	if err := dec.Decode(&raw); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("import: more than one JSON value")
		}
		return err
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return errors.New("import: top-level JSON value must be an object")
	}
	var meta map[string]any
	if mv, ok := obj["__torrenttool"].(map[string]any); ok {
		meta = mv
	}
	delete(obj, "__torrenttool")
	topVal, err := jsonToBValue(obj)
	if err != nil {
		return err
	}
	if topVal.kind != kindDict {
		return errors.New("import: top-level value must be a dictionary")
	}
	info := topVal.d.get("info")
	if info == nil || info.kind != kindDict {
		return errors.New("import: JSON has no info dictionary")
	}

	var rawInfo []byte
	if meta != nil {
		if rawStr, ok := meta["raw_info"].(string); ok && rawStr != "" {
			decoded, decErr := base64.StdEncoding.DecodeString(rawStr)
			if decErr != nil {
				return fmt.Errorf("import: invalid raw_info: %w", decErr)
			}
			fingerprint := sha256Hex(encodeBencode(info))
			if fp, ok := meta["info_fingerprint"].(string); ok && fp == fingerprint {
				rawInfo = decoded
			} else if !*quiet {
				fmt.Fprintln(os.Stderr, "notice: info was modified; the original raw info bytes cannot be reused")
			}
		}
	}
	var encoded []byte
	if len(rawInfo) > 0 {
		encoded = encodeTop(topVal, rawInfo, false)
	} else {
		encoded = encodeBencode(topVal)
		rawInfo = encodeBencode(info)
	}
	a, err := analyzeTorrent(topVal, rawInfo)
	if err != nil {
		return err
	}
	if *output == "" && isTerminal(os.Stdout) {
		return errors.New("import: use --output FILE or redirect stdout")
	}
	written, err := writeOutput(encoded, "", *output, false, 0644)
	if err != nil {
		return err
	}
	if !*quiet {
		v1hash, v2hash := a.infoHashes(rawInfo)
		fmt.Fprintf(os.Stderr, "imported to: %s\n", written)
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
