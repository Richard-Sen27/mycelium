package sharing

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
)

func TestLoadMissing(t *testing.T) {
	s, err := Load(t.TempDir())
	if err != nil || s != nil {
		t.Fatalf("Load(missing) = %v, %v; want nil, nil", s, err)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	tests := []*mycelv1.SharingSettings{
		Default([]string{"docker", "gpu"}),
		{},
		{Offer: caps()},
		{Offer: caps("ollama"), When: "schedule", Schedule: []*mycelv1.ScheduleWindow{win("fri,sat", "22:00", "07:00"), win("sun", "00:00", "24:00")}, LocalFirst: true, MaxJobs: 1, IdleAfterMinutes: 30},
		{When: "idle", IdleAfterMinutes: 5},
		{When: "idle", Schedule: []*mycelv1.ScheduleWindow{win("mon", "08:00", "09:00")}},
	}
	for i, want := range tests {
		dir := filepath.Join(t.TempDir(), "node")
		if err := Save(dir, want); err != nil {
			t.Fatalf("%d: Save: %v", i, err)
		}
		got, err := Load(dir)
		if err != nil {
			t.Fatalf("%d: Load: %v", i, err)
		}
		if !proto.Equal(got, want) {
			t.Fatalf("%d: round trip = %v, want %v", i, got, want)
		}
		fi, err := os.Stat(filepath.Join(dir, FileName))
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
			t.Fatalf("%d: mode %v, want 0600", i, fi.Mode().Perm())
		}
		entries, _ := os.ReadDir(dir)
		if len(entries) != 1 {
			t.Fatalf("%d: leftover files: %v", i, entries)
		}
	}
}

func TestSaveNilRemoves(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, &mycelv1.SharingSettings{MaxJobs: 1}); err != nil {
		t.Fatal(err)
	}
	if err := Save(dir, nil); err != nil {
		t.Fatal(err)
	}
	if s, err := Load(dir); err != nil || s != nil {
		t.Fatalf("Load after Save(nil) = %v, %v", s, err)
	}
	if err := Save(dir, nil); err != nil {
		t.Fatalf("Save(nil) twice: %v", err)
	}
}

func TestSaveRejectsInvalid(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, &mycelv1.SharingSettings{When: "sometimes"}); err == nil {
		t.Fatal("Save accepted invalid settings")
	}
	if _, err := os.Stat(filepath.Join(dir, FileName)); !os.IsNotExist(err) {
		t.Fatalf("file written for invalid settings: %v", err)
	}
}

func TestLoadFile(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want *mycelv1.SharingSettings
		err  string
	}{
		{"empty file", "", &mycelv1.SharingSettings{}, ""},
		{"proto names", `
offer: [gpu, docker]
when: schedule
schedule:
  - days: [mon, tue]
    from: "22:00"
    to: "07:00"
local_first: true
max_jobs: 3
idle_after_minutes: 20
`, &mycelv1.SharingSettings{Offer: caps("gpu", "docker"), When: "schedule", Schedule: []*mycelv1.ScheduleWindow{win("mon,tue", "22:00", "07:00")}, LocalFirst: true, MaxJobs: 3, IdleAfterMinutes: 20}, ""},
		{"empty offer list", "offer: []\n", &mycelv1.SharingSettings{Offer: caps()}, ""},
		{"null offer offers nothing", "offer:\nmax_jobs: 1\n", &mycelv1.SharingSettings{Offer: caps(), MaxJobs: 1}, ""},
		{"explicit null offer", "offer: null\n", &mycelv1.SharingSettings{Offer: caps()}, ""},
		{"absent offer is no restriction", "max_jobs: 1\n", &mycelv1.SharingSettings{MaxJobs: 1}, ""},
		{"offer not a list", "offer: {gpu: true}\n", nil, "offer"},
		{"trailing document", "max_jobs: 1\n---\nmax_jobs: 9\n", nil, "single YAML document"},
		{"comment-only file", "# nothing here\n", &mycelv1.SharingSettings{}, ""},
		{"too large", "max_jobs: 1\n" + strings.Repeat("#", 64<<10) + "\n", nil, "exceeds"},
		{"unknown field", "maxJobs: 3\n", nil, "maxJobs"},
		{"invalid", "offer: [tpu]\n", nil, "unknown offer"},
		{"garbage", "offer: {\n", nil, "parse"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, FileName), []byte(tt.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := Load(dir)
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("Load error = %v, want %q", err, tt.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !proto.Equal(got, tt.want) {
				t.Fatalf("Load = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSavedYAMLUsesProtoNames(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, &mycelv1.SharingSettings{Offer: caps("gpu"), LocalFirst: true, MaxJobs: 1, IdleAfterMinutes: 12}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"offer:", "local_first: true", "max_jobs: 1", "idle_after_minutes: 12"} {
		if !strings.Contains(string(data), k) {
			t.Errorf("saved YAML lacks %q:\n%s", k, data)
		}
	}
}
