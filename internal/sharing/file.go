package sharing

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
)

// FileName is the local ceiling's file name inside the node directory.
const FileName = "sharing.yaml"

// fileSettings is the YAML form of SharingSettings, with the proto field
// names. offer is a plain list; leaving the key out means no restriction,
// an empty list or a null value offers nothing.
type fileSettings struct {
	Offer            *[]string    `yaml:"offer,omitempty"`
	When             string       `yaml:"when,omitempty"`
	Schedule         []fileWindow `yaml:"schedule,omitempty"`
	LocalFirst       bool         `yaml:"local_first"`
	MaxJobs          int32        `yaml:"max_jobs,omitempty"`
	IdleAfterMinutes int32        `yaml:"idle_after_minutes,omitempty"`
}

// fileSettingsIn is fileSettings for reading: offer stays a node so an
// absent key (no restriction) differs from "offer:" with a null value
// (offer nothing).
type fileSettingsIn struct {
	Offer            yaml.Node    `yaml:"offer"`
	When             string       `yaml:"when"`
	Schedule         []fileWindow `yaml:"schedule"`
	LocalFirst       bool         `yaml:"local_first"`
	MaxJobs          int32        `yaml:"max_jobs"`
	IdleAfterMinutes int32        `yaml:"idle_after_minutes"`
}

// maxFileSize caps sharing.yaml.
const maxFileSize = 64 << 10

func readCapped(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		return nil, fmt.Errorf("sharing: read %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxFileSize+1))
	if err != nil {
		return nil, fmt.Errorf("sharing: read %s: %w", path, err)
	}
	if len(data) > maxFileSize {
		return nil, fmt.Errorf("sharing: %s exceeds %d bytes", path, maxFileSize)
	}
	return data, nil
}

type fileWindow struct {
	Days []string `yaml:"days,flow"`
	From string   `yaml:"from"`
	To   string   `yaml:"to"`
}

// Load reads <NodeDir>/sharing.yaml; a missing file yields nil (no local ceiling).
func Load(nodeDir string) (*mycelv1.SharingSettings, error) {
	path := filepath.Join(nodeDir, FileName)
	data, err := readCapped(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f fileSettingsIn
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("sharing: parse %s: %w", path, err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("sharing: parse %s: want a single YAML document", path)
	}
	s := &mycelv1.SharingSettings{
		When:             f.When,
		LocalFirst:       f.LocalFirst,
		MaxJobs:          f.MaxJobs,
		IdleAfterMinutes: f.IdleAfterMinutes,
	}
	switch {
	case f.Offer.Kind == 0: // key absent: no restriction
	case f.Offer.ShortTag() == "!!null": // "offer:" with no value: offer nothing
		s.Offer = &mycelv1.Capabilities{Names: []string{}}
	default:
		names := []string{}
		if err := f.Offer.Decode(&names); err != nil {
			return nil, fmt.Errorf("sharing: parse %s: offer: %w", path, err)
		}
		s.Offer = &mycelv1.Capabilities{Names: names}
	}
	for _, w := range f.Schedule {
		s.Schedule = append(s.Schedule, &mycelv1.ScheduleWindow{Days: w.Days, From: w.From, To: w.To})
	}
	if err := Validate(s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// Save writes <NodeDir>/sharing.yaml atomically (0600). Saving nil removes
// the file (no local ceiling).
func Save(nodeDir string, s *mycelv1.SharingSettings) error {
	path := filepath.Join(nodeDir, FileName)
	if s == nil {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("sharing: remove %s: %w", path, err)
		}
		return nil
	}
	if err := Validate(s); err != nil {
		return err
	}
	f := fileSettings{
		When:             s.GetWhen(),
		LocalFirst:       s.GetLocalFirst(),
		MaxJobs:          s.GetMaxJobs(),
		IdleAfterMinutes: s.GetIdleAfterMinutes(),
	}
	if s.GetOffer() != nil {
		names := append([]string{}, s.GetOffer().GetNames()...)
		f.Offer = &names
	}
	for _, w := range s.GetSchedule() {
		f.Schedule = append(f.Schedule, fileWindow{Days: w.GetDays(), From: w.GetFrom(), To: w.GetTo()})
	}
	data, err := yaml.Marshal(&f)
	if err != nil {
		return fmt.Errorf("sharing: encode: %w", err)
	}
	if err := os.MkdirAll(nodeDir, 0o700); err != nil {
		return fmt.Errorf("sharing: %w", err)
	}
	return writeFileAtomic(path, data)
}

// writeFileAtomic writes data to a temporary file next to path with mode
// 0600, syncs it and renames it into place.
func writeFileAtomic(path string, data []byte) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("sharing: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if err = tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("sharing: %w", err)
	}
	if _, err = tmp.Write(data); err != nil {
		return fmt.Errorf("sharing: write %s: %w", tmp.Name(), err)
	}
	if err = tmp.Sync(); err != nil {
		return fmt.Errorf("sharing: sync %s: %w", tmp.Name(), err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("sharing: close %s: %w", tmp.Name(), err)
	}
	if err = os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("sharing: rename to %s: %w", path, err)
	}
	return nil
}
