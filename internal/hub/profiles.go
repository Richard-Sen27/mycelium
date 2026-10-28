package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"gopkg.in/yaml.v3"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/internal/identity"
	"github.com/richard-sen27/mycelium/profiles"
)

// loadProfiles returns the built-in profiles embedded in the binary. Tests
// may swap it (loadRepoProfiles reads the repository's profiles/*.yaml).
var loadProfiles = profiles.All

// isFakeProfile reports whether p is the CI-only fake profile, which the
// hub offers only with Options.AllowFakeProfile.
var isFakeProfile = func(p *mycelv1.Profile) bool {
	return profiles.IsFake(p) || p.GetName() == "fake" || slices.Contains(p.GetBackends(), "fake")
}

// maxProfileFile bounds one profile YAML.
const maxProfileFile = 64 << 10

func loadRepoProfiles() ([]*mycelv1.Profile, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return nil, errors.New("hub: cannot locate the profiles directory")
	}
	dir := filepath.Join(filepath.Dir(file), "..", "..", "profiles")
	paths, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("hub: no profiles in %s", dir)
	}
	var out []*mycelv1.Profile
	for _, p := range paths {
		b, err := os.ReadFile(p) //nolint:gosec // fixed directory of the source tree
		if err != nil {
			return nil, err
		}
		prof, err := parseProfileYAML(b)
		if err != nil {
			return nil, fmt.Errorf("hub: profile %s: %w", filepath.Base(p), err)
		}
		out = append(out, prof)
	}
	return out, nil
}

// parseProfileYAML decodes a profile whose keys are the proto field names
// (limits.wall_clock is a duration string).
func parseProfileYAML(b []byte) (*mycelv1.Profile, error) {
	if len(b) > maxProfileFile {
		return nil, errors.New("profile file too large")
	}
	var v map[string]any
	if err := yaml.Unmarshal(b, &v); err != nil {
		return nil, err
	}
	j, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var p mycelv1.Profile
	if err := protojson.Unmarshal(j, &p); err != nil {
		return nil, err
	}
	if err := validateProfile(&p); err != nil {
		return nil, err
	}
	return &p, nil
}

func validateProfile(p *mycelv1.Profile) error {
	if !identity.ValidNodeName(p.GetName()) {
		return fmt.Errorf("invalid profile name %q", p.GetName())
	}
	if p.GetVersion() < 1 {
		return fmt.Errorf("profile %s: version must be at least 1", p.GetName())
	}
	if len(p.GetBackends()) == 0 {
		return fmt.Errorf("profile %s: no backends", p.GetName())
	}
	if _, err := profileWallClock(p); err != nil {
		return err
	}
	return nil
}

// profileWallClock is the profile's wall-clock limit.
func profileWallClock(p *mycelv1.Profile) (time.Duration, error) {
	d, err := time.ParseDuration(p.GetLimits().GetWallClock())
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("profile %s: invalid limits.wall_clock %q", p.GetName(), p.GetLimits().GetWallClock())
	}
	return d, nil
}

// syncProfiles stores the built-in profiles: new versions are inserted,
// stored ones are kept. The fake profile is stored only when allowFake.
// A failure to load the built-in set is logged, not fatal: the stored
// profiles stay usable.
func syncProfiles(ctx context.Context, st *store, allowFake bool, log *slog.Logger) error {
	ps, err := loadProfiles()
	if err != nil {
		log.Warn("built-in profiles not loaded", "err", err)
		return nil
	}
	for _, p := range ps {
		if err := validateProfile(p); err != nil {
			log.Warn("built-in profile skipped", "err", err)
			continue
		}
		if isFakeProfile(p) && !allowFake {
			continue
		}
		added, err := st.insertProfile(ctx, p)
		if err != nil {
			return fmt.Errorf("hub: store profile %s: %w", p.GetName(), err)
		}
		if added {
			log.Info("profile stored", "name", p.GetName(), "version", p.GetVersion())
		}
	}
	return nil
}

// profiles returns the newest version of each profile this hub offers.
func (s *server) profiles(ctx context.Context) ([]*mycelv1.Profile, error) {
	all, err := s.st.latestProfiles(ctx)
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, p := range all {
		if isFakeProfile(p) && !s.allowFake {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

// profile resolves a profile name to its newest offered version.
func (s *server) profile(ctx context.Context, name string) (*mycelv1.Profile, error) {
	ps, err := s.profiles(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range ps {
		if p.GetName() == name {
			return p, nil
		}
	}
	return nil, errNotFound
}
