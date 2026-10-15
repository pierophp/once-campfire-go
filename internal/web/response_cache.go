package web

import (
	"bytes"
	"compress/gzip"
	"container/list"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/basecamp/once-campfire-go/internal/database"
	"github.com/basecamp/once-campfire-go/internal/front"
	"github.com/basecamp/once-campfire-go/internal/responsebody"
)

type cachedResponse struct {
	key     string
	version uint64
	body    responsebody.Part
	header  http.Header
	cost    int
}
type responseCache struct {
	mu           sync.Mutex
	entries      map[string]*list.Element
	order        list.List
	limit, size  int
	version      uint64
	hits, misses uint64
}
type responseRound struct {
	version          uint64
	user             int64
	key              string
	gzip, hit, flash bool
}

func newResponseCache(limit int) *responseCache {
	return &responseCache{limit: limit, entries: make(map[string]*list.Element)}
}
func (c *responseCache) get(key string, version uint64) *cachedResponse {
	c.mu.Lock()
	defer c.mu.Unlock()
	if version < c.version {
		c.misses++
		return nil
	}
	if version != c.version {
		c.entries = make(map[string]*list.Element)
		c.order.Init()
		c.size = 0
		c.version = version
	}
	if element := c.entries[key]; element != nil {
		c.order.MoveToFront(element)
		c.hits++
		return element.Value.(*cachedResponse)
	}
	c.misses++
	return nil
}
func (c *responseCache) put(entry *cachedResponse) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry.version != c.version || len(entry.key) > 8192 || entry.cost > c.limit/8 {
		return
	}
	if c.entries[entry.key] != nil {
		return
	}
	for c.size+entry.cost > c.limit && c.order.Len() > 0 {
		oldest := c.order.Back()
		old := oldest.Value.(*cachedResponse)
		delete(c.entries, old.key)
		c.order.Remove(oldest)
		c.size -= old.cost
	}
	c.entries[entry.key] = c.order.PushFront(entry)
	c.size += entry.cost
}
func (s *Server) beginResponseCache(r *http.Request) {
	info := requestMetadata(r.Context())
	if info == nil {
		return
	}
	version, err := s.DB.ResponseVersion(r.Context())
	if err != nil {
		return
	}
	info.databaseVersion = version
	// Reads may reuse session and membership rows of this version; writes always query.
	if r.Method == "GET" || r.Method == "HEAD" {
		info.snapshot = database.Snapshot{Version: version, Valid: true}
	}
	if s.responses.limit == 0 || (r.Method != "GET" && r.Method != "HEAD") || r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
		return
	}
	route, _, err := recognizeRequest(r)
	if err != nil || route == nil {
		return
	}
	switch route.Endpoint {
	case "rooms#show", "messages#index", "users/sidebars#show", "searches#index":
	default:
		return
	}
	encoding := front.ResponseEncoding(r.Header.Get("Accept-Encoding"))
	if encoding == "" {
		return
	}
	state := browserState(r)
	state.load()
	info.response = &responseRound{version: version, gzip: encoding == "gzip", flash: state.values["flash"] != nil}
}
func (s *Server) responseHit(r *http.Request) *cachedResponse {
	info := requestMetadata(r.Context())
	if info == nil || info.response == nil {
		return nil
	}
	round := info.response
	if round.flash || round.user == 0 {
		return nil
	}
	// The version was observed before authentication; the entry lookup requires
	// that exact version, so a request that overlaps a commit is ordered before it.
	round.key = responseKey(r, info, round)
	if len(round.key) > 8192 {
		return nil
	}
	entry := s.responses.get(round.key, round.version)
	round.hit = entry != nil
	return entry
}
func appendField(b []byte, s string) []byte {
	b = strconv.AppendInt(b, int64(len(s)), 10)
	b = append(b, ':')
	return append(b, s...)
}
func appendFields(b []byte, values []string) []byte {
	b = strconv.AppendInt(b, int64(len(values)), 10)
	b = append(b, '[')
	for _, value := range values {
		b = appendField(b, value)
	}
	return append(b, ']')
}

// responseKey length-prefixes every component so distinct requests cannot collide.
func responseKey(r *http.Request, info *requestInfo, round *responseRound) string {
	b := make([]byte, 0, 512)
	b = appendField(b, info.host)
	b = appendField(b, info.origin)
	b = appendField(b, r.RequestURI)
	b = appendField(b, r.URL.RequestURI())
	names := make([]string, 0, len(r.Form))
	for name := range r.Form {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		b = appendField(b, name)
		b = appendFields(b, r.Form[name])
	}
	b = strconv.AppendInt(b, round.user, 10)
	b = appendFields(b, r.Header.Values("Cookie"))
	b = appendFields(b, r.Header.Values("Accept"))
	b = appendFields(b, r.Header.Values("Turbo-Frame"))
	b = appendField(b, r.UserAgent())
	b = appendField(b, r.Header.Get("Origin"))
	b = appendField(b, r.Header.Get("X-Requested-With"))
	b = strconv.AppendBool(b, round.gzip)
	b = appendField(b, os.Getenv("GIT_REVISION"))
	return string(b)
}
func (entry *cachedResponse) serve(w http.ResponseWriter) {
	for key, values := range entry.header {
		w.Header()[key] = append([]string(nil), values...)
	}
	writeParts(w, http.StatusOK, []responsebody.Part{entry.body})
}

// Only completed representations enter the cache; security and session headers
// stay on the live writer. The version was captured before authentication.
func (s *Server) cacheResponse(r *http.Request, w *responseBuffer, parts []responsebody.Part) []responsebody.Part {
	info := requestMetadata(r.Context())
	if info == nil || info.response == nil {
		return parts
	}
	round := info.response
	if round.hit || round.flash || round.key == "" || r.Method != "GET" || w.status != 200 || w.exception ||
		w.Header().Get("Content-Encoding") != "" || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") ||
		strings.Contains(w.Header().Get("Cache-Control"), "no-store") || strings.Contains(w.Header().Get("Cache-Control"), "no-transform") {
		return parts
	}
	version, err := s.DB.ResponseVersion(r.Context())
	if err != nil || version != round.version {
		return parts
	}
	size := 0
	for _, part := range parts {
		size += part.Len()
	}
	if size+len(round.key)+1024 > s.responses.limit/8 {
		return parts
	}
	var body bytes.Buffer
	body.Grow(size)
	for _, part := range parts {
		if _, err = part.WriteTo(&body); err != nil {
			return parts
		}
	}
	if round.gzip {
		var compressed bytes.Buffer
		writer, _ := gzip.NewWriterLevel(&compressed, 6)
		writer.Header.OS = 3
		if stamp, err := http.ParseTime(w.Header().Get("Last-Modified")); err == nil && stamp.Unix() > 0 {
			writer.Header.ModTime = time.Unix(stamp.Unix(), 0)
		}
		if _, err = writer.Write(body.Bytes()); err != nil {
			return parts
		}
		if err = writer.Close(); err != nil {
			return parts
		}
		body = compressed
	}
	header := make(http.Header)
	for _, name := range []string{"Content-Type", "ETag", "Last-Modified", "Cache-Control", "Link", "Vary"} {
		if values := w.Header().Values(name); len(values) > 0 {
			header[http.CanonicalHeaderKey(name)] = append([]string(nil), values...)
		}
	}
	if !strings.Contains(header.Get("Vary"), "Accept-Encoding") {
		header.Add("Vary", "Accept-Encoding")
	}
	if round.gzip {
		header.Set("Content-Encoding", "gzip")
	}
	entry := &cachedResponse{key: round.key, version: round.version, body: responsebody.NewPart(body.Bytes()), header: header}
	entry.cost = body.Cap() + len(entry.key) + 1024
	for name, values := range header {
		entry.cost += len(name)
		for _, value := range values {
			entry.cost += len(value)
		}
	}
	version, err = s.DB.ResponseVersion(r.Context())
	if err == nil && version == round.version {
		s.responses.put(entry)
	}
	for name, values := range header {
		w.Header()[name] = values
	}
	return []responsebody.Part{entry.body}
}
func responseCacheBudget() (int, error) {
	raw, ok := os.LookupEnv("CAMPFIRE_RESPONSE_CACHE_MB")
	if !ok || raw == "" {
		return 64 << 20, nil
	}
	mb, err := strconv.Atoi(raw)
	if err != nil || mb < 0 || mb > 1024 {
		return 0, strconv.ErrSyntax
	}
	return mb << 20, nil
}
