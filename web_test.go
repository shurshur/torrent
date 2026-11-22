package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

type webTestEnv struct {
	server *httptest.Server
	client *http.Client
	token  string
}

func newWebTestEnv(t *testing.T, opts webOptions) *webTestEnv {
	t.Helper()
	if opts.MaxUpload == 0 {
		opts.MaxUpload = 4 << 20
	}
	if opts.SessionTTL == 0 {
		opts.SessionTTL = time.Minute
	}
	if opts.MaxSessions == 0 {
		opts.MaxSessions = 4
	}
	srv, err := newWebServer(opts)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	client := ts.Client()
	resp, err := client.Get(ts.URL + "/api/info")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var info struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		t.Fatal(err)
	}
	if info.Token == "" {
		t.Fatal("empty web token")
	}
	return &webTestEnv{server: ts, client: client, token: info.Token}
}

func (e *webTestEnv) request(t *testing.T, method, path string, body []byte, contentType string, withToken bool) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, e.server.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if withToken {
		req.Header.Set("X-Torrent-Web-Token", e.token)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, data
}

func (e *webTestEnv) upload(t *testing.T, name string, data []byte) (int, map[string]any) {
	t.Helper()
	resp, body := e.request(t, http.MethodPost, "/api/torrents?name="+name, data, "application/x-bittorrent", true)
	var obj map[string]any
	if len(body) > 0 {
		if err := json.Unmarshal(body, &obj); err != nil {
			t.Fatalf("decode upload response: %v (%s)", err, body)
		}
	}
	return resp.StatusCode, obj
}

func (e *webTestEnv) edit(t *testing.T, id string, ops []map[string]any) (int, map[string]any) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"operations": ops})
	if err != nil {
		t.Fatal(err)
	}
	resp, body := e.request(t, http.MethodPost, "/api/torrents/"+id+"/edit", payload, "application/json", true)
	var obj map[string]any
	if len(body) > 0 {
		if err := json.Unmarshal(body, &obj); err != nil {
			t.Fatalf("decode edit response: %v (%s)", err, body)
		}
	}
	return resp.StatusCode, obj
}

func TestWebServesEmbeddedUI(t *testing.T) {
	env := newWebTestEnv(t, webOptions{})
	resp, body := env.request(t, http.MethodGet, "/", nil, "", false)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("index status %d", resp.StatusCode)
	}
	if !bytes.Contains(body, []byte(`id="uploadView"`)) {
		preview := body
		if len(preview) > 200 {
			preview = preview[:200]
		}
		t.Fatalf("index does not look like the web UI: %s", preview)
	}
	if resp.Header.Get("Content-Security-Policy") == "" {
		t.Fatal("missing Content-Security-Policy header")
	}
	resp, body = env.request(t, http.MethodGet, "/app.js", nil, "", false)
	if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("parseTrackers")) {
		t.Fatalf("app.js was not served correctly: status=%d", resp.StatusCode)
	}
}

func TestWebUploadInfoEditDownloadReset(t *testing.T) {
	fx := makeRepairFixture(t, "hybrid")
	original, err := os.ReadFile(fx.torrentPath)
	if err != nil {
		t.Fatal(err)
	}
	env := newWebTestEnv(t, webOptions{})
	status, uploaded := env.upload(t, "sample.torrent", original)
	if status != http.StatusCreated {
		t.Fatalf("upload status %d: %v", status, uploaded)
	}
	id, _ := uploaded["id"].(string)
	if id == "" {
		t.Fatal("upload response has no id")
	}
	meta := uploaded["metadata"].(map[string]any)
	if meta["name"] != "data" {
		t.Fatalf("name = %v", meta["name"])
	}
	v1Before, _ := meta["info_hash_v1"].(string)
	if v1Before == "" {
		t.Fatal("missing v1 info hash")
	}

	status, edited := env.edit(t, id, []map[string]any{{"op": "set_comment", "value": "web test"}})
	if status != http.StatusOK {
		t.Fatalf("edit status %d: %v", status, edited)
	}
	if edited["info_changed"] != false {
		t.Fatalf("top-level edit changed info hash: %v", edited)
	}
	if edited["metadata"].(map[string]any)["comment"] != "web test" {
		t.Fatalf("comment not updated: %v", edited["metadata"])
	}

	status, edited = env.edit(t, id, []map[string]any{{"op": "set_source", "value": "source-x"}})
	if status != http.StatusOK {
		t.Fatalf("source edit status %d: %v", status, edited)
	}
	if edited["info_changed"] != true {
		t.Fatalf("info edit did not report hash change: %v", edited)
	}
	if edited["info_hash_v1_before"] == edited["info_hash_v1_after"] {
		t.Fatalf("v1 hash did not change: %v", edited)
	}

	resp, downloaded := env.request(t, http.MethodGet, "/api/torrents/"+id+"/download", nil, "", true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download status %d", resp.StatusCode)
	}
	doc, err := parseTorrentDoc(downloaded)
	if err != nil {
		t.Fatal(err)
	}
	a, err := analyzeTorrent(doc.Top, doc.RawInfo)
	if err != nil {
		t.Fatal(err)
	}
	if a.Source != "source-x" {
		t.Fatalf("downloaded source = %q", a.Source)
	}
	if a.Comment != "web test" {
		t.Fatalf("downloaded comment = %q", a.Comment)
	}

	resp, body := env.request(t, http.MethodPost, "/api/torrents/"+id+"/reset", nil, "", true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reset status %d: %s", resp.StatusCode, body)
	}
	var reset struct {
		Metadata map[string]any `json:"metadata"`
	}
	if err := json.Unmarshal(body, &reset); err != nil {
		t.Fatal(err)
	}
	if reset.Metadata["dirty"] != false {
		t.Fatalf("reset session is still dirty: %v", reset.Metadata["dirty"])
	}
	_, downloaded = env.request(t, http.MethodGet, "/api/torrents/"+id+"/download", nil, "", true)
	if !bytes.Equal(original, downloaded) {
		t.Fatal("download after reset differs from original")
	}
}

func TestWebStructuredOperations(t *testing.T) {
	fx := makeRepairFixture(t, "v2")
	original, err := os.ReadFile(fx.torrentPath)
	if err != nil {
		t.Fatal(err)
	}
	env := newWebTestEnv(t, webOptions{})
	status, uploaded := env.upload(t, "ops.torrent", original)
	if status != http.StatusCreated {
		t.Fatalf("upload status %d: %v", status, uploaded)
	}
	id := uploaded["id"].(string)

	status, edited := env.edit(t, id, []map[string]any{
		{"op": "set", "path": "custom.nested", "type": "string", "value": "hello"},
		{"op": "set", "path": "custom.number", "type": "int", "value": "42"},
		{"op": "append", "path": "custom.list", "type": "int", "value": "7"},
		{"op": "set_announce", "value": "udp://tracker.example/announce"},
		{"op": "set_trackers", "tiers": [][]string{
			{"udp://tracker.example/announce", "udp://backup.example/announce"},
			{"udp://second.example/announce"},
		}},
	})
	if status != http.StatusOK {
		t.Fatalf("edit status %d: %v", status, edited)
	}
	meta := edited["metadata"].(map[string]any)
	extra := meta["extra"].(map[string]any)
	top := extra["top"].(map[string]any)
	custom := top["custom"].(map[string]any)
	if custom["nested"] != "hello" || custom["number"] != float64(42) {
		t.Fatalf("unexpected custom value: %#v", custom)
	}
	list := custom["list"].([]any)
	if len(list) != 1 || list[0] != float64(7) {
		t.Fatalf("unexpected list: %#v", list)
	}
	tiers := meta["trackers"].(map[string]any)["tiers"].([]any)
	if len(tiers) != 2 || len(tiers[0].([]any)) != 2 || len(tiers[1].([]any)) != 1 {
		t.Fatalf("tracker tiers were not preserved: %#v", tiers)
	}
	if edited["info_changed"] != false {
		t.Fatal("top-level custom edit changed info hash")
	}

	status, edited = env.edit(t, id, []map[string]any{{"op": "unset", "path": "custom.nested"}})
	if status != http.StatusOK {
		t.Fatalf("unset status %d: %v", status, edited)
	}
	custom = edited["metadata"].(map[string]any)["extra"].(map[string]any)["top"].(map[string]any)["custom"].(map[string]any)
	if _, ok := custom["nested"]; ok {
		t.Fatalf("nested key was not removed: %#v", custom)
	}
}

func TestWebRequiresTokenAndValidatesUpload(t *testing.T) {
	env := newWebTestEnv(t, webOptions{})
	fx := makeRepairFixture(t, "v1")
	original, err := os.ReadFile(fx.torrentPath)
	if err != nil {
		t.Fatal(err)
	}
	resp, body := env.request(t, http.MethodPost, "/api/torrents", original, "application/x-bittorrent", false)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("no-token upload status %d: %s", resp.StatusCode, body)
	}
	resp, body = env.request(t, http.MethodPost, "/api/torrents", []byte("not a torrent"), "application/x-bittorrent", true)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid upload status %d: %s", resp.StatusCode, body)
	}
	// /api/info intentionally has no token so the page can bootstrap.
	resp, body = env.request(t, http.MethodGet, "/api/info", nil, "", false)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("info status %d: %s", resp.StatusCode, body)
	}
}

func TestWebUploadLimitAndSessionLimit(t *testing.T) {
	fx := makeRepairFixture(t, "v2")
	original, err := os.ReadFile(fx.torrentPath)
	if err != nil {
		t.Fatal(err)
	}
	env := newWebTestEnv(t, webOptions{MaxUpload: 32, MaxSessions: 1})
	status, body := env.upload(t, "too-big.torrent", original)
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized upload status %d: %v", status, body)
	}

	env = newWebTestEnv(t, webOptions{MaxSessions: 1})
	status, first := env.upload(t, "first.torrent", original)
	if status != http.StatusCreated {
		t.Fatalf("first upload status %d: %v", status, first)
	}
	status, second := env.upload(t, "second.torrent", original)
	if status != http.StatusTooManyRequests {
		t.Fatalf("second upload status %d: %v", status, second)
	}
}

func TestWebSessionExpiry(t *testing.T) {
	fx := makeRepairFixture(t, "v2")
	original, err := os.ReadFile(fx.torrentPath)
	if err != nil {
		t.Fatal(err)
	}
	env := newWebTestEnv(t, webOptions{SessionTTL: time.Nanosecond})
	status, uploaded := env.upload(t, "expire.torrent", original)
	if status != http.StatusCreated {
		t.Fatalf("upload status %d: %v", status, uploaded)
	}
	id := uploaded["id"].(string)
	resp, body := env.request(t, http.MethodGet, "/api/torrents/"+id, nil, "", true)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expired session status %d: %s", resp.StatusCode, body)
	}
}
