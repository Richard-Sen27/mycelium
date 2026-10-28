package identity

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
)

const (
	joinIDLen       = 16
	joinSecretLen   = 32
	fingerprintLen  = 64 // sha256, hex
	agentKeyLen     = 32
	maxJoinTokenLen = 2048
	maxAgentToken   = 4096
	agentClockSkew  = 2 * time.Minute
)

var b64 = base64.RawURLEncoding.Strict()

func randomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return nil, fmt.Errorf("identity: random: %w", err)
	}
	return b, nil
}

func isLowerHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func constantTimeEqualHex(a, b string) bool {
	if len(a) != len(b) || len(a) == 0 {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// ---- join tokens ----

func validHubURL(s string) error {
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("hub_url: %v", err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("hub_url: scheme must be https")
	}
	if u.Host == "" || u.Hostname() == "" {
		return fmt.Errorf("hub_url: empty host")
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.ContainsAny(s, "?#@") {
		return fmt.Errorf("hub_url: userinfo, query and fragment are not allowed")
	}
	return nil
}

// isBase64URL reports whether s consists only of [A-Za-z0-9_-]. Go's base64
// decoder silently skips \r and \n even in strict mode, so this check runs
// before every decode to keep tokens non-malleable.
func isBase64URL(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '_' {
			return false
		}
	}
	return true
}

func (t *JoinToken) validate() error {
	if err := validHubURL(t.HubURL); err != nil {
		return err
	}
	if !isLowerHex(t.ID, 2*joinIDLen) {
		return fmt.Errorf("token_id must be %d lowercase hex chars", 2*joinIDLen)
	}
	if len(t.Secret) != joinSecretLen {
		return fmt.Errorf("secret must be %d bytes", joinSecretLen)
	}
	if !isLowerHex(t.CAFingerprint, fingerprintLen) {
		return fmt.Errorf("ca_fingerprint must be %d lowercase hex chars", fingerprintLen)
	}
	return nil
}

func newJoinToken(hubURL, caFingerprint string) (*JoinToken, error) {
	id, err := randomBytes(joinIDLen)
	if err != nil {
		return nil, err
	}
	secret, err := randomBytes(joinSecretLen)
	if err != nil {
		return nil, err
	}
	t := &JoinToken{
		HubURL:        hubURL,
		ID:            hex.EncodeToString(id),
		Secret:        secret,
		CAFingerprint: caFingerprint,
	}
	if err := t.validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	return t, nil
}

func (t *JoinToken) encode() string {
	p := &mycelv1.JoinTokenPayload{
		HubUrl:        t.HubURL,
		TokenId:       t.ID,
		Secret:        t.Secret,
		CaFingerprint: t.CAFingerprint,
	}
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(p)
	if err != nil {
		// Unreachable for a message of plain scalar fields.
		return ""
	}
	return JoinTokenPrefix + b64.EncodeToString(raw)
}

func parseJoinToken(s string) (*JoinToken, error) {
	if len(s) > maxJoinTokenLen {
		return nil, fmt.Errorf("%w: too long", ErrInvalidToken)
	}
	body, ok := strings.CutPrefix(s, JoinTokenPrefix)
	if !ok {
		return nil, fmt.Errorf("%w: missing %q prefix", ErrInvalidToken, JoinTokenPrefix)
	}
	if !isBase64URL(body) {
		return nil, fmt.Errorf("%w: bad encoding", ErrInvalidToken)
	}
	raw, err := b64.DecodeString(body)
	if err != nil {
		return nil, fmt.Errorf("%w: bad encoding", ErrInvalidToken)
	}
	var p mycelv1.JoinTokenPayload
	if err := proto.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("%w: bad payload", ErrInvalidToken)
	}
	if len(p.ProtoReflect().GetUnknown()) != 0 {
		return nil, fmt.Errorf("%w: unknown fields", ErrInvalidToken)
	}
	t := &JoinToken{
		HubURL:        p.GetHubUrl(),
		ID:            p.GetTokenId(),
		Secret:        bytes.Clone(p.GetSecret()),
		CAFingerprint: p.GetCaFingerprint(),
	}
	if err := t.validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	return t, nil
}

// ---- agent tokens ----

// agentWire is the JSON form of AgentClaims inside a token. Times are Unix
// seconds.
type agentWire struct {
	AgentID   string `json:"aid"`
	NodeName  string `json:"node"`
	SandboxID string `json:"sbx,omitempty"`
	OwnerID   string `json:"own,omitempty"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
}

func newAgentTokenSigner(key []byte) (*AgentTokenSigner, error) {
	if len(key) < agentKeyLen {
		return nil, fmt.Errorf("%w: agent token key must be at least %d bytes", ErrInvalidKey, agentKeyLen)
	}
	return &AgentTokenSigner{key: bytes.Clone(key)}, nil
}

// mac authenticates the prefix together with the base64 payload.
func (s *AgentTokenSigner) mac(payload string) []byte {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(AgentTokenPrefix + payload))
	return m.Sum(nil)
}

// validAgentID reports whether id is "agent:<name>@<node>" with name passing
// ValidNodeName and node equal to nodeName.
func validAgentID(id, nodeName string) bool {
	rest, ok := strings.CutPrefix(id, "agent:")
	if !ok {
		return false
	}
	name, node, ok := strings.Cut(rest, "@")
	return ok && ValidNodeName(name) && node == nodeName
}

func (s *AgentTokenSigner) issueAt(claims AgentClaims, ttl time.Duration, now time.Time) (string, error) {
	if ttl < time.Second || ttl > AgentTokenLifetime {
		return "", fmt.Errorf("%w: ttl must be in [1s, %v]", ErrInvalidArgument, AgentTokenLifetime)
	}
	if !ValidNodeName(claims.NodeName) {
		return "", ErrInvalidName
	}
	if !validAgentID(claims.AgentID, claims.NodeName) {
		return "", fmt.Errorf("%w: agent id must be agent:<name>@%s", ErrInvalidArgument, claims.NodeName)
	}
	iat := now.UTC().Truncate(time.Second)
	exp := iat.Add(ttl)
	w := agentWire{
		AgentID:   claims.AgentID,
		NodeName:  claims.NodeName,
		SandboxID: claims.SandboxID,
		OwnerID:   claims.OwnerID,
		IssuedAt:  iat.Unix(),
		ExpiresAt: exp.Unix(),
	}
	raw, err := json.Marshal(w)
	if err != nil {
		return "", fmt.Errorf("identity: encode claims: %w", err)
	}
	payload := b64.EncodeToString(raw)
	return AgentTokenPrefix + payload + "." + b64.EncodeToString(s.mac(payload)), nil
}

func (s *AgentTokenSigner) verifyAt(token string, now time.Time) (*AgentClaims, error) {
	if len(token) > maxAgentToken {
		return nil, fmt.Errorf("%w: too long", ErrInvalidToken)
	}
	body, ok := strings.CutPrefix(token, AgentTokenPrefix)
	if !ok {
		return nil, fmt.Errorf("%w: missing %q prefix", ErrInvalidToken, AgentTokenPrefix)
	}
	if strings.Count(body, ".") != 1 {
		return nil, fmt.Errorf("%w: malformed", ErrInvalidToken)
	}
	payload, sigStr, _ := strings.Cut(body, ".")
	if payload == "" || sigStr == "" || !isBase64URL(payload) || !isBase64URL(sigStr) {
		return nil, fmt.Errorf("%w: malformed", ErrInvalidToken)
	}
	sig, err := b64.DecodeString(sigStr)
	if err != nil || len(sig) != sha256.Size {
		return nil, fmt.Errorf("%w: malformed signature", ErrInvalidToken)
	}
	if !hmac.Equal(sig, s.mac(payload)) {
		return nil, ErrBadSignature
	}
	raw, err := b64.DecodeString(payload)
	if err != nil {
		return nil, fmt.Errorf("%w: bad payload encoding", ErrInvalidToken)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var w agentWire
	if err := dec.Decode(&w); err != nil {
		return nil, fmt.Errorf("%w: bad claims", ErrInvalidToken)
	}
	if dec.More() {
		return nil, fmt.Errorf("%w: trailing data", ErrInvalidToken)
	}
	if !ValidNodeName(w.NodeName) || !validAgentID(w.AgentID, w.NodeName) {
		return nil, fmt.Errorf("%w: bad claims", ErrInvalidToken)
	}
	if window := w.ExpiresAt - w.IssuedAt; window <= 0 || window > int64(AgentTokenLifetime/time.Second) {
		return nil, fmt.Errorf("%w: validity window out of range", ErrInvalidToken)
	}
	iat := time.Unix(w.IssuedAt, 0).UTC()
	if iat.After(now.Add(agentClockSkew)) {
		return nil, fmt.Errorf("%w: issued in the future", ErrInvalidToken)
	}
	exp := time.Unix(w.ExpiresAt, 0).UTC()
	if !now.Before(exp) {
		return nil, ErrExpired
	}
	return &AgentClaims{
		AgentID:   w.AgentID,
		NodeName:  w.NodeName,
		SandboxID: w.SandboxID,
		OwnerID:   w.OwnerID,
		IssuedAt:  iat,
		ExpiresAt: exp,
	}, nil
}
