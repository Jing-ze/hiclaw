// Package credprovider talks to the hiclaw-credential-provider sidecar to
// obtain short-lived Alibaba Cloud STS tokens.
//
// The sidecar is the only component in the controller that is allowed to
// hold (or derive) long-lived Alibaba Cloud identity material — an RRSA
// OIDC projected token, a pod-identity OIDC token, or (in the
// mock-credential-provider) a long-lived AccessKey pair. The controller
// itself is credential-less: whenever it needs to talk to APIG, OSS, or
// any other Alibaba Cloud service, it asks the sidecar for a fresh STS
// triple via this package.
package credprovider

// Role identifies the logical caller requesting a token and determines the
// scope of the inline policy attached to the resulting STS token.
type Role string

const (
	// RoleWorker: a normal Worker Agent. Scoped to agents/<name>/* and
	// shared/* in the configured OSS bucket.
	RoleWorker Role = "worker"

	// RoleManager: the Manager Agent. Adds write access to the manager/*
	// prefix for workspace sync.
	RoleManager Role = "manager"

	// RoleController: the hiclaw-controller itself. No inline policy is
	// attached: the caller inherits the full permission set granted by
	// the RAM role (expected to include OSS read/write plus APIG
	// consumer management). Used for bootstrap OSS operations and APIG
	// SDK calls.
	RoleController Role = "controller"
)

// IssueRequest is the body sent to the sidecar's POST /issue endpoint.
type IssueRequest struct {
	Role            Role   `json:"role"`
	Name            string `json:"name,omitempty"`
	Bucket          string `json:"bucket,omitempty"`
	DurationSeconds int    `json:"duration_seconds,omitempty"`
}

// IssueResponse is the sidecar's reply to POST /issue.
type IssueResponse struct {
	AccessKeyID     string `json:"access_key_id"`
	AccessKeySecret string `json:"access_key_secret"`
	SecurityToken   string `json:"security_token"`
	Expiration      string `json:"expiration"`
	ExpiresInSec    int    `json:"expires_in_sec"`
	Endpoint        string `json:"endpoint"`
}
