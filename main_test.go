package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestBencodeParseEncode(t *testing.T) {
	data := []byte("d4:spaml1:a1:be3:fooi42e3:bar4:eggse")
	v, err := parseBencode(data)
	if err != nil {
		t.Fatal(err)
	}
	got := string(encodeBencode(v))
	want := "d3:bar4:eggs3:fooi42e4:spaml1:a1:bee"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestRawInfoPreservation(t *testing.T) {
	// Deliberately not in canonical key order.
	data := []byte("d4:infod4:name1:x6:lengthi3ee3:fooi1ee")
	top, err := parseBencode(data)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := findRawInfo(data)
	if err != nil {
		t.Fatal(err)
	}
	top.d.setString("foo", newInt(2))
	encoded := encodeTop(top, raw, false)
	raw2, err := findRawInfo(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, raw2) {
		t.Fatalf("raw info changed: %q -> %q", raw, raw2)
	}
	// Editing info must re-encode it.
	info := top.d.get("info")
	info.d.setString("name", newString("y"))
	encoded = encodeTop(top, raw, true)
	raw3, err := findRawInfo(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(raw, raw3) {
		t.Fatal("raw info should have changed after editing info")
	}
}

func TestPaths(t *testing.T) {
	root := newDictValue()
	if _, err := setPath(root, mustPath(t, `info.files[0]`), newDictValue()); err != nil {
		t.Fatal(err)
	}
	if _, err := setPath(root, mustPath(t, `info.files[0].path[0]`), newString("a")); err != nil {
		t.Fatal(err)
	}
	elems := mustPath(t, `info.files[0].path[1]`)
	if _, err := setPath(root, elems, newString("x")); err != nil {
		t.Fatal(err)
	}
	got, err := getPath(root, elems)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || string(got.b) != "x" {
		t.Fatalf("unexpected value: %#v", got)
	}
	if _, err := unsetPath(root, elems); err != nil {
		t.Fatal(err)
	}
	parent, err := getPath(root, mustPath(t, `info.files[0].path`))
	if err != nil {
		t.Fatal(err)
	}
	if parent == nil || parent.kind != kindList || len(parent.l) != 1 {
		t.Fatalf("path was not removed: %#v", parent)
	}
	// Quoted keys and escaped dots.
	elems, err = parsePath(`["a.b"].c`)
	if err != nil {
		t.Fatal(err)
	}
	if elems[0].key != "a.b" || elems[1].key != "c" {
		t.Fatalf("bad quoted path: %#v", elems)
	}
	elems, err = parsePath(`a\.b`)
	if err != nil {
		t.Fatal(err)
	}
	if elems[0].key != "a.b" {
		t.Fatalf("bad escaped path: %#v", elems)
	}
}

func mustPath(t *testing.T, s string) []pathElem {
	t.Helper()
	elems, err := parsePath(s)
	if err != nil {
		t.Fatal(err)
	}
	return elems
}

func TestV2HashVectors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.bin")
	data := make([]byte, 100000)
	for i := range data {
		data[i] = byte(i % 256)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}

	h, err := v2FileHashes(path, 16384)
	if err != nil {
		t.Fatal(err)
	}
	const wantRoot = "47ef7178c4aa458963d46add44d36bd3524640c248fcd08a448096f1ad43fe6c"
	if got := hex.EncodeToString(h.Root[:]); got != wantRoot {
		t.Fatalf("root = %s, want %s", got, wantRoot)
	}
	if len(h.PieceRoots) != 7 {
		t.Fatalf("piece roots = %d, want 7", len(h.PieceRoots))
	}
	firstPiece := "a1f259d4365ed4320c377ce26f5c8c56dcdc9a89e7b641bfd8eabfbbeac86654"
	lastPiece := "acff875f02db2f6c0e8429458e53322c64a611cf533417f5bfa38782c99dc02f"
	if got := hex.EncodeToString(h.Layer[:sha256.Size]); got != firstPiece {
		t.Fatalf("first layer hash = %s, want %s", got, firstPiece)
	}
	if got := hex.EncodeToString(h.Layer[len(h.Layer)-sha256.Size:]); got != lastPiece {
		t.Fatalf("last layer hash = %s, want %s", got, lastPiece)
	}

	h, err = v2FileHashes(path, 131072)
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(h.Root[:]); got != wantRoot {
		t.Fatalf("single-piece root = %s, want %s", got, wantRoot)
	}
	if len(h.Layer) != 0 {
		t.Fatalf("single-piece file should have no stored layer")
	}

	smallPath := filepath.Join(dir, "three-blocks.bin")
	small := make([]byte, 3*blockSize)
	for i := range small {
		small[i] = byte(i % 256)
	}
	if err := os.WriteFile(smallPath, small, 0644); err != nil {
		t.Fatal(err)
	}
	h, err = v2FileHashes(smallPath, 4*blockSize)
	if err != nil {
		t.Fatal(err)
	}
	const wantSmall = "f58f20cf9931d62f43e46349544f414bfdcb31eb514dbda6b30732415f46573f"
	if got := hex.EncodeToString(h.Root[:]); got != wantSmall {
		t.Fatalf("three-block root = %s, want %s", got, wantSmall)
	}
}

func TestCreateEditVerifyExportImport(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(filepath.Join(dataDir, "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "a.txt"), []byte("hello world\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "empty"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	big := make([]byte, 100000)
	for i := range big {
		big[i] = byte(i % 251)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "sub", "b.bin"), big, 0644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		args []string
		v1   bool
		v2   bool
	}{
		{"v1", []string{"create", "--v1", "--tracker", "udp://tracker.example:6969/announce"}, true, false},
		{"v2", []string{"create", "--v2", "--tracker", "udp://tracker.example:6969/announce"}, false, true},
		{"hybrid", []string{"create", "--hybrid", "--tracker", "udp://tracker.example:6969/announce"}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := filepath.Join(dir, tc.name+".torrent")
			args := append(append([]string{}, tc.args...), "-o", out, dataDir)
			if code := run(args); code != 0 {
				t.Fatalf("create returned %d", code)
			}
			doc, err := loadTorrentDoc(out)
			if err != nil {
				t.Fatal(err)
			}
			a, err := analyzeTorrent(doc.Top, doc.RawInfo)
			if err != nil {
				t.Fatal(err)
			}
			if a.V1 != tc.v1 || a.V2 != tc.v2 {
				t.Fatalf("versions: v1=%v v2=%v", a.V1, a.V2)
			}
			if code := run([]string{"verify", "--data", dir, out}); code != 0 {
				t.Fatalf("verify returned %d", code)
			}
		})
	}

	// Editing outside info must leave the raw info bytes untouched.
	src := filepath.Join(dir, "v1.torrent")
	doc, err := loadTorrentDoc(src)
	if err != nil {
		t.Fatal(err)
	}
	rawBefore := append([]byte(nil), doc.RawInfo...)
	before, _ := analyzeTorrent(doc.Top, doc.RawInfo)
	h1Before, _ := before.infoHashes(doc.RawInfo)
	if code := run([]string{"edit", "-i", src, "--tracker", "udp://second.example/announce", "--set", "comment=hello"}); code != 0 {
		t.Fatalf("edit returned %d", code)
	}
	doc, err = loadTorrentDoc(src)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rawBefore, doc.RawInfo) {
		t.Fatal("raw info changed after editing outside info")
	}
	after, _ := analyzeTorrent(doc.Top, doc.RawInfo)
	h1After, _ := after.infoHashes(doc.RawInfo)
	if h1Before != h1After {
		t.Fatalf("info hash changed: %s -> %s", h1Before, h1After)
	}

	// Export/import without touching info must preserve raw info as well.
	hybrid := filepath.Join(dir, "hybrid.torrent")
	jsonPath := filepath.Join(dir, "hybrid.json")
	if code := run([]string{"export", "-o", jsonPath, hybrid}); code != 0 {
		t.Fatalf("export returned %d", code)
	}
	imported := filepath.Join(dir, "imported.torrent")
	if code := run([]string{"import", "-o", imported, jsonPath}); code != 0 {
		t.Fatalf("import returned %d", code)
	}
	srcDoc, err := loadTorrentDoc(hybrid)
	if err != nil {
		t.Fatal(err)
	}
	dstDoc, err := loadTorrentDoc(imported)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(srcDoc.RawInfo, dstDoc.RawInfo) {
		t.Fatal("raw info changed after export/import")
	}

	// Editing an info key in JSON is allowed, but changes the info hash.
	var obj map[string]any
	rawJSON, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(rawJSON, &obj); err != nil {
		t.Fatal(err)
	}
	info := obj["info"].(map[string]any)
	info["name"] = "renamed"
	editedJSON, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	editedPath := filepath.Join(dir, "edited.json")
	if err := os.WriteFile(editedPath, editedJSON, 0644); err != nil {
		t.Fatal(err)
	}
	renamed := filepath.Join(dir, "renamed.torrent")
	if code := run([]string{"import", "-o", renamed, editedPath}); code != 0 {
		t.Fatalf("import edited returned %d", code)
	}
	renamedDoc, err := loadTorrentDoc(renamed)
	if err != nil {
		t.Fatal(err)
	}
	ra, err := analyzeTorrent(renamedDoc.Top, renamedDoc.RawInfo)
	if err != nil {
		t.Fatal(err)
	}
	_, h2Before := after.infoHashes(after.RawInfo)
	_, h2After := ra.infoHashes(renamedDoc.RawInfo)
	if h2Before == h2After {
		t.Fatal("info hash should change when info.name is edited")
	}
}

func TestHybridFileOrder(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "order")
	for _, sub := range []string{"a", "foo"} {
		if err := os.MkdirAll(filepath.Join(dataDir, sub), 0755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		"a.txt":   "1",
		"a/b.txt": "2",
		"foo.txt": "3",
		"foo/x":   "4",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dataDir, filepath.FromSlash(name)), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	torrentPath := filepath.Join(dir, "order.torrent")
	if code := run([]string{"create", "--hybrid", "-p", "16KiB", "-o", torrentPath, dataDir}); code != 0 {
		t.Fatalf("create returned %d", code)
	}
	doc, err := loadTorrentDoc(torrentPath)
	if err != nil {
		t.Fatal(err)
	}
	a, err := analyzeTorrent(doc.Top, doc.RawInfo)
	if err != nil {
		t.Fatal(err)
	}
	var v1, v2 []string
	for _, f := range a.V1Files {
		if !f.Padding {
			v1 = append(v1, pathKey(f.Path))
		}
	}
	for _, f := range a.V2Files {
		v2 = append(v2, pathKey(f.Path))
	}
	if len(v1) != len(v2) {
		t.Fatalf("v1/v2 file counts differ: %v vs %v", v1, v2)
	}
	for i := range v1 {
		if v1[i] != v2[i] {
			t.Fatalf("file order differs at %d: v1=%v v2=%v", i, v1, v2)
		}
	}
	if code := run([]string{"verify", "--data", dir, torrentPath}); code != 0 {
		t.Fatalf("verify returned %d", code)
	}
}

func TestPadCreation(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "a"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "b"), []byte("world"), 0644); err != nil {
		t.Fatal(err)
	}
	torrentPath := filepath.Join(dir, "hybrid.torrent")
	if code := run([]string{"create", "--hybrid", "-p", "16KiB", "-o", torrentPath, dataDir}); code != 0 {
		t.Fatalf("create returned %d", code)
	}
	if code := run([]string{"pad", "--dir", dir, torrentPath}); code != 0 {
		t.Fatalf("pad returned %d", code)
	}
	doc, err := loadTorrentDoc(torrentPath)
	if err != nil {
		t.Fatal(err)
	}
	a, err := analyzeTorrent(doc.Top, doc.RawInfo)
	if err != nil {
		t.Fatal(err)
	}
	pads := 0
	for _, f := range a.V1Files {
		if !f.Padding {
			continue
		}
		pads++
		p := filepath.Join(dir, a.Name, filepath.Join(f.Path...))
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if st.Size() != f.Length {
			t.Fatalf("%s: size %d, want %d", p, st.Size(), f.Length)
		}
	}
	if pads == 0 {
		t.Fatal("expected at least one padding file")
	}
}

func TestSafeJoin(t *testing.T) {
	if _, err := safeJoinComponents("/tmp/base", []string{"..", "etc"}); err == nil {
		t.Fatal("expected an error for ..")
	}
	if _, err := safeJoinComponents("/tmp/base", []string{"a/b"}); err == nil {
		t.Fatal("expected an error for slash")
	}
	got, err := safeJoinComponents("/tmp/base", []string{"name", "file"})
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join("/tmp/base", "name", "file") {
		t.Fatalf("got %q", got)
	}
}
