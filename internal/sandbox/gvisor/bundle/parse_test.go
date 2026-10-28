package bundle

import (
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestParseStatsEvent(t *testing.T) {
	out := `{"type":"stats","id":"sbx-1","data":{"cpu":{"usage":{"total":2500000000,"kernel":500000000,"user":2000000000,"percpu":[1,2]}},"memory":{"cache":10,"usage":{"limit":4294967296,"usage":123456789,"max":200000000,"failcnt":0},"swap":{"usage":0}},"pids":{"current":7,"limit":512}}}` + "\n"
	st, err := ParseStatsEvent([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	if st.CPU.Usage.Total != 2500000000 || st.Memory.Usage.Usage != 123456789 || st.Pids.Current != 7 || st.Pids.Limit != 512 {
		t.Errorf("parsed %+v", st)
	}

	// Other events are skipped.
	st, err = ParseStatsEvent([]byte(`{"type":"oom","id":"sbx-1"}` + "\n" + out))
	if err != nil || st.Pids.Current != 7 {
		t.Errorf("after oom event: %+v %v", st, err)
	}
	// Stats without data are zero.
	if st, err := ParseStatsEvent([]byte(`{"type":"stats","id":"x","data":null}`)); err != nil || st.Pids.Current != 0 {
		t.Errorf("null data: %+v %v", st, err)
	}
	for _, bad := range []string{"", "not json", `{"type":"oom"}`, `{"type":"stats","data":{"cpu":"x"}}`} {
		if _, err := ParseStatsEvent([]byte(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestParseSmall(t *testing.T) {
	if n, err := ParseDiskUsage([]byte("4096\t/var/lib/mycel/sbx/x\n")); err != nil || n != 4096 {
		t.Errorf("du %d %v", n, err)
	}
	if _, err := ParseDiskUsage(nil); err == nil {
		t.Error("empty du accepted")
	}
	if n, err := ParsePid([]byte("42\n")); err != nil || n != 42 {
		t.Errorf("pid %d %v", n, err)
	}
	for _, bad := range []string{"", "0", "-1", "x"} {
		if _, err := ParsePid([]byte(bad)); err == nil {
			t.Errorf("pid %q accepted", bad)
		}
	}
}

func utf16le(s string, bom bool) []byte {
	var b []byte
	if bom {
		b = append(b, 0xff, 0xfe)
	}
	for _, u := range utf16.Encode([]rune(s)) {
		b = append(b, byte(u), byte(u>>8)) // #nosec G115 -- splitting a uint16 into bytes
	}
	return b
}

func TestDecodeWSLText(t *testing.T) {
	want := "Ubuntu\r\nmycel-sbx\r\n"
	for _, in := range [][]byte{utf16le(want, false), utf16le(want, true), []byte(want)} {
		if got := DecodeWSLText(in); got != want {
			t.Errorf("decode %q = %q", in, got)
		}
	}
}

func TestConfine(t *testing.T) {
	tests := []struct {
		in        string
		allowRoot bool
		want      string
	}{
		{"a.txt", false, "/workspace/a.txt"},
		{"dir/b/c", false, "/workspace/dir/b/c"},
		{"./x//y/", false, "/workspace/x/y"},
		{".", true, "/workspace"},
		{".", false, ""},
		{"", true, ""},
		{"/etc/passwd", false, ""},
		{"../x", false, ""},
		{"a/../../x", false, ""},
		{"a/..", true, ""},
		{"a\x00b", false, ""},
		{`a\..\b`, false, ""},
	}
	for _, tt := range tests {
		got, err := Confine(tt.in, tt.allowRoot)
		if tt.want == "" {
			if err == nil || !errors.Is(err, ErrPath) {
				t.Errorf("Confine(%q) = %q, %v; want ErrPath", tt.in, got, err)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("Confine(%q) = %q, %v; want %q", tt.in, got, err, tt.want)
		}
	}
}

func TestGlob(t *testing.T) {
	out := []byte("./a.txt\x00./dist/x.whl\x00./dist/sub/y.whl\x00./report.xml\x00./t/r/junit.xml\x00")
	tests := []struct {
		patterns []string
		want     []string
	}{
		{[]string{"*.txt"}, []string{"a.txt"}},
		{[]string{"dist/*.whl"}, []string{"dist/x.whl"}},
		{[]string{"dist/**/*.whl"}, []string{"dist/sub/y.whl", "dist/x.whl"}},
		{[]string{"**/*.xml"}, []string{"report.xml", "t/r/junit.xml"}},
		{[]string{"**"}, []string{"a.txt", "dist/sub/y.whl", "dist/x.whl", "report.xml", "t/r/junit.xml"}},
		{[]string{"nothing"}, nil},
	}
	for _, tt := range tests {
		if got := FilterGlob(out, tt.patterns); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%v: got %v want %v", tt.patterns, got, tt.want)
		}
	}
	for _, bad := range []string{"/abs/*", "../*", "a/../../b", "[", ""} {
		if ValidatePattern(bad) == nil {
			t.Errorf("pattern %q accepted", bad)
		}
	}
	if ValidatePattern("src/**/*.go") != nil {
		t.Error("valid pattern refused")
	}
}

func TestSignalName(t *testing.T) {
	for in, want := range map[string]string{"TERM": "SIGTERM", "kill": "SIGKILL", "SIGINT": "SIGINT"} {
		if got, err := SignalName(in); err != nil || got != want {
			t.Errorf("%s -> %s %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "9", "SEGV", "--all"} {
		if _, err := SignalName(bad); err == nil {
			t.Errorf("signal %q accepted", bad)
		}
	}
}

func TestBufPipe(t *testing.T) {
	p := newBufPipe(4)
	done := make(chan struct{})
	go func() {
		_, _ = p.Write([]byte("hello world"))
		p.CloseWrite(nil)
		close(done)
	}()
	b, err := io.ReadAll(p)
	if err != nil || string(b) != "hello world" {
		t.Fatalf("%q %v", b, err)
	}
	<-done

	// A reader that gives up does not block the writer.
	q := newBufPipe(2)
	_ = q.Close()
	if n, err := q.Write([]byte(strings.Repeat("x", 100))); n != 100 || err != nil {
		t.Errorf("write after reader close: %d %v", n, err)
	}

	// The writer's error reaches the reader after the data.
	r := newBufPipe(8)
	_, _ = r.Write([]byte("ab"))
	r.CloseWrite(io.ErrUnexpectedEOF)
	b, err = io.ReadAll(r)
	if string(b) != "ab" || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("%q %v", b, err)
	}
}
