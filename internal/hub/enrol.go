package hub

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"errors"
	"time"

	"connectrpc.com/connect"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/internal/identity"
)

// invalidToken is the single answer to every failed enrolment, so a caller
// cannot learn whether a token exists, was used, revoked or expired, or
// whether only the CSR was wrong.
func invalidToken() error {
	return connect.NewError(connect.CodePermissionDenied, errors.New("invalid or expired token"))
}

// maxCSRBytes bounds the CSR accepted by Enrol and Renew.
const maxCSRBytes = 4096

// checkCSR parses and verifies a CSR: size, self-signature, ECDSA P-256/P-384 key.
func checkCSR(der []byte) error {
	if len(der) == 0 || len(der) > maxCSRBytes {
		return errors.New("CSR size out of range")
	}
	csr, err := x509.ParseCertificateRequest(der)
	if err != nil {
		return err
	}
	if err := csr.CheckSignature(); err != nil {
		return err
	}
	k, ok := csr.PublicKey.(*ecdsa.PublicKey)
	if !ok || (k.Curve != elliptic.P256() && k.Curve != elliptic.P384()) {
		return errors.New("CSR key must be ECDSA P-256 or P-384")
	}
	return nil
}

type enrolService struct{ s *server }

// Enrol exchanges a join token and a CSR for a node certificate. No API key.
//
// Requests are rate limited per remote IP. Failures that do not match a real
// token row (malformed or unknown token, bad CSR) are only debug-logged, so
// anonymous callers cannot grow the audit log; failures against a real token
// and successes are audited.
func (e *enrolService) Enrol(ctx context.Context, req *connect.Request[mycelv1.EnrolRequest]) (*connect.Response[mycelv1.EnrolResponse], error) {
	s := e.s
	peer := remoteIP(req.Peer().Addr)
	if !s.enrolLimit.allow(peer) {
		s.log.Debug("enrol rate limited", "peer", peer)
		return nil, connect.NewError(connect.CodeResourceExhausted, errors.New("too many enrolment attempts; try again later"))
	}
	quiet := func(reason string) error {
		s.log.Debug("enrol refused", "reason", reason, "peer", peer)
		return invalidToken()
	}
	fail := func(reason, resource string) error {
		s.audit(ctx, "anonymous", "enrol", resource, outcomeDenied, map[string]any{"reason": reason, "peer": peer})
		return invalidToken()
	}
	if err := checkCSR(req.Msg.GetCsrDer()); err != nil {
		return nil, quiet("bad CSR")
	}
	jt, err := identity.ParseJoinToken(req.Msg.GetToken())
	if err != nil {
		return nil, quiet("malformed token")
	}
	row, err := s.st.joinTokenBySecretHash(ctx, jt.SecretHash())
	if errors.Is(err, errNotFound) {
		return nil, quiet("unknown token")
	}
	if err != nil {
		return nil, s.internalError("token lookup", err)
	}
	res := "token:" + row.ID
	if row.ID != jt.ID {
		return nil, fail("token id mismatch", res)
	}
	if row.UsedAt != "" {
		return nil, fail("token already used", res)
	}
	if row.RevokedAt != "" {
		return nil, fail("token revoked", res)
	}
	exp, err := parseTime(row.ExpiresAt)
	if err != nil || !time.Now().Before(exp) {
		return nil, fail("token expired", res)
	}
	exists, err := s.st.nodeExists(ctx, row.NodeName)
	if err != nil {
		return nil, s.internalError("node lookup", err)
	}
	if exists {
		return nil, fail("node name taken", res)
	}
	certPEM, serial, err := s.ca.SignNodeCSR(req.Msg.GetCsrDer(), row.NodeName, identity.NodeCertLifetime)
	if err != nil {
		return nil, fail("CSR rejected by CA", res)
	}
	notAfter, err := certNotAfter(certPEM)
	if err != nil {
		return nil, s.internalError("parse issued cert", err)
	}
	spiffeID := identity.NodeSPIFFEID(s.td, row.NodeName)
	node := &nodeRow{
		Name: row.NodeName, SPIFFEID: spiffeID, OwnerID: row.OwnerID,
		CapsCeiling: row.Caps, CapsEnabled: row.Caps,
		CertSerial: serial, CertNotAfter: formatTime(notAfter), CreatedAt: nowString(),
	}
	if err := s.st.enrolNode(ctx, row.ID, node); err != nil {
		if errors.Is(err, errTokenSpent) || errors.Is(err, errConflict) {
			return nil, fail("lost race: token spent or name taken", res)
		}
		return nil, s.internalError("enrol node", err)
	}
	s.audit(ctx, "node:"+row.NodeName, "enrol", "node:"+row.NodeName, outcomeOK,
		map[string]any{"token_id": row.ID, "owner_id": row.OwnerID, "caps": row.Caps, "serial": serial})
	s.alert(ctx, row.NodeName, "node %s enrolled (owner %s)", row.NodeName, row.OwnerID)
	return connect.NewResponse(&mycelv1.EnrolResponse{
		NodeId: "node:" + row.NodeName, SpiffeId: spiffeID, CertPem: certPEM, CaPem: s.caPEM,
		BusUrl: s.cfg.BusAdvertise, HubUrl: s.cfg.AdvertiseURL,
		Caps: &mycelv1.Capabilities{Names: row.Caps}, OwnerId: row.OwnerID,
	}), nil
}

// Renew issues a fresh certificate to a node that presents its current one as
// the TLS client certificate. The old certificate is NOT revoked: it stays
// valid until it expires, so a node whose renewal response is lost keeps
// working. Removing the node revokes all of its certificates.
func (e *enrolService) Renew(ctx context.Context, req *connect.Request[mycelv1.RenewRequest]) (*connect.Response[mycelv1.RenewResponse], error) {
	s := e.s
	peer := nodeFrom(ctx)
	if peer == nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("a node client certificate is required"))
	}
	actor := "node:" + peer.Name
	serial := identity.SerialHex(peer.Cert)
	spiffeID := identity.NodeSPIFFEID(s.td, peer.Name)
	if s.st.IsRevoked(serial, spiffeID) {
		s.audit(ctx, actor, "renew", actor, outcomeDenied, map[string]any{"reason": "revoked", "serial": serial})
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("certificate revoked"))
	}
	if _, err := s.st.getNode(ctx, peer.Name); errors.Is(err, errNotFound) {
		s.audit(ctx, actor, "renew", actor, outcomeDenied, map[string]any{"reason": "node removed", "serial": serial})
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("node removed; certificate revoked"))
	} else if err != nil {
		return nil, s.internalError("get node", err)
	}
	certPEM, newSerial, err := s.ca.SignNodeCSR(req.Msg.GetCsrDer(), peer.Name, identity.NodeCertLifetime)
	if err != nil {
		s.audit(ctx, actor, "renew", actor, outcomeError, map[string]any{"reason": "bad CSR"})
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid CSR"))
	}
	notAfter, err := certNotAfter(certPEM)
	if err != nil {
		return nil, s.internalError("parse issued cert", err)
	}
	if err := s.st.renewNodeCert(ctx, peer.Name, newSerial, formatTime(notAfter)); err != nil {
		if errors.Is(err, errNotFound) {
			return nil, connect.NewError(connect.CodePermissionDenied, errors.New("node removed; certificate revoked"))
		}
		return nil, s.internalError("renew", err)
	}
	s.audit(ctx, actor, "renew", actor, outcomeOK, map[string]any{"old_serial": serial, "serial": newSerial})
	return connect.NewResponse(&mycelv1.RenewResponse{CertPem: certPEM, CaPem: s.caPEM}), nil
}
