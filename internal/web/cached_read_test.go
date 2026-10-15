package web

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/basecamp/once-campfire-go/internal/rails"
)

// The fast path may only answer what the ordinary pipeline would answer. Each case warms the
// cache for the signed-in user, changes the database behind the app's back, and expects the next
// request to see the change rather than the cached representation.
func TestCachedReadHonorsChangesBehindTheCache(t *testing.T) {
	warm := func(t *testing.T) (*Server, string, func(path string, headers map[string]string) (int, string, string)) {
		app, _, cookie, user := testApp(t)
		// A day-long cookie for the same session, so a clock moved ahead leaves it valid.
		var token string
		if err := app.DB.Read.QueryRow("SELECT token FROM sessions").Scan(&token); err != nil {
			t.Fatal(err)
		}
		signed, err := app.Secrets.SignCookie("session_token", token, time.Now().Add(24*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		cookie = &http.Cookie{Name: "session_token", Value: rails.EscapeCookie(signed)}
		rooms, _ := app.DB.Rooms(context.Background(), user.ID)
		path := fmt.Sprintf("/rooms/%d", rooms[0].ID)
		get := func(path string, headers map[string]string) (int, string, string) {
			response := cachedRequest(t, app, cookie, "GET", path, headers)
			return response.Code, response.Header().Get("Location") + response.Header().Get("Set-Cookie"), response.Body.String()
		}
		for range 3 {
			if code, _, _ := get(path, nil); code != 200 {
				t.Fatal("warm-up", code)
			}
		}
		if hits := cacheHits(app); hits < 2 {
			t.Fatal("cache never hit", hits)
		}
		return app, path, get
	}

	t.Run("revoked session", func(t *testing.T) {
		app, path, get := warm(t)
		execForeign(t, foreignWriter(t, app), "DELETE FROM sessions")
		code, location, _ := get(path, nil)
		if code != 302 || !strings.Contains(location, "/session/new") {
			t.Fatalf("revoked session served: %d %s", code, location)
		}
	})
	t.Run("banned user", func(t *testing.T) {
		app, path, get := warm(t)
		execForeign(t, foreignWriter(t, app), "UPDATE users SET status=2")
		if code, _, _ := get(path, nil); code != 302 {
			t.Fatalf("banned user served: %d", code)
		}
	})
	t.Run("left the room", func(t *testing.T) {
		app, path, get := warm(t)
		execForeign(t, foreignWriter(t, app), "DELETE FROM memberships")
		if code, _, _ := get(path, nil); code == 200 {
			t.Fatal("removed member served the room")
		}
	})
	t.Run("session goes stale with no commit", func(t *testing.T) {
		app, path, get := warm(t)
		// Only time passes: no commit moves the cache to a new version, so a cached
		// response exists for a session that is now due a refresh.
		app.DB.Now = func() time.Time { return time.Now().Add(2 * time.Hour) }
		code, cookies, _ := get(path, nil)
		if code != 200 || !strings.Contains(cookies, "session_token=") {
			t.Fatalf("session not refreshed: %d %q", code, cookies)
		}
	})
	t.Run("blocked browser", func(t *testing.T) {
		_, path, get := warm(t)
		code, _, body := get(path, map[string]string{"User-Agent": "Mozilla/4.0 (compatible; MSIE 6.0; Windows NT 5.1)"})
		if code != 200 || !strings.Contains(strings.ToLower(body), "browser") || strings.Contains(body, "message-area") {
			t.Fatal("blocked browser received the cached room")
		}
	})
	t.Run("another user's cookie", func(t *testing.T) {
		app, path, get := warm(t)
		_ = app
		code, location, _ := get(path, map[string]string{"Cookie": "session_token=bogus"})
		if code == 200 {
			t.Fatalf("invalid cookie served: %d %s", code, location)
		}
	})
}
