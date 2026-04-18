package credentials

import (
	"context"
	"errors"
	"testing"

	"github.com/hiclaw/hiclaw-controller/internal/credprovider"
)

type fakeProvider struct {
	lastReq credprovider.IssueRequest
	resp    *credprovider.IssueResponse
	err     error
}

func (f *fakeProvider) Issue(_ context.Context, req credprovider.IssueRequest) (*credprovider.IssueResponse, error) {
	f.lastReq = req
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

func TestIssueWorkerToken_DelegatesToProvider(t *testing.T) {
	fake := &fakeProvider{
		resp: &credprovider.IssueResponse{
			AccessKeyID:     "STS.test-ak",
			AccessKeySecret: "test-sk",
			SecurityToken:   "test-token",
			Expiration:      "2026-03-26T12:00:00Z",
			ExpiresInSec:    3600,
			Endpoint:        "oss-cn-hangzhou-internal.aliyuncs.com",
		},
	}
	svc := NewSTSService(STSConfig{OSSBucket: "test-bucket"}, fake)

	tok, err := svc.IssueWorkerToken(context.Background(), "alice")
	if err != nil {
		t.Fatalf("IssueWorkerToken: %v", err)
	}
	if tok.AccessKeyID != "STS.test-ak" {
		t.Errorf("AccessKeyID = %q, want STS.test-ak", tok.AccessKeyID)
	}
	if tok.SecurityToken != "test-token" {
		t.Errorf("SecurityToken = %q, want test-token", tok.SecurityToken)
	}
	if tok.OSSEndpoint != "oss-cn-hangzhou-internal.aliyuncs.com" {
		t.Errorf("OSSEndpoint = %q, want internal endpoint", tok.OSSEndpoint)
	}
	if tok.OSSBucket != "test-bucket" {
		t.Errorf("OSSBucket = %q, want test-bucket", tok.OSSBucket)
	}
	if tok.ExpiresInSec != 3600 {
		t.Errorf("ExpiresInSec = %d, want 3600", tok.ExpiresInSec)
	}
	if fake.lastReq.Role != credprovider.RoleWorker {
		t.Errorf("provider role = %q, want worker", fake.lastReq.Role)
	}
	if fake.lastReq.Name != "alice" {
		t.Errorf("provider name = %q, want alice", fake.lastReq.Name)
	}
	if fake.lastReq.Bucket != "test-bucket" {
		t.Errorf("provider bucket = %q, want test-bucket", fake.lastReq.Bucket)
	}
}

func TestIssueWorkerToken_ProviderError(t *testing.T) {
	svc := NewSTSService(STSConfig{OSSBucket: "b"}, &fakeProvider{err: errors.New("boom")})
	if _, err := svc.IssueWorkerToken(context.Background(), "alice"); err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestConfigured_NilProvider(t *testing.T) {
	svc := NewSTSService(STSConfig{}, nil)
	if svc.Configured() {
		t.Fatal("Configured() = true with nil provider, want false")
	}
	if _, err := svc.IssueWorkerToken(context.Background(), "x"); err == nil {
		t.Fatal("expected error from unconfigured service")
	}
}
