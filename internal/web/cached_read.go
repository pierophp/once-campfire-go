package web

import (
	"net/http"
	"strconv"

	"github.com/basecamp/once-campfire-go/internal/database"
)

// Routes whose finished responses enter the response cache (see beginResponseCache).
var cachedReadPatterns = map[string]bool{
	"/rooms/:id(.:format)":               true,
	"/rooms/:room_id/messages(.:format)": true,
	"/users/:user_id/sidebar(.:format)":  true,
	"/searches(.:format)":                true,
}

// cachedRead answers a cached read before the router, authentication wrapper and handler run.
// It only looks: it verifies the session cookie, loads the user, checks that the session needs
// no refresh and the browser is allowed, resolves room membership and probes the cache. If any
// of that does not hold, or the cache misses, it has changed nothing but request-local state and
// the ordinary handler runs from the start. A hit performs what the handlers' own hit branch
// performs (the last_room cookie for rooms, then the stored representation), so the response is
// the same bytes.
func (s *Server) cachedRead(w http.ResponseWriter, r *http.Request, route *routeContract) bool {
	if !cachedReadPatterns[route.Pattern] || (r.Method != "GET" && r.Method != "HEAD") {
		return false
	}
	info := requestMetadata(r.Context())
	if info == nil || info.response == nil || info.response.flash {
		return false
	}
	cookie, err := r.Cookie("session_token")
	if err != nil {
		return false
	}
	token, err := s.Secrets.VerifyEscapedStringCookie("session_token", cookie.Value, s.DB.Now())
	if err != nil || token == "" {
		return false
	}
	ctx := r.Context()
	user, err := s.DB.SessionUser(ctx, token)
	if err != nil {
		return false
	}
	if stale, err := s.DB.SessionStale(ctx, token); err != nil || stale {
		return false
	}
	if blocked, _ := requestAgent(r).Blocked(); blocked {
		return false
	}
	var room database.Room
	inRoom := route.Endpoint == "rooms#show" || route.Endpoint == "messages#index"
	if inRoom {
		if room, err = s.DB.Room(ctx, user.ID, roomID(r)); err != nil {
			return false
		}
	}
	info.response.user = user.ID
	hit := s.responseHit(r)
	if hit == nil {
		return false
	}
	if route.Endpoint == "rooms#show" {
		s.rememberRoom(w, r, strconv.FormatInt(room.ID, 10))
	}
	hit.serve(w)
	return true
}
