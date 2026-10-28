package daemon

import (
	"context"
	"crypto"
	"crypto/tls"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"connectrpc.com/connect"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/gen/mycel/v1/mycelv1connect"
	"github.com/richard-sen27/mycelium/internal/identity"
)

// errRenewRevoked reports that the hub refused renewal because the node is revoked.
var errRenewRevoked = errors.New("renewal refused: node revoked")

func (n *node) renewLoop(ctx context.Context) {
	for {
		delay := n.cfg.renewCheckEvery
		n.checkExpired()
		if n.renewDue() {
			if err := n.renew(ctx); err != nil {
				if errors.Is(err, errRenewRevoked) {
					n.triggerRevoked()
					return
				}
				if ctx.Err() != nil {
					return
				}
				n.log.Warn("certificate renewal failed; will retry", "err", err, "in", n.cfg.renewRetryEvery)
				delay = n.cfg.renewRetryEvery
			}
		}
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// checkExpired flags an expired certificate once: the hub refuses it on the
// bus and for renewal, so only a new join can recover. Renewal keeps being
// retried and the identity is kept.
func (n *node) checkExpired() {
	n.mu.Lock()
	expired := time.Now().After(n.leaf.NotAfter)
	changed := expired != n.expired
	n.expired = expired
	if changed && expired && n.cfg.onState != nil {
		n.cfg.onState(StateExpired)
	}
	notAfter := n.leaf.NotAfter
	n.mu.Unlock()
	if changed && expired {
		n.log.Error("node certificate expired; the hub will refuse it. Run `mycel join` with a new token to enrol this machine again",
			"not_after", notAfter.UTC().Format(time.RFC3339))
	}
}

// renewDue reports whether less than a third of the certificate's lifetime is left.
func (n *node) renewDue() bool {
	n.mu.Lock()
	leaf := n.leaf
	n.mu.Unlock()
	lifetime := leaf.NotAfter.Sub(leaf.NotBefore)
	return time.Until(leaf.NotAfter) < lifetime/3
}

// renew asks the hub for a new certificate for the current key, stores it and
// reconnects the bus with it.
func (n *node) renew(ctx context.Context) error {
	n.mu.Lock()
	cur := n.cert
	name, hubURL := n.st.Name, n.st.HubURL
	n.mu.Unlock()

	key, ok := cur.PrivateKey.(crypto.Signer)
	if !ok {
		return errors.New("node key is not a signer")
	}
	csr, err := identity.NewCSR(key, name)
	if err != nil {
		return err
	}
	hc := pinnedClient(n.pool, func() *tls.Certificate { return &cur })
	defer hc.CloseIdleConnections()
	client := mycelv1connect.NewEnrolServiceClient(hc, hubURL)
	resp, err := client.Renew(ctx, connect.NewRequest(&mycelv1.RenewRequest{CsrDer: csr}))
	if err != nil {
		var ce *connect.Error
		if errors.As(err, &ce) && ce.Code() == connect.CodePermissionDenied &&
			strings.Contains(strings.ToLower(ce.Message()), "revoked") {
			return errRenewRevoked
		}
		return fmt.Errorf("renew: %w", err)
	}
	certPEM := resp.Msg.GetCertPem()
	if _, certName, err := identity.VerifyNodeCert(n.pool, n.td, certPEM); err != nil {
		return fmt.Errorf("renewed certificate: %w", err)
	} else if certName != name {
		return errors.New("renewed certificate: wrong node name")
	}
	if _, _, err := loadKeyPair(certPEM, n.keyPEM); err != nil {
		return fmt.Errorf("renewed certificate: %w", err)
	}

	path := filepath.Join(n.dir, CertFile)
	if err := writeFileAtomic(path, certPEM); err != nil {
		return fmt.Errorf("store renewed certificate: %w", err)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reload certificate: %w", err)
	}
	cert, leaf, err := loadKeyPair(onDisk, n.keyPEM)
	if err != nil {
		return fmt.Errorf("reload certificate: %w", err)
	}
	n.mu.Lock()
	n.cert, n.leaf = cert, leaf
	n.expired = false
	n.mu.Unlock()
	n.log.Info("certificate renewed", "serial", identity.SerialHex(leaf), "not_after", leaf.NotAfter.UTC().Format(time.RFC3339))

	// Reconnect the bus with the new certificate. The new connection is set
	// up before the old one is closed, so the ctl subscription stays served.
	nc, gen, err := n.connectBus(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("reconnect with renewed certificate: %w", err)
	}
	return n.install(nc, gen)
}
