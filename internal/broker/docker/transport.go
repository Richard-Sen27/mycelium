package docker

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// defaultEngine dials the host Engine: DOCKER_HOST when it is unix:// or
// tcp:// (plain, loopback expected), else the first standard socket that
// exists at dial time.
func defaultEngine() http.RoundTripper {
	network, addrs := engineAddrs(os.Getenv("DOCKER_HOST"))
	d := &net.Dialer{Timeout: 5 * time.Second}
	return &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			if len(addrs) == 0 {
				return nil, errors.New("no docker engine address: set DOCKER_HOST to unix:// or tcp://")
			}
			var last error
			for _, a := range addrs {
				if network == "unix" {
					if _, err := os.Stat(a); err != nil {
						last = err
						continue
					}
				}
				c, err := d.DialContext(ctx, network, a)
				if err == nil {
					return c, nil
				}
				last = err
			}
			return nil, last
		},
		MaxIdleConns:          16,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 0, // logs, attach and wait stream for as long as they run
		DisableCompression:    true,
	}
}

// engineAddrs resolves where the Engine listens.
func engineAddrs(dockerHost string) (network string, addrs []string) {
	if dockerHost != "" {
		if u, err := url.Parse(dockerHost); err == nil {
			switch u.Scheme {
			case "unix":
				return "unix", []string{u.Path}
			case "tcp":
				return "tcp", []string{u.Host}
			}
		}
	}
	addrs = []string{"/var/run/docker.sock"}
	if home, err := os.UserHomeDir(); err == nil {
		addrs = append(addrs, filepath.Join(home, ".docker", "run", "docker.sock"))
	}
	if runtime.GOOS == "windows" {
		// Docker Desktop on Windows serves a named pipe, which the standard
		// library cannot dial; DOCKER_HOST=tcp://… is required there.
		return "unix", nil
	}
	return "unix", addrs
}
