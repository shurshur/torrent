package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"
)

type webEditRequest struct {
	Operations    []webEditOp `json:"operations"`
	IgnoreMissing bool        `json:"ignore_missing"`
}

type webEditOp struct {
	Op     string     `json:"op"`
	Path   string     `json:"path,omitempty"`
	Type   string     `json:"type,omitempty"`
	Value  string     `json:"value,omitempty"`
	Values []string   `json:"values,omitempty"`
	Tiers  [][]string `json:"tiers,omitempty"`
}

func (s *webServer) handleInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"version":             s.version,
		"token":               s.token,
		"max_upload":          s.opts.MaxUpload,
		"max_sessions":        s.opts.MaxSessions,
		"session_ttl_seconds": int64(s.opts.SessionTTL / time.Second),
		"capabilities":        []string{"info", "edit", "download", "reset"},
	})
}

func (s *webServer) handleUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, s.opts.MaxUpload)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeAPIError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("torrent is larger than %d bytes", maxErr.Limit))
			return
		}
		writeAPIError(w, http.StatusBadRequest, "read upload: "+err.Error())
		return
	}
	if len(data) == 0 {
		writeAPIError(w, http.StatusBadRequest, "empty upload")
		return
	}
	doc, err := parseTorrentDoc(data)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid torrent: "+err.Error())
		return
	}
	a, err := analyzeTorrent(doc.Top, doc.RawInfo)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid torrent: "+err.Error())
		return
	}
	rawName := r.URL.Query().Get("name")
	if rawName == "" {
		rawName = r.Header.Get("X-Filename")
	}
	fallback := "uploaded.torrent"
	if a.Name != "" {
		fallback = a.Name + ".torrent"
	}
	filename := sanitizeTorrentFilename(rawName, fallback)
	v1, v2 := a.infoHashes(doc.RawInfo)
	sess, err := s.createSession(data, filename, v1, v2)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, errTooManySessions) {
			status = http.StatusTooManyRequests
		}
		writeAPIError(w, status, err.Error())
		return
	}
	meta, err := s.sessionMetadata(sess)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":       sess.ID,
		"filename": sess.Filename,
		"metadata": meta,
	})
}

func (s *webServer) handleGetTorrent(w http.ResponseWriter, r *http.Request) {
	sess := s.getSession(r.PathValue("id"))
	if sess == nil {
		writeAPIError(w, http.StatusNotFound, "torrent session not found")
		return
	}
	meta, err := s.sessionMetadata(sess)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":       sess.ID,
		"filename": sess.Filename,
		"metadata": meta,
	})
}

func (s *webServer) handleEditTorrent(w http.ResponseWriter, r *http.Request) {
	sess := s.getSession(r.PathValue("id"))
	if sess == nil {
		writeAPIError(w, http.StatusNotFound, "torrent session not found")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
	defer r.Body.Close()
	var req webEditRequest
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	o, err := buildEditOptions(req)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}

	sess.mu.Lock()
	doc, err := parseTorrentDoc(sess.Current)
	if err != nil {
		sess.mu.Unlock()
		writeAPIError(w, http.StatusInternalServerError, "stored torrent is invalid: "+err.Error())
		return
	}
	res, err := applyEdit(doc, &o)
	if err != nil {
		sess.mu.Unlock()
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	sess.Current = append([]byte(nil), res.Encoded...)
	sess.mu.Unlock()
	s.touchSession(sess)

	meta, err := s.sessionMetadata(sess)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":                  sess.ID,
		"filename":            sess.Filename,
		"metadata":            meta,
		"info_changed":        res.InfoChanged,
		"info_hash_v1_before": res.OldV1,
		"info_hash_v1_after":  res.NewV1,
		"info_hash_v2_before": res.OldV2,
		"info_hash_v2_after":  res.NewV2,
		"operations":          res.Operations,
		"warnings":            res.Warnings,
	})
}

func (s *webServer) handleResetTorrent(w http.ResponseWriter, r *http.Request) {
	sess := s.getSession(r.PathValue("id"))
	if sess == nil {
		writeAPIError(w, http.StatusNotFound, "torrent session not found")
		return
	}
	sess.mu.Lock()
	sess.Current = append([]byte(nil), sess.Original...)
	sess.mu.Unlock()
	s.touchSession(sess)
	meta, err := s.sessionMetadata(sess)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":       sess.ID,
		"filename": sess.Filename,
		"metadata": meta,
	})
}

func (s *webServer) handleDownloadTorrent(w http.ResponseWriter, r *http.Request) {
	sess := s.getSession(r.PathValue("id"))
	if sess == nil {
		writeAPIError(w, http.StatusNotFound, "torrent session not found")
		return
	}
	sess.mu.Lock()
	data := append([]byte(nil), sess.Current...)
	filename := sess.Filename
	sess.mu.Unlock()
	s.touchSession(sess)

	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": filename})
	if disposition == "" {
		disposition = "attachment"
	}
	w.Header().Set("Content-Type", "application/x-bittorrent")
	w.Header().Set("Content-Disposition", disposition)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (s *webServer) handleDeleteTorrent(w http.ResponseWriter, r *http.Request) {
	if !s.deleteSession(r.PathValue("id")) {
		writeAPIError(w, http.StatusNotFound, "torrent session not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

func (s *webServer) sessionMetadata(sess *webSession) (map[string]any, error) {
	sess.mu.Lock()
	current := append([]byte(nil), sess.Current...)
	original := append([]byte(nil), sess.Original...)
	filename := sess.Filename
	sess.mu.Unlock()
	dirty := !bytes.Equal(current, original)
	return webMetadata(current, filename, sess.ID, dirty, sess.OriginalV1, sess.OriginalV2)
}

func (s *webServer) touchSession(sess *webSession) {
	s.mu.Lock()
	sess.Touched = time.Now()
	s.mu.Unlock()
}

func webMetadata(data []byte, filename, id string, dirty bool, originalV1, originalV2 string) (map[string]any, error) {
	doc, err := parseTorrentDoc(data)
	if err != nil {
		return nil, err
	}
	a, err := analyzeTorrent(doc.Top, doc.RawInfo)
	if err != nil {
		return nil, err
	}
	obj := a.jsonMetadata(filename, true, false)
	obj["session_id"] = id
	obj["filename"] = filename
	obj["dirty"] = dirty
	if originalV1 != "" {
		obj["original_info_hash_v1"] = originalV1
	}
	if originalV2 != "" {
		obj["original_info_hash_v2"] = originalV2
	}
	return obj, nil
}

func buildEditOptions(req webEditRequest) (editOptions, error) {
	var o editOptions
	o.ignoreMissing = req.IgnoreMissing
	if len(req.Operations) == 0 {
		return o, errors.New("no edit operations supplied")
	}
	for i, op := range req.Operations {
		opName := strings.ToLower(strings.TrimSpace(op.Op))
		switch opName {
		case "set":
			if op.Path == "" {
				return o, fmt.Errorf("operation %d: set requires a path", i+1)
			}
			if err := addTypedSet(&o, op.Path, op.Type, op.Value); err != nil {
				return o, fmt.Errorf("operation %d (%s): %w", i+1, op.Path, err)
			}
		case "append":
			if op.Path == "" {
				return o, fmt.Errorf("operation %d: append requires a path", i+1)
			}
			if err := addTypedAppend(&o, op.Path, op.Type, op.Value); err != nil {
				return o, fmt.Errorf("operation %d (%s): %w", i+1, op.Path, err)
			}
		case "unset":
			if op.Path == "" {
				return o, fmt.Errorf("operation %d: unset requires a path", i+1)
			}
			o.unsets = append(o.unsets, op.Path)
		case "set_announce":
			o.announce = optString{value: op.Value, set: true}
		case "unset_announce":
			o.unsetAnnounce = true
		case "add_tracker":
			if strings.TrimSpace(op.Value) == "" {
				return o, fmt.Errorf("operation %d: add_tracker requires a value", i+1)
			}
			o.trackers = append(o.trackers, op.Value)
		case "remove_tracker":
			if strings.TrimSpace(op.Value) == "" {
				return o, fmt.Errorf("operation %d: remove_tracker requires a value", i+1)
			}
			o.removeTrackers = append(o.removeTrackers, op.Value)
		case "replace_trackers":
			o.replaceTrackers = true
			if len(op.Values) == 0 {
				o.clearTrackers = true
			} else {
				o.trackers = append(o.trackers, op.Values...)
			}
		case "set_trackers":
			if op.Tiers == nil {
				return o, fmt.Errorf("operation %d: set_trackers requires a tiers array", i+1)
			}
			encoded, err := json.Marshal(op.Tiers)
			if err != nil {
				return o, fmt.Errorf("operation %d: encode tiers: %w", i+1, err)
			}
			o.setJSONs = append(o.setJSONs, "announce-list="+string(encoded))
		case "clear_trackers":
			o.clearTrackers = true
		case "set_comment":
			o.comment = optString{value: op.Value, set: true}
		case "unset_comment":
			o.unsetComment = true
		case "set_source":
			o.source = optString{value: op.Value, set: true}
		case "unset_source":
			o.unsetSource = true
		case "set_private":
			o.private = true
		case "set_public":
			o.public = true
		default:
			return o, fmt.Errorf("operation %d: unknown operation %q", i+1, op.Op)
		}
	}
	return o, nil
}

func normalizeValueType(t string) string {
	t = strings.ToLower(strings.TrimSpace(t))
	if t == "" {
		return "string"
	}
	return t
}

func addTypedSet(o *editOptions, keyPath, valueType, value string) error {
	switch normalizeValueType(valueType) {
	case "string":
		o.sets = append(o.sets, keyPath+"=str:"+value)
	case "int":
		o.setInts = append(o.setInts, keyPath+"="+value)
	case "json":
		o.setJSONs = append(o.setJSONs, keyPath+"="+value)
	case "bencode":
		o.setBencodes = append(o.setBencodes, keyPath+"="+value)
	case "base64":
		o.sets = append(o.sets, keyPath+"=b64:"+value)
	case "hex":
		o.sets = append(o.sets, keyPath+"=hex:"+value)
	default:
		return fmt.Errorf("unknown value type %q", valueType)
	}
	return nil
}

func addTypedAppend(o *editOptions, keyPath, valueType, value string) error {
	switch normalizeValueType(valueType) {
	case "string":
		o.appends = append(o.appends, keyPath+"=str:"+value)
	case "int":
		o.appends = append(o.appends, keyPath+"=int:"+value)
	case "json":
		o.appends = append(o.appends, keyPath+"=json:"+value)
	case "bencode":
		o.appends = append(o.appends, keyPath+"=bencode:"+value)
	case "base64":
		o.appends = append(o.appends, keyPath+"=b64:"+value)
	case "hex":
		o.appends = append(o.appends, keyPath+"=hex:"+value)
	default:
		return fmt.Errorf("unknown value type %q", valueType)
	}
	return nil
}

func sanitizeTorrentFilename(raw, fallback string) string {
	name := strings.TrimSpace(raw)
	name = strings.ReplaceAll(name, "\\", "/")
	name = path.Base(name)
	if name == "" || name == "." || name == ".." || name == "/" {
		name = fallback
		name = strings.ReplaceAll(name, "\\", "/")
		name = path.Base(name)
	}
	name = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, name)
	if name == "" || name == "." || name == ".." || name == "/" {
		name = "uploaded.torrent"
	}
	if !strings.HasSuffix(strings.ToLower(name), ".torrent") {
		name += ".torrent"
	}
	if name == ".torrent" {
		name = "uploaded.torrent"
	}
	return name
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(value)
}

func writeAPIError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": message})
}
