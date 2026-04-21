package matrix

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEnsureUser_NewRegistration(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_matrix/client/v3/register":
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{
				"user_id":      "@alice:test.domain",
				"access_token": "token-abc",
			})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := NewTuwunelClient(Config{
		ServerURL:         server.URL,
		Domain:            "test.domain",
		RegistrationToken: "reg-secret",
	}, server.Client())

	creds, err := c.EnsureUser(context.Background(), EnsureUserRequest{
		Username: "alice",
		Password: "pass123",
	})
	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	if !creds.Created {
		t.Error("expected Created=true for new registration")
	}
	if creds.UserID != "@alice:test.domain" {
		t.Errorf("UserID = %q, want @alice:test.domain", creds.UserID)
	}
	if creds.AccessToken != "token-abc" {
		t.Errorf("AccessToken = %q, want token-abc", creds.AccessToken)
	}
	if creds.Password != "pass123" {
		t.Errorf("Password = %q, want pass123", creds.Password)
	}
}

func TestEnsureUser_ExistingUser(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_matrix/client/v3/register":
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{
				"errcode": "M_USER_IN_USE",
				"error":   "User ID already taken",
			})
		case "/_matrix/client/v3/login":
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{
				"access_token": "login-token-xyz",
			})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := NewTuwunelClient(Config{
		ServerURL:         server.URL,
		Domain:            "test.domain",
		RegistrationToken: "reg-secret",
	}, server.Client())

	creds, err := c.EnsureUser(context.Background(), EnsureUserRequest{
		Username: "bob",
		Password: "existing-pass",
	})
	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	if creds.Created {
		t.Error("expected Created=false for existing user")
	}
	if creds.AccessToken != "login-token-xyz" {
		t.Errorf("AccessToken = %q, want login-token-xyz", creds.AccessToken)
	}
}

func TestEnsureUser_GeneratesPassword(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{
			"user_id":      "@gen:test.domain",
			"access_token": "tok",
		})
	}))
	defer server.Close()

	c := NewTuwunelClient(Config{
		ServerURL:         server.URL,
		Domain:            "test.domain",
		RegistrationToken: "reg-secret",
	}, server.Client())

	creds, err := c.EnsureUser(context.Background(), EnsureUserRequest{Username: "gen"})
	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	if len(creds.Password) != 32 { // 16 bytes hex = 32 chars
		t.Errorf("generated password length = %d, want 32", len(creds.Password))
	}
}

func TestCreateRoom(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_matrix/client/v3/createRoom" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer creator-token" {
			t.Errorf("Authorization = %q, want Bearer creator-token", auth)
		}

		var body map[string]interface{}
		json.NewDecoder(r.Body).Decode(&body)

		if body["preset"] != "trusted_private_chat" {
			t.Errorf("preset = %v, want trusted_private_chat", body["preset"])
		}

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{
			"room_id": "!room123:test.domain",
		})
	}))
	defer server.Close()

	c := NewTuwunelClient(Config{
		ServerURL: server.URL,
		Domain:    "test.domain",
	}, server.Client())

	info, err := c.CreateRoom(context.Background(), CreateRoomRequest{
		Name:         "Worker: alice",
		Topic:        "Communication channel",
		Invite:       []string{"@admin:test.domain", "@alice:test.domain"},
		CreatorToken: "creator-token",
		PowerLevels: map[string]int{
			"@admin:test.domain": 100,
			"@alice:test.domain": 0,
		},
	})
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	if !info.Created {
		t.Error("expected Created=true")
	}
	if info.RoomID != "!room123:test.domain" {
		t.Errorf("RoomID = %q, want !room123:test.domain", info.RoomID)
	}
}

func TestCreateRoom_ExistingRoomID(t *testing.T) {
	c := NewTuwunelClient(Config{ServerURL: "http://unused", Domain: "d"}, nil)
	info, err := c.CreateRoom(context.Background(), CreateRoomRequest{
		ExistingRoomID: "!existing:domain",
	})
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	if info.Created {
		t.Error("expected Created=false for existing room ID")
	}
	if info.RoomID != "!existing:domain" {
		t.Errorf("RoomID = %q, want !existing:domain", info.RoomID)
	}
}

// adminLoginHandler returns a handler that responds to admin login with a
// fixed token, allowing tests that exercise admin-driven endpoints.
func adminLoginHandler(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"access_token": "admin-token"})
}

func TestListRoomMembers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_matrix/client/v3/login":
			adminLoginHandler(t, w)
		case "/_matrix/client/v3/rooms/!room:d/members":
			if auth := r.Header.Get("Authorization"); auth != "Bearer admin-token" {
				t.Errorf("Authorization = %q, want Bearer admin-token", auth)
			}
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"chunk": []map[string]interface{}{
					{"state_key": "@alice:d", "content": map[string]string{"membership": "join"}},
					{"state_key": "@bob:d", "content": map[string]string{"membership": "invite"}},
					{"state_key": "@carol:d", "content": map[string]string{"membership": "leave"}},
					{"state_key": "@dave:d", "content": map[string]string{"membership": "ban"}},
					{"state_key": "", "content": map[string]string{"membership": "join"}},
				},
			})
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := NewTuwunelClient(Config{
		ServerURL:     server.URL,
		Domain:        "d",
		AdminUser:     "admin",
		AdminPassword: "pw",
	}, server.Client())

	members, err := c.ListRoomMembers(context.Background(), "!room:d")
	if err != nil {
		t.Fatalf("ListRoomMembers: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("got %d members, want 2 (filtered join+invite); members=%+v", len(members), members)
	}
	if members[0].UserID != "@alice:d" || members[0].Membership != "join" {
		t.Errorf("members[0] = %+v, want {@alice:d join}", members[0])
	}
	if members[1].UserID != "@bob:d" || members[1].Membership != "invite" {
		t.Errorf("members[1] = %+v, want {@bob:d invite}", members[1])
	}
}

func TestInviteToRoom_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_matrix/client/v3/login":
			adminLoginHandler(t, w)
		case "/_matrix/client/v3/rooms/!room:d/invite":
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			if body["user_id"] != "@alice:d" {
				t.Errorf("user_id = %q, want @alice:d", body["user_id"])
			}
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("{}"))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := NewTuwunelClient(Config{
		ServerURL: server.URL, Domain: "d", AdminUser: "admin", AdminPassword: "pw",
	}, server.Client())
	if err := c.InviteToRoom(context.Background(), "!room:d", "@alice:d"); err != nil {
		t.Fatalf("InviteToRoom: %v", err)
	}
}

func TestInviteToRoom_Idempotent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_matrix/client/v3/login":
			adminLoginHandler(t, w)
		case "/_matrix/client/v3/rooms/!room:d/invite":
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(map[string]string{
				"errcode": "M_FORBIDDEN",
				"error":   "@alice:d is already in the room.",
			})
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := NewTuwunelClient(Config{
		ServerURL: server.URL, Domain: "d", AdminUser: "admin", AdminPassword: "pw",
	}, server.Client())
	if err := c.InviteToRoom(context.Background(), "!room:d", "@alice:d"); err != nil {
		t.Errorf("expected nil for already-in-room, got %v", err)
	}
}

func TestInviteToRoom_RealError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_matrix/client/v3/login":
			adminLoginHandler(t, w)
		case "/_matrix/client/v3/rooms/!room:d/invite":
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(map[string]string{
				"errcode": "M_FORBIDDEN",
				"error":   "inviter has insufficient power level",
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := NewTuwunelClient(Config{
		ServerURL: server.URL, Domain: "d", AdminUser: "admin", AdminPassword: "pw",
	}, server.Client())
	if err := c.InviteToRoom(context.Background(), "!room:d", "@alice:d"); err == nil {
		t.Error("expected error for unrelated 403, got nil")
	}
}

func TestKickFromRoom_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_matrix/client/v3/login":
			adminLoginHandler(t, w)
		case "/_matrix/client/v3/rooms/!room:d/kick":
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			if body["user_id"] != "@alice:d" {
				t.Errorf("user_id = %q", body["user_id"])
			}
			if body["reason"] != "access revoked" {
				t.Errorf("reason = %q", body["reason"])
			}
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("{}"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := NewTuwunelClient(Config{
		ServerURL: server.URL, Domain: "d", AdminUser: "admin", AdminPassword: "pw",
	}, server.Client())
	if err := c.KickFromRoom(context.Background(), "!room:d", "@alice:d", "access revoked"); err != nil {
		t.Fatalf("KickFromRoom: %v", err)
	}
}

func TestKickFromRoom_Idempotent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_matrix/client/v3/login":
			adminLoginHandler(t, w)
		case "/_matrix/client/v3/rooms/!room:d/kick":
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(map[string]string{
				"errcode": "M_FORBIDDEN",
				"error":   "User @alice:d is not in the room.",
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := NewTuwunelClient(Config{
		ServerURL: server.URL, Domain: "d", AdminUser: "admin", AdminPassword: "pw",
	}, server.Client())
	if err := c.KickFromRoom(context.Background(), "!room:d", "@alice:d", ""); err != nil {
		t.Errorf("expected nil for not-in-room, got %v", err)
	}
}

func TestUserID(t *testing.T) {
	c := NewTuwunelClient(Config{Domain: "matrix.example.com:8080"}, nil)
	got := c.UserID("alice")
	want := "@alice:matrix.example.com:8080"
	if got != want {
		t.Errorf("UserID = %q, want %q", got, want)
	}
}

func TestGeneratePassword(t *testing.T) {
	p1, err := GeneratePassword(16)
	if err != nil {
		t.Fatal(err)
	}
	if len(p1) != 32 {
		t.Errorf("len = %d, want 32", len(p1))
	}

	p2, _ := GeneratePassword(16)
	if p1 == p2 {
		t.Error("two generated passwords should not be equal")
	}
}
