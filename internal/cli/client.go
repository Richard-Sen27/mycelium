package cli

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	"github.com/richard-sen27/mycelium/gen/mycel/v1/mycelv1connect"
)

// hubClient bundles the Connect clients for one hub.
type hubClient struct {
	url    string
	tokens mycelv1connect.TokenServiceClient
	nodes  mycelv1connect.NodeServiceClient
	users  mycelv1connect.UserServiceClient
}

const loginHint = "run `mycel login <hub-url> --api-key <key>`, or `mycel hub init` on the hub machine"

// hub returns clients for the configured hub, failing with a helpful message
// when hub_url, api_key or ca_cert_pem are missing.
func (a *app) hub(cmd *cobra.Command) (*hubClient, error) {
	dirs, f, err := a.loadConfig(cmd)
	if err != nil {
		return nil, err
	}
	var missing []string
	if f.HubURL == "" {
		missing = append(missing, "hub_url")
	}
	if f.APIKey == "" {
		missing = append(missing, "api_key")
	}
	if f.CACertPEM == "" {
		missing = append(missing, "ca_cert_pem")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("not connected to a hub (%s missing in %s): %s",
			strings.Join(missing, ", "), dirs.ConfigFile(), loginHint)
	}
	return newHubClient(f.HubURL, f.APIKey, []byte(f.CACertPEM))
}

func newHubClient(hubURL, apiKey string, caPEM []byte) (*hubClient, error) {
	base, err := normalizeHubURL(hubURL)
	if err != nil {
		return nil, err
	}
	hc, err := pinnedHTTPClient(caPEM)
	if err != nil {
		return nil, err
	}
	opts := connect.WithInterceptors(bearerInterceptor(apiKey))
	return &hubClient{
		url:    base,
		tokens: mycelv1connect.NewTokenServiceClient(hc, base, opts),
		nodes:  mycelv1connect.NewNodeServiceClient(hc, base, opts),
		users:  mycelv1connect.NewUserServiceClient(hc, base, opts),
	}, nil
}

// normalizeHubURL checks that u is an https URL and strips a trailing slash.
func normalizeHubURL(u string) (string, error) {
	p, err := url.Parse(u)
	if err != nil || p.Host == "" {
		return "", fmt.Errorf("invalid hub URL %q: want https://host[:port]", u)
	}
	if p.Scheme != "https" {
		return "", fmt.Errorf("invalid hub URL %q: the hub API is only reachable over https", u)
	}
	return strings.TrimRight(u, "/"), nil
}

// pinnedHTTPClient trusts only the given CA, never the system roots.
func pinnedHTTPClient(caPEM []byte) (*http.Client, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("config has no valid hub CA certificate (ca_cert_pem): " + loginHint)
	}
	tr := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		TLSClientConfig:     &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	return &http.Client{Transport: tr, Timeout: 60 * time.Second}, nil
}

// bearerInterceptor sets the API key on every request. The key is never logged.
func bearerInterceptor(apiKey string) connect.Interceptor {
	return connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			req.Header().Set("Authorization", "Bearer "+apiKey)
			return next(ctx, req)
		}
	})
}

// rpcError turns a Connect error into a message a person can act on.
func rpcError(hubURL string, err error) error {
	var ce *connect.Error
	if !errors.As(err, &ce) {
		return err
	}
	msg := ce.Message()
	switch ce.Code() {
	case connect.CodeUnauthenticated:
		return fmt.Errorf("authentication failed: %s (check your API key, or %s)", msg, loginHint)
	case connect.CodePermissionDenied:
		return fmt.Errorf("permission denied: %s", msg)
	case connect.CodeNotFound:
		return fmt.Errorf("not found: %s", msg)
	case connect.CodeAlreadyExists:
		return fmt.Errorf("already exists: %s", msg)
	case connect.CodeInvalidArgument, connect.CodeFailedPrecondition:
		return fmt.Errorf("rejected by hub: %s", msg)
	case connect.CodeUnavailable:
		return fmt.Errorf("hub unreachable at %s: %s", hubURL, msg)
	case connect.CodeDeadlineExceeded:
		return fmt.Errorf("hub at %s did not answer in time", hubURL)
	default:
		return fmt.Errorf("hub error (%s): %s", ce.Code(), msg)
	}
}
