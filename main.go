// torrent - a small, dependency-free utility for BitTorrent metainfo files.
//
// It supports v1, v2 and hybrid v1+v2 torrents, emits machine-readable JSON,
// edits arbitrary (including nested and non-standard) keys while preserving
// the raw info dictionary, creates torrents, verifies data, repairs missing v2
// piece layers, creates BEP 47 padding files and serves a local web UI for
// metadata inspection and editing.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

const (
	programName = "torrent"
	version     = "0.1.0"

	blockSize = 16 * 1024 // BEP 52 leaf block size
)

// ---------------------------------------------------------------------------
// Main and help

type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		printUsage(os.Stderr)
		return 1
	}
	cmd := args[0]
	rest := args[1:]
	switch cmd {
	case "-h", "--help":
		printUsage(os.Stdout)
		return 0
	case "-V", "--version", "version":
		fmt.Printf("%s %s\n", programName, version)
		return 0
	case "help":
		if len(rest) > 0 {
			return run([]string{rest[0], "--help"})
		}
		printUsage(os.Stdout)
		return 0
	}

	var err error
	switch cmd {
	case "info", "show", "inspect":
		err = cmdInfo(rest)
	case "edit":
		err = cmdEdit(rest)
	case "create", "make":
		err = cmdCreate(rest)
	case "verify", "check":
		err = cmdVerify(rest)
	case "repair":
		err = cmdRepair(rest)
	case "pad", "padding":
		err = cmdPad(rest)
	case "export", "to-json":
		err = cmdExport(rest)
	case "import", "from-json":
		err = cmdImport(rest)
	case "web":
		err = cmdWeb(rest)
	default:
		fmt.Fprintf(os.Stderr, "error: unknown command %q\n\n", cmd)
		printUsage(os.Stderr)
		return 1
	}
	if err == nil {
		return 0
	}
	var ee *exitError
	if errors.As(err, &ee) {
		if ee.err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", ee.err)
		}
		return ee.code
	}
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
	return 1
}

func printUsage(w io.Writer) {
	fmt.Fprintf(w, `%s %s - inspect, edit, create, verify and repair BitTorrent metainfo files

Usage:
  %s <command> [options] [arguments]

Commands:
  info FILE...       Show metadata (human-readable by default, --json for jq)
  edit FILE          Edit keys; info hash is preserved unless info is edited
  create PATH...     Create a v1, v2 or hybrid v1+v2 torrent
  verify FILE...     Verify files against piece hashes
  repair FILE...     Rebuild missing v2 piece layers from local data
  pad FILE           Create BEP 47 padding files (zero-filled)
  export FILE        Export a torrent as editable JSON
  import FILE.json   Build a torrent from exported/edited JSON
  web                Start the local web UI for info/edit
  version            Print the version
  help [command]     Show help

Examples:
  %s info --json file.torrent | jq .name
  %s info --json --compact file.torrent | jq -r '.trackers.all[]'
  %s edit -i file.torrent --tracker udp://tracker.example:6969/announce
  %s edit -i file.torrent --set comment="new comment"
  %s edit file.torrent -o fixed.torrent --announce udp://example/announce
  %s create -o out.torrent --hybrid --tracker udp://example/announce ./data
  %s verify --data /downloads out.torrent
  %s repair --data /downloads saved.torrent
  %s pad --dir /downloads out.torrent
  %s web --no-browser

Key paths:
  Dot-separated paths address nested dictionaries; [N] addresses lists.
  Examples: announce, announce-list, info.source, info.files[0].path[1],
  ["key.with.dots"].  Quote keys with ["..."].

Value prefixes for --set and --append:
  int:42        integer
  str:42        force a string
  b64:...       binary value from base64
  hex:...       binary value from hex
  file:PATH     binary value from a file
  json:...      JSON value (objects, arrays, numbers, strings; no null/bool)
  bencode:...   literal bencoded value
  Anything else is stored as a UTF-8 string.

JSON output notes:
  Binary bencode strings are represented as {"$base64":"..."}.
  The export/import commands keep the original raw info bytes so that
  editing anything outside info does not change the info hash.
`, programName, version, programName, programName, programName, programName, programName, programName, programName, programName, programName, programName, programName)
}
