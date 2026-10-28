package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"

	"google.golang.org/protobuf/proto"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
)

// Phase 5: the node owner's local controls ("This machine": restrict only),
// used by `mycel node share|grants|freeze`.

// LocalFreeze is GET /api/local/freeze.
type LocalFreeze struct {
	Frozen bool   `json:"frozen"`
	Scope  string `json:"scope"`
	Reason string `json:"reason"`
}

// Sharing returns the hub, local and effective sharing settings of this node.
func (c *Client) Sharing(ctx context.Context) (*mycelv1.GetNodeSharingResponse, error) {
	var out mycelv1.GetNodeSharingResponse
	if err := c.do(ctx, http.MethodGet, "/api/local/sharing", nil, decodeProto(&out)); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetSharing stores the local ceiling and returns the new effective settings.
func (c *Client) SetSharing(ctx context.Context, s *mycelv1.SharingSettings) (*mycelv1.SharingSettings, error) {
	body, err := marshal.Marshal(s)
	if err != nil {
		return nil, err
	}
	var out mycelv1.SharingSettings
	if err := c.do(ctx, http.MethodPut, "/api/local/sharing", body, decodeProto(&out)); err != nil {
		return nil, err
	}
	return &out, nil
}

// LocalGrants lists the grants attached on this node. The reply is read as
// {"grants":[Grant…]} (ListGrantsResponse).
func (c *Client) LocalGrants(ctx context.Context) ([]*mycelv1.Grant, error) {
	var out mycelv1.ListGrantsResponse
	if err := c.do(ctx, http.MethodGet, "/api/local/grants", nil, decodeProto(&out)); err != nil {
		return nil, err
	}
	return out.GetGrants(), nil
}

// RevokeLocalGrant detaches a grant on this node at once.
func (c *Client) RevokeLocalGrant(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/api/local/grants/"+url.PathEscape(id), nil, nil)
}

// decodeProto decodes a protojson reply into m; an empty body leaves m empty.
func decodeProto(m proto.Message) func([]byte) error {
	return func(b []byte) error {
		if len(bytes.TrimSpace(b)) == 0 {
			return nil
		}
		return unmarshal.Unmarshal(b, m)
	}
}

// Freeze returns this node's freeze state.
func (c *Client) Freeze(ctx context.Context) (*LocalFreeze, error) {
	var out LocalFreeze
	if err := c.do(ctx, http.MethodGet, "/api/local/freeze", nil, func(b []byte) error { return json.Unmarshal(b, &out) }); err != nil {
		return nil, err
	}
	return &out, nil
}
