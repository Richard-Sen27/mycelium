package cli

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/internal/config"
)

func (a *app) loginCmd() *cobra.Command {
	var fingerprint string
	cmd := &cobra.Command{
		Use:   "login <hub-url> --api-key <key> [--ca-fingerprint <hex>]",
		Short: "Pin a hub's CA and save credentials for it",
		Args:  exactArgs(1, "hub-url"),
		RunE: func(cmd *cobra.Command, args []string) error {
			hubURL, err := normalizeHubURL(args[0])
			if err != nil {
				return usageError{err}
			}
			apiKey, ok := a.overrideFlag(cmd, "api-key", EnvAPIKey, a.apiKey)
			if !ok || apiKey == "" {
				return usagef("--api-key is required")
			}

			caPEM, fp, err := fetchCA(cmd.Context(), hubURL)
			if err != nil {
				return err
			}
			if fingerprint != "" {
				if normalizeFingerprint(fingerprint) != fp {
					return fmt.Errorf("CA fingerprint mismatch: hub %s presents %s, expected %s; not trusting it", hubURL, fp, normalizeFingerprint(fingerprint))
				}
			} else if err := a.confirmFingerprint(hubURL, fp); err != nil {
				return err
			}

			hc, err := newHubClient(hubURL, apiKey, caPEM)
			if err != nil {
				return err
			}
			resp, err := hc.users.Whoami(cmd.Context(), connect.NewRequest(&mycelv1.WhoamiRequest{}))
			if err != nil {
				return rpcError(hubURL, err)
			}

			dirs, err := config.Resolve()
			if err != nil {
				return err
			}
			f, err := config.Load(dirs)
			if err != nil {
				return err
			}
			f.HubURL = hubURL
			f.APIKey = apiKey
			f.CACertPEM = string(caPEM)
			if err := config.Save(dirs, f); err != nil {
				return err
			}
			if a.jsonOut {
				return a.printProto(resp.Msg)
			}
			u := resp.Msg.GetUser()
			_, err = fmt.Fprintf(a.stdout, "Logged in to %s as %s (%s). Saved to %s.\n", hubURL, u.GetName(), roleString(u.GetRole()), dirs.ConfigFile())
			return err
		},
	}
	cmd.Flags().StringVar(&fingerprint, "ca-fingerprint", "", "expected SHA-256 fingerprint of the hub CA certificate (hex)")
	return cmd
}

// confirmFingerprint asks on the terminal whether to trust the CA. Without a
// terminal, or with --json, it refuses.
func (a *app) confirmFingerprint(hubURL, fp string) error {
	if a.jsonOut || !a.isTTY() {
		return fmt.Errorf("hub CA fingerprint is %s; pass --ca-fingerprint %s to trust it non-interactively", fp, fp)
	}
	_, _ = fmt.Fprintf(a.stderr, "Hub %s presents a CA with SHA-256 fingerprint\n  %s\nCompare it with the one `mycel hub init` printed.\nTrust this hub? [y/N] ", hubURL, fp)
	line, err := bufio.NewReader(a.stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return nil
	default:
		return errors.New("hub not trusted; nothing saved")
	}
}

// fetchCA downloads <hub>/ca.pem and returns it with its SHA-256 fingerprint
// (lowercase hex of the certificate DER).
func fetchCA(ctx context.Context, hubURL string) ([]byte, string, error) {
	// This single request cannot verify TLS: fetching the CA is how we learn
	// it. Trust comes from the fingerprint check that follows, never from
	// this connection.
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}, //nolint:gosec // CA bootstrap; verified by fingerprint below
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, hubURL+"/ca.pem", nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("fetch hub CA: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("fetch hub CA: %s/ca.pem returned %s", hubURL, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil, "", fmt.Errorf("fetch hub CA: %w", err)
	}
	block, _ := pem.Decode(body)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, "", errors.New("fetch hub CA: response is not a PEM certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, "", fmt.Errorf("fetch hub CA: %w", err)
	}
	sum := sha256.Sum256(cert.Raw)
	// Pin exactly the certificate that was fingerprinted.
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	return caPEM, hex.EncodeToString(sum[:]), nil
}

// normalizeFingerprint accepts hex with or without colons, any case.
func normalizeFingerprint(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, ":", "")
	return strings.TrimPrefix(s, "sha256")
}
