package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
)

func printTorrentInfo(a *torrentAnalysis, file string, showFiles, noExtra, verbose bool) {
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "File:\t%s\n", file)
	fmt.Fprintf(w, "Name:\t%s\n", cleanText(a.NameRaw))
	versionText := strings.Join(a.versions(), "+")
	if versionText == "" {
		versionText = "unknown"
	} else if a.V1 && a.V2 {
		versionText += " (hybrid)"
	}
	if a.Future {
		versionText += fmt.Sprintf(" (meta version %d)", a.MetaVersion)
	}
	fmt.Fprintf(w, "Version:\t%s\n", versionText)
	v1hash, v2hash := a.infoHashes(a.RawInfo)
	if v1hash != "" {
		fmt.Fprintf(w, "Info hash v1:\t%s\n", v1hash)
	}
	if v2hash != "" {
		fmt.Fprintf(w, "Info hash v2:\t%s\n", v2hash)
	}
	if a.PieceLen > 0 {
		fmt.Fprintf(w, "Piece length:\t%d (%s)\n", a.PieceLen, formatSize(a.PieceLen))
	} else {
		fmt.Fprintf(w, "Piece length:\tunknown\n")
	}
	if a.V1 {
		fmt.Fprintf(w, "Pieces v1:\t%d\n", a.PieceCountV1)
	}
	if a.V2 {
		fmt.Fprintf(w, "Pieces v2:\t%d (per-file address space)\n", a.PieceCountV2)
	}
	fmt.Fprintf(w, "Files:\t%d\n", countNonPadding(a.displayFiles()))
	if a.TotalWithPadding != a.TotalSize {
		fmt.Fprintf(w, "Total size:\t%s (%d bytes, including %s padding)\n", formatSize(a.TotalSize), a.TotalSize, formatSize(a.TotalWithPadding-a.TotalSize))
	} else {
		fmt.Fprintf(w, "Total size:\t%s (%d bytes)\n", formatSize(a.TotalSize), a.TotalSize)
	}
	if a.Private != nil {
		if *a.Private {
			fmt.Fprintf(w, "Private:\tyes\n")
		} else {
			fmt.Fprintf(w, "Private:\tno\n")
		}
	}
	if a.CreationDate != nil {
		fmt.Fprintf(w, "Created:\t%s (%d)\n", formatDate(*a.CreationDate), *a.CreationDate)
	}
	if a.CreatedBy != "" {
		fmt.Fprintf(w, "Created by:\t%s\n", a.CreatedBy)
	}
	if a.Comment != "" {
		fmt.Fprintf(w, "Comment:\t%s\n", a.Comment)
	}
	if a.Encoding != "" {
		fmt.Fprintf(w, "Encoding:\t%s\n", a.Encoding)
	}
	if a.Source != "" {
		fmt.Fprintf(w, "Source:\t%s\n", a.Source)
	}
	if len(a.URLList) > 0 {
		fmt.Fprintf(w, "Web seeds:\t%s\n", strings.Join(a.URLList, ", "))
	}
	w.Flush()

	if a.Trackers.HasAnnounce {
		fmt.Printf("announce:\t%s\n", a.Trackers.Announce)
	}
	if len(a.Trackers.Tiers) > 0 {
		for i, tier := range a.Trackers.Tiers {
			fmt.Printf("tier %d:\t%s\n", i+1, strings.Join(tier, ", "))
		}
	}
	if !a.Trackers.HasAnnounce && len(a.Trackers.Tiers) == 0 {
		fmt.Printf("trackers:\tnone (DHT/PEX only)\n")
	}

	files := a.displayFiles()
	if showFiles && len(files) > 0 {
		limit := len(files)
		if !verbose && limit > 30 {
			limit = 30
		}
		fmt.Printf("\nFiles (%d):\n", countNonPadding(files))
		for i := 0; i < limit; i++ {
			f := files[i]
			marker := ""
			if f.Padding {
				marker = " [pad]"
			}
			fmt.Printf("  %s\t%s%s\n", strings.Join(f.Path, "/"), formatSize(f.Length), marker)
		}
		if limit < len(files) {
			fmt.Printf("  ... (%d more; use --files to show all)\n", len(files)-limit)
		}
	}

	if !noExtra {
		var topExtra, infoExtra []string
		for _, k := range a.Top.d.keys {
			if !knownTopKeys[string(k)] {
				topExtra = append(topExtra, string(k))
			}
		}
		for _, k := range a.Info.d.keys {
			if !knownInfoKeys[string(k)] {
				infoExtra = append(infoExtra, string(k))
			}
		}
		if len(topExtra) > 0 || len(infoExtra) > 0 {
			fmt.Printf("\nNon-standard keys:\n")
			for _, k := range topExtra {
				v := a.Top.d.get(k)
				fmt.Printf("  %s = %s\n", k, formatBValue(v, 200))
			}
			for _, k := range infoExtra {
				v := a.Info.d.get(k)
				fmt.Printf("  info.%s = %s\n", k, formatBValue(v, 200))
			}
		}
	}

	if len(a.Warnings) > 0 {
		fmt.Printf("\nWarnings:\n")
		for _, warning := range a.Warnings {
			fmt.Printf("  - %s\n", warning)
		}
	}
}

func countNonPadding(files []fileEntry) int {
	n := 0
	for _, f := range files {
		if !f.Padding {
			n++
		}
	}
	return n
}

func cmdInfo(args []string) error {
	fs := flag.NewFlagSet("info [options] FILE...", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "output JSON")
	fs.BoolVar(jsonOut, "j", false, "output JSON (shorthand)")
	compact := fs.Bool("compact", false, "compact JSON output")
	raw := fs.Bool("raw", false, "include the complete bencode tree in JSON")
	noExtra := fs.Bool("no-extra", false, "do not show non-standard keys")
	showFiles := fs.Bool("files", false, "show all files in human-readable output")
	noFiles := fs.Bool("no-files", false, "do not show the file list in human-readable output")
	verbose := fs.Bool("verbose", false, "show more details")
	help := fs.Bool("help", false, "show help")
	isBool := boolFlags("json", "j", "compact", "raw", "no-extra", "files", "no-files", "verbose", "help")
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
		return errors.New("info: at least one torrent file is required")
	}

	var jsonObjects []any
	for i, path := range paths {
		doc, err := loadTorrentDoc(path)
		if err != nil {
			return err
		}
		a, err := analyzeTorrent(doc.Top, doc.RawInfo)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if *jsonOut {
			jsonObjects = append(jsonObjects, a.jsonMetadata(path, !*noExtra, *raw))
		} else {
			if i > 0 {
				fmt.Println()
			}
			printTorrentInfo(a, path, !*noFiles, *noExtra, *showFiles || *verbose)
		}
	}

	if *jsonOut {
		var value any
		if len(jsonObjects) == 1 {
			value = jsonObjects[0]
		} else {
			value = jsonObjects
		}
		var data []byte
		if *compact {
			data, err = json.Marshal(value)
		} else {
			data, err = json.MarshalIndent(value, "", "  ")
		}
		if err != nil {
			return err
		}
		data = append(data, '\n')
		_, err = os.Stdout.Write(data)
		return err
	}
	return nil
}
