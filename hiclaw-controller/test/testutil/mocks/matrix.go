package mocks

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/hiclaw/hiclaw-controller/internal/matrix"
)

// MockMatrixClient implements matrix.Client for integration tests. The
// state model tracks (a) which users exist with their credentials, and
// (b) which users are currently "joined" to each room. Tests can drive
// the mock either through the default stateful behaviour or via *Fn
// overrides. Unused methods (CreateRoom / SendMessage / Login) panic
// on call to surface unexpected reliance on the real Matrix client.
type MockMatrixClient struct {
	mu sync.Mutex

	// Domain is used to build full Matrix user IDs. Default "localhost".
	Domain string

	// users maps username (localpart) → credentials.
	users map[string]*matrix.UserCredentials

	// tokens maps access token → username (localpart) for reverse lookup
	// on JoinRoom / LeaveRoom, which are token-based per the interface.
	tokens map[string]string

	// rooms maps roomID → (matrixUserID → joined?). An entry for a room
	// is auto-created on first Invite/Join to avoid tests needing to
	// preseed rooms.
	rooms map[string]map[string]bool

	// Fn overrides.
	EnsureUserFn      func(ctx context.Context, req matrix.EnsureUserRequest) (*matrix.UserCredentials, error)
	JoinRoomFn        func(ctx context.Context, roomID, userToken string) error
	LeaveRoomFn       func(ctx context.Context, roomID, userToken string) error
	InviteRoomFn      func(ctx context.Context, roomID, inviteeMatrixID string) error
	KickRoomFn        func(ctx context.Context, roomID, targetMatrixID, reason string) error
	ListRoomMembersFn func(ctx context.Context, roomID string) ([]string, error)
	DeactivateUserFn  func(ctx context.Context, username string) error

	Calls struct {
		EnsureUser      []string
		JoinRoom        []MatrixJoinCall
		LeaveRoom       []MatrixLeaveCall
		InviteRoom      []MatrixInviteCall
		KickRoom        []MatrixKickCall
		ListRoomMembers []string
		DeactivateUser  []string
	}
}

// MatrixJoinCall records a JoinRoom invocation.
type MatrixJoinCall struct {
	RoomID string
	Token  string
}

// MatrixLeaveCall records a LeaveRoom invocation.
type MatrixLeaveCall struct {
	RoomID string
	Token  string
}

// MatrixInviteCall records an InviteRoom invocation.
type MatrixInviteCall struct {
	RoomID    string
	InviteeID string
}

// MatrixKickCall records a KickRoom invocation.
type MatrixKickCall struct {
	RoomID   string
	TargetID string
	Reason   string
}

// NewMockMatrixClient constructs a MockMatrixClient with empty state.
func NewMockMatrixClient() *MockMatrixClient {
	return &MockMatrixClient{
		Domain: "localhost",
		users:  make(map[string]*matrix.UserCredentials),
		tokens: make(map[string]string),
		rooms:  make(map[string]map[string]bool),
	}
}

// Reset clears all state, Fn overrides, and call records.
func (m *MockMatrixClient) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.users = make(map[string]*matrix.UserCredentials)
	m.tokens = make(map[string]string)
	m.rooms = make(map[string]map[string]bool)
	m.EnsureUserFn = nil
	m.JoinRoomFn = nil
	m.LeaveRoomFn = nil
	m.InviteRoomFn = nil
	m.KickRoomFn = nil
	m.ListRoomMembersFn = nil
	m.DeactivateUserFn = nil
	m.clearCallsLocked()
}

// ClearCalls resets only the Calls records, preserving user/room state
// and Fn overrides.
func (m *MockMatrixClient) ClearCalls() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.clearCallsLocked()
}

func (m *MockMatrixClient) clearCallsLocked() {
	m.Calls = struct {
		EnsureUser      []string
		JoinRoom        []MatrixJoinCall
		LeaveRoom       []MatrixLeaveCall
		InviteRoom      []MatrixInviteCall
		KickRoom        []MatrixKickCall
		ListRoomMembers []string
		DeactivateUser  []string
	}{}
}

// UserID builds a full Matrix user ID from a localpart.
func (m *MockMatrixClient) UserID(localpart string) string {
	m.mu.Lock()
	domain := m.Domain
	m.mu.Unlock()
	if domain == "" {
		domain = "localhost"
	}
	return fmt.Sprintf("@%s:%s", localpart, domain)
}

// EnsureUser registers or logs in a user. Default: generate deterministic
// "mock-pw-<name>" when password empty; return access token "mock-token-<name>".
// Created=true only on first call for the username.
func (m *MockMatrixClient) EnsureUser(ctx context.Context, req matrix.EnsureUserRequest) (*matrix.UserCredentials, error) {
	m.mu.Lock()
	m.Calls.EnsureUser = append(m.Calls.EnsureUser, req.Username)
	fn := m.EnsureUserFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, req)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	existing, ok := m.users[req.Username]
	if ok {
		// Existing user: return cached creds with Created=false.
		out := *existing
		out.Created = false
		return &out, nil
	}

	password := req.Password
	if password == "" {
		password = "mock-pw-" + req.Username
	}
	creds := &matrix.UserCredentials{
		UserID:      fmt.Sprintf("@%s:%s", req.Username, m.defaultDomainLocked()),
		AccessToken: "mock-token-" + req.Username,
		Password:    password,
		Created:     true,
	}
	m.users[req.Username] = creds
	m.tokens[creds.AccessToken] = req.Username
	return creds, nil
}

// JoinRoom marks the user corresponding to userToken as joined to roomID.
func (m *MockMatrixClient) JoinRoom(ctx context.Context, roomID, userToken string) error {
	m.mu.Lock()
	m.Calls.JoinRoom = append(m.Calls.JoinRoom, MatrixJoinCall{RoomID: roomID, Token: userToken})
	fn := m.JoinRoomFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, roomID, userToken)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	localpart, ok := m.tokens[userToken]
	if !ok {
		return fmt.Errorf("mock matrix: unknown token %q", userToken)
	}
	matrixID := fmt.Sprintf("@%s:%s", localpart, m.defaultDomainLocked())
	if m.rooms[roomID] == nil {
		m.rooms[roomID] = make(map[string]bool)
	}
	m.rooms[roomID][matrixID] = true
	return nil
}

// LeaveRoom marks the user corresponding to userToken as no longer joined.
func (m *MockMatrixClient) LeaveRoom(ctx context.Context, roomID, userToken string) error {
	m.mu.Lock()
	m.Calls.LeaveRoom = append(m.Calls.LeaveRoom, MatrixLeaveCall{RoomID: roomID, Token: userToken})
	fn := m.LeaveRoomFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, roomID, userToken)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	localpart, ok := m.tokens[userToken]
	if !ok {
		return fmt.Errorf("mock matrix: unknown token %q", userToken)
	}
	matrixID := fmt.Sprintf("@%s:%s", localpart, m.defaultDomainLocked())
	if m.rooms[roomID] != nil {
		delete(m.rooms[roomID], matrixID)
	}
	return nil
}

// InviteRoom is idempotent: marks the invitee as joined directly, since
// HiClaw worker provisioning auto-accepts invites in the real client.
func (m *MockMatrixClient) InviteRoom(ctx context.Context, roomID, inviteeMatrixID string) error {
	m.mu.Lock()
	m.Calls.InviteRoom = append(m.Calls.InviteRoom, MatrixInviteCall{RoomID: roomID, InviteeID: inviteeMatrixID})
	fn := m.InviteRoomFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, roomID, inviteeMatrixID)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rooms[roomID] == nil {
		m.rooms[roomID] = make(map[string]bool)
	}
	m.rooms[roomID][inviteeMatrixID] = true
	return nil
}

// KickRoom is idempotent: removes the target from the room if present.
func (m *MockMatrixClient) KickRoom(ctx context.Context, roomID, targetMatrixID, reason string) error {
	m.mu.Lock()
	m.Calls.KickRoom = append(m.Calls.KickRoom, MatrixKickCall{RoomID: roomID, TargetID: targetMatrixID, Reason: reason})
	fn := m.KickRoomFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, roomID, targetMatrixID, reason)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rooms[roomID] != nil {
		delete(m.rooms[roomID], targetMatrixID)
	}
	return nil
}

// ListRoomMembers returns the sorted set of Matrix user IDs currently
// joined to the given room.
func (m *MockMatrixClient) ListRoomMembers(ctx context.Context, roomID string) ([]string, error) {
	m.mu.Lock()
	m.Calls.ListRoomMembers = append(m.Calls.ListRoomMembers, roomID)
	fn := m.ListRoomMembersFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, roomID)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	room := m.rooms[roomID]
	out := make([]string, 0, len(room))
	for userID, joined := range room {
		if joined {
			out = append(out, userID)
		}
	}
	sort.Strings(out)
	return out, nil
}

// DeactivateUser removes the user from the mock's user table.
func (m *MockMatrixClient) DeactivateUser(ctx context.Context, username string) error {
	m.mu.Lock()
	m.Calls.DeactivateUser = append(m.Calls.DeactivateUser, username)
	fn := m.DeactivateUserFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, username)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if creds, ok := m.users[username]; ok {
		delete(m.tokens, creds.AccessToken)
		delete(m.users, username)
	}
	return nil
}

// CreateRoom is not used by HumanReconciler or Provisioner-through-mock
// code paths; if a future reconciler relies on it, the panic surfaces
// the missing stub immediately.
func (m *MockMatrixClient) CreateRoom(ctx context.Context, req matrix.CreateRoomRequest) (*matrix.RoomInfo, error) {
	panic("mock matrix: CreateRoom not implemented")
}

// SendMessage is not exercised by reconcilers; the real path goes
// through the Tuwunel client directly in the Worker agent runtime.
func (m *MockMatrixClient) SendMessage(ctx context.Context, roomID, token, body string) error {
	panic("mock matrix: SendMessage not implemented")
}

// Login is reserved for debugging flows; reconcilers use EnsureUser
// instead.
func (m *MockMatrixClient) Login(ctx context.Context, username, password string) (string, error) {
	panic("mock matrix: Login not implemented")
}

// --- test helpers ---

// IsUserInRoom reports whether matrixUserID is currently joined to roomID.
func (m *MockMatrixClient) IsUserInRoom(roomID, matrixUserID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	room := m.rooms[roomID]
	return room != nil && room[matrixUserID]
}

// RoomMembers returns a sorted snapshot of the joined Matrix user IDs.
func (m *MockMatrixClient) RoomMembers(roomID string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	room := m.rooms[roomID]
	out := make([]string, 0, len(room))
	for userID, joined := range room {
		if joined {
			out = append(out, userID)
		}
	}
	sort.Strings(out)
	return out
}

// JoinCallCount returns len(Calls.JoinRoom) under the mutex.
func (m *MockMatrixClient) JoinCallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.Calls.JoinRoom)
}

// LeaveCallCount returns len(Calls.LeaveRoom) under the mutex.
func (m *MockMatrixClient) LeaveCallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.Calls.LeaveRoom)
}

// UserTokens returns a shallow copy of the token → username map for
// tests that need to map a token back to the owning Human.
func (m *MockMatrixClient) UserTokens() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]string, len(m.tokens))
	for k, v := range m.tokens {
		out[k] = v
	}
	return out
}

// defaultDomainLocked returns the configured domain, substituting the
// default "localhost" when unset. Assumes m.mu is held.
func (m *MockMatrixClient) defaultDomainLocked() string {
	if m.Domain == "" {
		return "localhost"
	}
	return m.Domain
}

var _ matrix.Client = (*MockMatrixClient)(nil)
