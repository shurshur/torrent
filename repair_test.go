package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

type repairFixture struct {
	dir         string
	dataDir     string
	torrentPath string
}

func makeRepairFixture(t *testing.T, mode string) repairFixture {
	t.Helper()
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
	writePatternFile(t, filepath.Join(dataDir, "big1.bin"), 100000, 7)
	writePatternFile(t, filepath.Join(dataDir, "sub", "big2.bin"), 70000, 31)
	torrentPath := filepath.Join(dir, mode+".torrent")
	code := run([]string{"create", "--" + mode, "-p", "16KiB", "--tracker", "udp://tracker.example/announce", "-o", torrentPath, dataDir})
	if code != 0 {
		t.Fatalf("create %s returned %d", mode, code)
	}
	return repairFixture{dir: dir, dataDir: dataDir, torrentPath: torrentPath}
}

func writePatternFile(t *testing.T, path string, size int, seed byte) {
	t.Helper()
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i*37) + seed
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
}

func pieceLayerMap(t *testing.T, path string) map[string][]byte {
	t.Helper()
	doc, err := loadTorrentDoc(path)
	if err != nil {
		t.Fatal(err)
	}
	a, err := analyzeTorrent(doc.Top, doc.RawInfo)
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string][]byte)
	if a.PieceLayers == nil || a.PieceLayers.kind != kindDict {
		return out
	}
	for _, key := range a.PieceLayers.d.keys {
		v := a.PieceLayers.d.get(string(key))
		if v != nil && v.kind == kindBytes {
			out[string(key)] = append([]byte(nil), v.b...)
		}
	}
	return out
}

func writeTorrentWithoutAllLayers(t *testing.T, src, dst string) {
	t.Helper()
	doc, err := loadTorrentDoc(src)
	if err != nil {
		t.Fatal(err)
	}
	doc.Top.d.del("piece layers")
	data, err := doc.encoded()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0644); err != nil {
		t.Fatal(err)
	}
}

func assertLayersEqual(t *testing.T, want, got map[string][]byte) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("layer count is %d, want %d", len(got), len(want))
	}
	for key, wantLayer := range want {
		gotLayer, ok := got[key]
		if !ok {
			t.Fatalf("missing layer for root %x", []byte(key))
		}
		if !bytes.Equal(gotLayer, wantLayer) {
			t.Fatalf("layer for root %x differs", []byte(key))
		}
	}
}

func TestRepairMissingPieceLayers(t *testing.T) {
	for _, mode := range []string{"v2", "hybrid"} {
		t.Run(mode, func(t *testing.T) {
			fx := makeRepairFixture(t, mode)
			wantLayers := pieceLayerMap(t, fx.torrentPath)
			if len(wantLayers) == 0 {
				t.Fatal("fixture has no piece layers")
			}
			srcDoc, err := loadTorrentDoc(fx.torrentPath)
			if err != nil {
				t.Fatal(err)
			}
			incomplete := filepath.Join(fx.dir, mode+"-incomplete.torrent")
			writeTorrentWithoutAllLayers(t, fx.torrentPath, incomplete)
			repaired := filepath.Join(fx.dir, mode+"-repaired.torrent")
			if code := run([]string{"repair", "--quiet", "--data", fx.dir, "-o", repaired, incomplete}); code != 0 {
				t.Fatalf("repair returned %d", code)
			}
			gotLayers := pieceLayerMap(t, repaired)
			assertLayersEqual(t, wantLayers, gotLayers)

			outDoc, err := loadTorrentDoc(repaired)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(srcDoc.RawInfo, outDoc.RawInfo) {
				t.Fatal("repair changed raw info bytes")
			}
			before, _ := analyzeTorrent(srcDoc.Top, srcDoc.RawInfo)
			after, _ := analyzeTorrent(outDoc.Top, outDoc.RawInfo)
			beforeV1, beforeV2 := before.infoHashes(srcDoc.RawInfo)
			afterV1, afterV2 := after.infoHashes(outDoc.RawInfo)
			if beforeV1 != afterV1 || beforeV2 != afterV2 {
				t.Fatalf("info hashes changed: %s/%s -> %s/%s", beforeV1, beforeV2, afterV1, afterV2)
			}
			if code := run([]string{"verify", "--quiet", "--data", fx.dir, repaired}); code != 0 {
				t.Fatalf("verify returned %d", code)
			}
		})
	}
}

func TestRepairSingleFileV2(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "big.bin")
	writePatternFile(t, file, 200000, 11)
	torrentPath := filepath.Join(dir, "single.torrent")
	if code := run([]string{"create", "--v2", "-p", "16KiB", "-o", torrentPath, file}); code != 0 {
		t.Fatalf("create returned %d", code)
	}
	wantLayers := pieceLayerMap(t, torrentPath)
	if len(wantLayers) == 0 {
		t.Fatal("fixture has no piece layers")
	}
	incomplete := filepath.Join(dir, "single-incomplete.torrent")
	writeTorrentWithoutAllLayers(t, torrentPath, incomplete)
	repaired := filepath.Join(dir, "single-repaired.torrent")
	if code := run([]string{"repair", "--quiet", "--data", dir, "-o", repaired, incomplete}); code != 0 {
		t.Fatalf("repair returned %d", code)
	}
	assertLayersEqual(t, wantLayers, pieceLayerMap(t, repaired))
	if code := run([]string{"verify", "--quiet", "--data", dir, repaired}); code != 0 {
		t.Fatalf("verify returned %d", code)
	}
}

func TestRepairOnlyMissingLayer(t *testing.T) {
	fx := makeRepairFixture(t, "v2")
	wantLayers := pieceLayerMap(t, fx.torrentPath)
	if len(wantLayers) < 2 {
		t.Fatalf("fixture needs at least two layers, got %d", len(wantLayers))
	}
	doc, err := loadTorrentDoc(fx.torrentPath)
	if err != nil {
		t.Fatal(err)
	}
	a, err := analyzeTorrent(doc.Top, doc.RawInfo)
	if err != nil {
		t.Fatal(err)
	}
	var missingRoot []byte
	for _, f := range a.V2Files {
		if f.Length > a.PieceLen && len(f.PiecesRoot) == 32 {
			missingRoot = f.PiecesRoot
			break
		}
	}
	if missingRoot == nil {
		t.Fatal("no large v2 file found")
	}
	layers := doc.Top.d.get("piece layers")
	if layers == nil || layers.kind != kindDict {
		t.Fatal("piece layers dictionary is missing")
	}
	layers.d.del(string(missingRoot))
	data, err := doc.encoded()
	if err != nil {
		t.Fatal(err)
	}
	incomplete := filepath.Join(fx.dir, "partial.torrent")
	if err := os.WriteFile(incomplete, data, 0644); err != nil {
		t.Fatal(err)
	}
	repaired := filepath.Join(fx.dir, "partial-repaired.torrent")
	if code := run([]string{"repair", "--quiet", "--data", fx.dir, "-o", repaired, incomplete}); code != 0 {
		t.Fatalf("repair returned %d", code)
	}
	assertLayersEqual(t, wantLayers, pieceLayerMap(t, repaired))
}

func TestRepairAlreadyCompleteAndV1Noop(t *testing.T) {
	for _, mode := range []string{"v1", "v2"} {
		t.Run(mode, func(t *testing.T) {
			fx := makeRepairFixture(t, mode)
			out := filepath.Join(fx.dir, "should-not-exist.torrent")
			if code := run([]string{"repair", "--quiet", "--data", fx.dir, "-o", out, fx.torrentPath}); code != 0 {
				t.Fatalf("repair returned %d", code)
			}
			if _, err := os.Stat(out); !os.IsNotExist(err) {
				t.Fatalf("repair unexpectedly wrote %s", out)
			}
		})
	}
}

func TestRepairRejectsWrongData(t *testing.T) {
	fx := makeRepairFixture(t, "v2")
	incomplete := filepath.Join(fx.dir, "incomplete.torrent")
	writeTorrentWithoutAllLayers(t, fx.torrentPath, incomplete)
	big := filepath.Join(fx.dataDir, "big1.bin")
	data, err := os.ReadFile(big)
	if err != nil {
		t.Fatal(err)
	}
	data[12345] ^= 0xff
	if err := os.WriteFile(big, data, 0644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(fx.dir, "broken-repaired.torrent")
	if code := run([]string{"repair", "--quiet", "--data", fx.dir, "-o", out, incomplete}); code != 1 {
		t.Fatalf("repair returned %d, want 1", code)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("repair wrote output for mismatched data: %s", out)
	}
}

func TestRepairDefaultOutputAndForce(t *testing.T) {
	fx := makeRepairFixture(t, "v2")
	input := filepath.Join(fx.dir, "saved.torrent")
	writeTorrentWithoutAllLayers(t, fx.torrentPath, input)
	if code := run([]string{"repair", "--quiet", "--data", fx.dir, input}); code != 0 {
		t.Fatalf("repair returned %d", code)
	}
	out := filepath.Join(fx.dir, "saved.repaired.torrent")
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("default output was not created: %v", err)
	}
	// The incomplete input still needs repair, but the output now exists.
	if code := run([]string{"repair", "--quiet", "--data", fx.dir, input}); code != 1 {
		t.Fatalf("repair without --force returned %d, want 1", code)
	}
	if code := run([]string{"repair", "--quiet", "--force", "--data", fx.dir, input}); code != 0 {
		t.Fatalf("repair with --force returned %d", code)
	}
}

func TestRepairRecomputeExistingLayer(t *testing.T) {
	fx := makeRepairFixture(t, "v2")
	wantLayers := pieceLayerMap(t, fx.torrentPath)
	doc, err := loadTorrentDoc(fx.torrentPath)
	if err != nil {
		t.Fatal(err)
	}
	layers := doc.Top.d.get("piece layers")
	if layers == nil || layers.kind != kindDict || len(layers.d.keys) == 0 {
		t.Fatal("piece layers dictionary is missing")
	}
	key := layers.d.keys[0]
	old := layers.d.get(string(key))
	if old == nil || old.kind != kindBytes {
		t.Fatal("first layer is not a string")
	}
	corrupt := make([]byte, len(old.b))
	layers.d.set(key, newBytes(corrupt))
	data, err := doc.encoded()
	if err != nil {
		t.Fatal(err)
	}
	corruptTorrent := filepath.Join(fx.dir, "corrupt-layers.torrent")
	if err := os.WriteFile(corruptTorrent, data, 0644); err != nil {
		t.Fatal(err)
	}

	// Structurally the layers are complete, so the default mode does nothing.
	outNoRecompute := filepath.Join(fx.dir, "no-recompute.torrent")
	if code := run([]string{"repair", "--quiet", "--data", fx.dir, "-o", outNoRecompute, corruptTorrent}); code != 0 {
		t.Fatalf("repair returned %d", code)
	}
	if _, err := os.Stat(outNoRecompute); !os.IsNotExist(err) {
		t.Fatalf("repair unexpectedly rewrote complete-length layers: %s", outNoRecompute)
	}

	out := filepath.Join(fx.dir, "recomputed.torrent")
	if code := run([]string{"repair", "--quiet", "--recompute", "--data", fx.dir, "-o", out, corruptTorrent}); code != 0 {
		t.Fatalf("repair --recompute returned %d", code)
	}
	assertLayersEqual(t, wantLayers, pieceLayerMap(t, out))
}
