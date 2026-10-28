package hub

import (
	"context"
	"crypto/x509"
	"errors"
	"net/http"
	"strings"

	"connectrpc.com/connect"

	"github.com/richard-sen27/mycelium/internal/identity"
	"github.com/richard-sen27/mycelium/internal/policy"
)

// Audit outcomes.
const (
	outcomeOK     = "ok"
	outcomeDenied = "denied"
	outcomeError  = "error"
)

type ctxKey int

const (
	ctxUser ctxKey = iota
	ctxNode
)

// nodePeer is a node authenticated by a verified TLS client certificate.
type nodePeer struct {
	Name string
	Cert *x509.Certificate
}

// authMiddleware resolves the caller: a verified client certificate (node)
// and/or an API key (user); without an API key, a node certificate with the
// read scope header (the node owner, read-only) or a session cookie that
// passes the CSRF rules (uiauth.go). It never rejects, except read-scoped
// requests to routes outside the scope; handlers decide what they need.
func (s *server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		var np *nodePeer
		if r.TLS != nil && len(r.TLS.VerifiedChains) > 0 && len(r.TLS.VerifiedChains[0]) > 0 {
			leaf := r.TLS.VerifiedChains[0][0]
			if name, err := identity.NodeNameFromCert(s.td, leaf); err == nil {
				np = &nodePeer{Name: name, Cert: leaf}
				ctx = context.WithValue(ctx, ctxNode, np)
			}
		}
		if auth := r.Header.Get("Authorization"); auth != "" {
			if secret, ok := strings.CutPrefix(auth, "Bearer "); ok && strings.HasPrefix(secret, apiKeyPrefix) {
				u, _, err := s.st.lookupAPIKey(ctx, secret)
				if err == nil {
					ctx = context.WithValue(ctx, ctxUser, u)
				} else if !errors.Is(err, errNotFound) {
					s.log.Error("api key lookup", "err", err)
				}
			}
		}
		if userFrom(ctx) == nil {
			switch {
			case np != nil && r.Header.Get(scopeHeader) == scopeNetworkRead:
				if u := s.readScopeUser(ctx, np); u != nil {
					ctx = context.WithValue(context.WithValue(ctx, ctxUser, u), ctxReadScope, np.Name)
					if !readScopeHTTPAllowed(r) {
						s.auditDenied(ctx, "node:"+np.Name, "scope.network_read", r.Method+" "+r.URL.Path,
							map[string]any{"reason": "node read scope"})
						writeAPIError(w, http.StatusForbidden, "node read scope")
						return
					}
				}
			case np == nil:
				if id, u, ok := s.cookieUser(r); ok {
					ctx = context.WithValue(context.WithValue(ctx, ctxUser, u), ctxSession, id)
				}
			}
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func userFrom(ctx context.Context) *userRow {
	u, _ := ctx.Value(ctxUser).(*userRow)
	return u
}

func nodeFrom(ctx context.Context) *nodePeer {
	n, _ := ctx.Value(ctxNode).(*nodePeer)
	return n
}

func principalOf(u *userRow) policy.Principal {
	return policy.Principal{Kind: policy.KindUser, ID: u.ID, Role: policy.Role(u.Role), OwnerID: u.ID}
}

// requireUser returns the calling user or an Unauthenticated error.
func requireUser(ctx context.Context) (*userRow, error) {
	u := userFrom(ctx)
	if u == nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("missing or invalid API key"))
	}
	return u, nil
}

// authorize asks the policy. A deny is audited when audit is true (mutating
// actions) and returned as PermissionDenied with the decision's reason.
func (s *server) authorize(ctx context.Context, u *userRow, a policy.Action, r policy.Resource, audit bool) error {
	d := s.policy.Authorize(ctx, principalOf(u), a, r)
	if d.Allow {
		return nil
	}
	reason := d.Reason
	if reason == "" {
		reason = "permission denied"
	}
	if audit {
		s.audit(ctx, u.ID, string(a), resourceName(r), outcomeDenied, map[string]any{"reason": reason})
	}
	return connect.NewError(connect.CodePermissionDenied, errors.New(reason))
}

func resourceName(r policy.Resource) string {
	switch {
	case r.Kind != "" && r.ID != "":
		return r.Kind + ":" + r.ID
	case r.Kind != "":
		return r.Kind
	default:
		return r.ID
	}
}

// audit writes an audit row and a log line. details must never hold secrets.
func (s *server) audit(ctx context.Context, actor, action, resource, outcome string, details map[string]any) {
	// Audit even if the request context is already cancelled.
	ctx = context.WithoutCancel(ctx)
	if err := s.st.appendAudit(ctx, actor, action, resource, outcome, details); err != nil {
		s.log.Error("audit write failed", "err", err, "action", action)
	}
	s.publishAudit(actor, action, resource, outcome, details)
	attrs := []any{"actor", actor, "action", action, "resource", resource, "outcome", outcome}
	for k, v := range details {
		attrs = append(attrs, k, v)
	}
	s.log.Info("audit", attrs...)
}

// internalError logs err and returns a generic Internal error.
func (s *server) internalError(op string, err error) error {
	s.log.Error(op, "err", err)
	return connect.NewError(connect.CodeInternal, errors.New("internal error"))
}
