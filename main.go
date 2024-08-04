package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	log "log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

func init() {
	var logLevel = log.LevelDebug
	// parse log level from env
	if envLogLevel := os.Getenv("LOG_LEVEL"); envLogLevel != "" {
		if err := logLevel.UnmarshalText([]byte(envLogLevel)); err != nil {
			log.Error("failed to parse log level", "error", err)
		}
	}
	log.SetDefault(log.New(log.NewTextHandler(os.Stdout, &log.HandlerOptions{
		Level: logLevel,
	})))
}

type Object struct {
	Etag        string
	ContentType string
	Content     []byte
	UpdateTime  time.Time
	ExpiryTime  time.Time
}

type Cache map[string]map[string]Object

func (c Cache) Get(hostName, pageName string) (Object, bool) {
	host, ok := c[hostName]
	if !ok {
		return Object{}, false
	}
	obj, ok := host[pageName]
	return obj, ok
}

func (c Cache) Put(hostName, pageName string, obj Object) {
	host, ok := c[hostName]
	if !ok {
		host = make(map[string]Object)
		c[hostName] = host
	}
	host[pageName] = obj
}

func NewStorage(ctx context.Context) (*Storage, error) {
	var allowedHostNames = map[string]struct{}{
		"https://paulgraham.com": {},
	}

	cache := make(Cache)
	return &Storage{
		allowed: allowedHostNames,
		cache:   cache,
	}, nil
}

type Storage struct {
	allowed map[string]struct{}
	cache   Cache
}

func (s *Storage) Get(ctx context.Context, hostName, pageName string) (obj Object, err error) {
	if hostName == "" {
		return Object{}, fmt.Errorf("host name is empty")
	}
	if pageName == "" {
		return Object{}, fmt.Errorf("page name is empty")
	}

	if _, ok := s.allowed[hostName]; !ok {
		log.Error("host not allowed", "host", hostName)
		return Object{}, fmt.Errorf("host not allowed")
	}

	obj, ok := s.cache.Get(hostName, pageName)
	if ok && obj.ExpiryTime.After(time.Now()) {
		log.Debug("cache hit", "host", hostName, "object", pageName)
		return obj, nil
	}

	// get object from web page
	url := fmt.Sprintf("%s/%s", hostName, pageName)

	resp, err := http.Get(url)
	if err != nil {
		log.Error("failed to get object", "url", url, "error", err)
		return Object{}, fmt.Errorf("failed to get object")
	}

	defer resp.Body.Close()
	content, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Error("failed to read object", "url", url, "error", err)
		return Object{}, fmt.Errorf("failed to read object")
	}

	attrs := resp.Header

	// get md5 hash of content
	hash := md5.New()
	hash.Write(content)
	etag := hex.EncodeToString(hash.Sum(nil))

	obj = Object{
		Etag:        etag,
		ContentType: attrs.Get("Content-Type"),
		Content:     content,
		UpdateTime:  time.Now(),
		ExpiryTime:  time.Now().Add(24 * time.Hour),
	}

	s.cache.Put(hostName, pageName, obj)

	return obj, nil
}

func main() {
	ctx := context.Background()
	s, err := NewStorage(ctx)
	if err != nil {
		fatalf("failed to create storage: %+v", err)
	}

	router := http.NewServeMux()

	router.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	router.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		url := r.URL.Query().Get("url")
		var prefix string

		if strings.HasPrefix(url, "https://") {
			prefix = "https://"
			// remove prefix from x
			url = strings.TrimPrefix(url, "https://")
		} else if strings.HasPrefix(url, "http://") {
			prefix = "http://"
			url = strings.TrimPrefix(url, "http://")
		} else {
			prefix = "httpd://"
		}

		pathSegments := strings.SplitN(strings.TrimRight(url, "/"), "/", 2)
		if len(pathSegments) != 2 {
			log.Error("invalid path", "path", r.URL.Path)
			http.NotFound(w, r)
			return
		}

		hostName := pathSegments[0]
		pageName := pathSegments[1]

		hostName = prefix + hostName

		log.Info("get object", "host", hostName, "page", pageName)

		obj, err := s.Get(ctx, hostName, pageName)
		if err != nil {
			http.NotFound(w, r)
			return
		}

		w.Header().Set("Content-Type", obj.ContentType)
		w.Header().Set("ETag", obj.Etag)
		// set cors header
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		http.ServeContent(w, r, pageName, obj.UpdateTime, bytes.NewReader(obj.Content))
	})

	err = http.ListenAndServe(":8080", router)
	if err != nil {
		fatalf("http server failed: %+v", err)
	}
}

func fatalf(format string, args ...interface{}) {
	fmt.Printf(format, args...)
	os.Exit(1)
}
