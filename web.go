package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed web
var webAssets embed.FS

var errTooManySessions = errors.New("too many web sessions")

type webOptions struct {
	MaxUpload   int64
	SessionTTL  time.Duration
	MaxSessions int
}

type webSession struct {
	ID       string
	Filename string
	Original []byte
	Current  []byte

	OriginalV1 string
	OriginalV2 string

	Touched time.Time

	mu sync.Mutex
}

type webServer struct {
	opts    webOptions
	version string
	token   string

	mu       sync.Mutex
	sessions map[string]*webSession

	handler http.Handler
}

func newWebServer(opts webOptions) (*webServer, error) {
	if opts.MaxUpload <= 0 {
		return nil, errors.New("max upload size must be positive")
	}
	if opts.SessionTTL <= 0 {
		return nil, errors.New("session TTL must be positive")
	}
	if opts.MaxSessions <= 0 {
		return nil, errors.New("max sessions must be positive")
	}
	token, err := randomToken(32)
	if err != nil {
		return nil, err
	}
	s := &webServer{
		opts:     opts,
		version:  version,
		token:    token,
		sessions: make(map[string]*webSession),
	}
	webRoot, err := fs.Sub(webAssets, "web")
	if err != nil {
		return nil, err
	}
	fileServer := http.FileServer(http.FS(webRoot))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/info", s.handleInfo)
	mux.HandleFunc("POST /api/torrents", s.handleUpload)
	mux.HandleFunc("GET /api/torrents/{id}", s.handleGetTorrent)
	mux.HandleFunc("POST /api/torrents/{id}/edit", s.handleEditTorrent)
	mux.HandleFunc("POST /api/torrents/{id}/reset", s.handleResetTorrent)
	mux.HandleFunc("GET /api/torrents/{id}/download", s.handleDownloadTorrent)
	mux.HandleFunc("DELETE /api/torrents/{id}", s.handleDeleteTorrent)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeAPIError(w, http.StatusNotFound, "not found")
	})
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		fileServer.ServeHTTP(w, r)
	}))

	s.handler = s.securityHeaders(s.recoverer(s.tokenAuth(mux)))
	return s, nil
}

func (s *webServer) Handler() http.Handler { return s.handler }

func (s *webServer) tokenAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/torrents") {
			got := r.Header.Get("X-Torrent-Web-Token")
			if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
				writeAPIError(w, http.StatusForbidden, "invalid or missing web token")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *webServer) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'")
		if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/" || r.URL.Path == "/index.html" {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

func (s *webServer) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				fmt.Fprintf(os.Stderr, "torrent web: panic: %v\n%s", rec, debug.Stack())
				if strings.HasPrefix(r.URL.Path, "/api/") {
					writeAPIError(w, http.StatusInternalServerError, "internal server error")
				} else {
					http.Error(w, "internal server error", http.StatusInternalServerError)
				}
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *webServer) cleanupLocked(now time.Time) {
	for id, sess := range s.sessions {
		if now.Sub(sess.Touched) > s.opts.SessionTTL {
			delete(s.sessions, id)
		}
	}
}

func (s *webServer) cleanupLoop(ctx context.Context) {
	interval := s.opts.SessionTTL / 2
	if interval < time.Minute {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			s.mu.Lock()
			s.cleanupLocked(now)
			s.mu.Unlock()
		}
	}
}

func (s *webServer) createSession(original []byte, filename, originalV1, originalV2 string) (*webSession, error) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupLocked(now)
	if len(s.sessions) >= s.opts.MaxSessions {
		return nil, fmt.Errorf("%w (limit %d); close an old torrent first", errTooManySessions, s.opts.MaxSessions)
	}
	id, err := randomToken(18)
	if err != nil {
		return nil, err
	}
	sess := &webSession{
		ID:         id,
		Filename:   filename,
		Original:   append([]byte(nil), original...),
		Current:    append([]byte(nil), original...),
		OriginalV1: originalV1,
		OriginalV2: originalV2,
		Touched:    now,
	}
	s.sessions[id] = sess
	return sess, nil
}

func (s *webServer) getSession(id string) *webSession {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupLocked(now)
	sess := s.sessions[id]
	if sess != nil {
		sess.Touched = now
	}
	return sess
}

func (s *webServer) deleteSession(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[id]; !ok {
		return false
	}
	delete(s.sessions, id)
	return true
}

func randomToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func cmdWeb(args []string) error {
	fs := flag.NewFlagSet("web [options]", flag.ContinueOnError)
	listen := fs.String("listen", "127.0.0.1:0", "HTTP listen address")
	noBrowser := fs.Bool("no-browser", false, "do not open the browser automatically")
	maxUploadStr := fs.String("max-upload", "64MiB", "maximum uploaded torrent size")
	sessionTTL := fs.Duration("session-ttl", 30*time.Minute, "in-memory session lifetime")
	maxSessions := fs.Int("max-sessions", 20, "maximum number of in-memory torrent sessions")
	help := fs.Bool("help", false, "show help")
	isBool := boolFlags("no-browser", "help")
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
	if len(paths) > 0 {
		return errors.New("web: this command does not take positional arguments")
	}
	maxUpload, err := parseSize(*maxUploadStr)
	if err != nil {
		return fmt.Errorf("--max-upload: %w", err)
	}
	if maxUpload <= 0 {
		return errors.New("--max-upload must be positive")
	}
	if *sessionTTL <= 0 {
		return errors.New("--session-ttl must be positive")
	}
	if *maxSessions <= 0 {
		return errors.New("--max-sessions must be positive")
	}

	srv, err := newWebServer(webOptions{
		MaxUpload:   maxUpload,
		SessionTTL:  *sessionTTL,
		MaxSessions: *maxSessions,
	})
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", *listen, err)
	}
	url := listenerURL(ln)
	if !isLoopbackListen(*listen) {
		fmt.Fprintln(os.Stderr, "warning: web UI listens on a non-loopback address; the API has no user authentication")
	}
	fmt.Fprintln(os.Stdout, url)
	fmt.Fprintf(os.Stderr, "torrent web UI: %s\n", url)

	httpServer := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- httpServer.Serve(ln)
	}()

	if !*noBrowser {
		if err := openBrowser(url); err != nil {
			fmt.Fprintf(os.Stderr, "could not open the browser; open %s manually: %v\n", url, err)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go srv.cleanupLoop(ctx)
	var runErr error
	select {
	case <-ctx.Done():
		fmt.Fprintln(os.Stderr, "shutting down")
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			runErr = err
		}
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		_ = httpServer.Close()
	}
	return runErr
}

func listenerURL(ln net.Listener) string {
	addr := ln.Addr().String()
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://" + addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

func isLoopbackListen(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil || host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "linux":
		cmd = exec.Command("xdg-open", url)
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		return fmt.Errorf("opening a browser is not supported on %s", runtime.GOOS)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
