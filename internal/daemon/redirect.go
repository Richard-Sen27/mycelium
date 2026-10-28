package daemon

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/internal/identity"
)

// Signed hub redirects (internal/hub/moving.go): helpers shared by the
// hub, the node and the CLI.

// RedirectHeader carries base64(Redirect proto) on a "hub moved" answer.
const RedirectHeader = "Mycel-Redirect"

// maxRedirectHeader bounds the encoded header.
const maxRedirectHeader = 4096

// EncodeRedirect returns the RedirectHeader value for r.
func EncodeRedirect(r *mycelv1.Redirect) (string, error) {
	b, err := proto.Marshal(r)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// DecodeRedirect parses a RedirectHeader value.
func DecodeRedirect(v string) (*mycelv1.Redirect, error) {
	if v == "" || len(v) > maxRedirectHeader {
		return nil, errors.New("redirect: missing or oversized header")
	}
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(v))
	if err != nil {
		return nil, errors.New("redirect: header is not base64")
	}
	var r mycelv1.Redirect
	if err := proto.Unmarshal(b, &r); err != nil {
		return nil, errors.New("redirect: malformed")
	}
	return &r, nil
}

// RedirectFromError returns the signed redirect carried by a Connect
// "hub moved" error (FailedPrecondition with the RedirectHeader), or nil.
// The redirect is not verified.
func RedirectFromError(err error) *mycelv1.Redirect {
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeFailedPrecondition {
		return nil
	}
	v := ce.Meta().Get(RedirectHeader)
	if v == "" {
		return nil
	}
	r, derr := DecodeRedirect(v)
	if derr != nil {
		return nil
	}
	return r
}

// VerifyRedirect checks r against the pinned hub CA: the trust domain must
// be the CA's, the hub URL https with a host, the bus URL present, the
// signature the CA's (identity.VerifyRedirect) and issued_at within 24 h
// in the past and 5 min in the future. Monotonicity is the caller's check.
func VerifyRedirect(caPEM []byte, r *mycelv1.Redirect, now time.Time) error {
	if r == nil {
		return errors.New("redirect: empty")
	}
	ca, err := identity.ParseCertPEM(caPEM)
	if err != nil {
		return fmt.Errorf("redirect: pinned CA: %w", err)
	}
	if len(ca.URIs) != 1 || ca.URIs[0].Scheme != "spiffe" || ca.URIs[0].Host != r.GetTrustDomain() {
		return errors.New("redirect: trust domain differs from the pinned CA")
	}
	if err := checkHubURL(r.GetHubUrl()); err != nil {
		return fmt.Errorf("redirect: %w", err)
	}
	if err := checkBusURL(r.GetBusUrl()); err != nil {
		return err
	}
	if err := identity.VerifyRedirect(ca, identity.TrustDomain(r.GetTrustDomain()), r.GetHubUrl(), r.GetBusUrl(),
		r.GetIssuedAt(), r.GetSignature(), now.Unix()); err != nil {
		return fmt.Errorf("redirect: %w", err)
	}
	return nil
}

// checkBusURL accepts host:port or tls://host:port with a numeric port and
// no control characters.
func checkBusURL(s string) error {
	_, err := NormalizeBusURL(s)
	return err
}

// NormalizeBusURL returns tls://host:port for a bus address given as
// host:port or tls://host:port (lower-case host), so two spellings of the
// same address compare equal.
func NormalizeBusURL(s string) (string, error) {
	if s == "" {
		return "", errors.New("redirect: no bus URL")
	}
	if u, err := url.Parse(s); err == nil && u.Scheme != "" && u.Host != "" && u.Scheme != "tls" {
		return "", errors.New("redirect: bus URL must be tls://host:port")
	}
	if err := identity.CheckRedirectFields("", "", s); err != nil {
		return "", fmt.Errorf("redirect: %w", err)
	}
	h, p, _ := net.SplitHostPort(strings.TrimPrefix(s, "tls://"))
	return "tls://" + net.JoinHostPort(strings.ToLower(h), p), nil
}

// sameHub reports whether two hub URLs and bus addresses name the same hub.
func sameHub(hubA, busA, hubB, busB string) bool {
	if !strings.EqualFold(strings.TrimRight(hubA, "/"), strings.TrimRight(hubB, "/")) {
		return false
	}
	a, errA := NormalizeBusURL(busA)
	b, errB := NormalizeBusURL(busB)
	return errA == nil && errB == nil && a == b
}
