package gpu

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// parseDevices parses
// `nvidia-smi --query-gpu=index,uuid,name,memory.total --format=csv,noheader,nounits`.
func parseDevices(out []byte) ([]Device, error) {
	var devs []Device
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		f := strings.Split(line, ",")
		if len(f) < 4 {
			return nil, fmt.Errorf("gpu: unexpected nvidia-smi line %q", line)
		}
		for i := range f {
			f[i] = strings.TrimSpace(f[i])
		}
		idx, err := strconv.Atoi(f[0])
		if err != nil {
			return nil, fmt.Errorf("gpu: bad device index in %q", line)
		}
		mem, _ := strconv.ParseInt(f[len(f)-1], 10, 64)
		if f[1] == "" {
			return nil, fmt.Errorf("gpu: no uuid in %q", line)
		}
		devs = append(devs, Device{
			Index:     idx,
			UUID:      f[1],
			Name:      strings.Join(f[2:len(f)-1], ","),
			MemoryMiB: mem,
		})
	}
	return devs, nil
}

// addMIG fills in MIG slices from `nvidia-smi -L`:
//
//	GPU 0: NVIDIA A100-SXM4-40GB (UUID: GPU-5d5b…)
//	  MIG 1g.5gb      Device  0: (UUID: MIG-c6d4…)
func addMIG(devs []Device, out []byte) {
	byUUID := make(map[string]*Device, len(devs))
	for i := range devs {
		byUUID[devs[i].UUID] = &devs[i]
	}
	var cur *Device
	for _, line := range strings.Split(string(out), "\n") {
		t := strings.TrimSpace(line)
		uuid := uuidOf(t)
		switch {
		case strings.HasPrefix(t, "GPU "):
			cur = byUUID[uuid]
		case strings.HasPrefix(t, "MIG ") && cur != nil && uuid != "":
			rest := strings.TrimSpace(strings.TrimPrefix(t, "MIG "))
			fields := strings.Fields(rest)
			s := MIGSlice{UUID: uuid, Index: -1}
			if len(fields) > 0 {
				s.Profile = fields[0]
			}
			for i, f := range fields {
				if f == "Device" && i+1 < len(fields) {
					if n, err := strconv.Atoi(strings.TrimSuffix(fields[i+1], ":")); err == nil {
						s.Index = n
					}
				}
			}
			cur.MIG = append(cur.MIG, s)
		}
	}
}

func uuidOf(line string) string {
	_, rest, ok := strings.Cut(line, "(UUID: ")
	if !ok {
		return ""
	}
	u, _, ok := strings.Cut(rest, ")")
	if !ok {
		return ""
	}
	return strings.TrimSpace(u)
}

// parseMetrics parses
// `nvidia-smi --query-gpu=index,utilization.gpu,memory.used,power.draw --format=csv,noheader,nounits`.
// Values nvidia-smi does not report ("[N/A]", "[Not Supported]") become -1.
func parseMetrics(out []byte, at time.Time) map[string]DeviceMetrics {
	ms := make(map[string]DeviceMetrics)
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Split(strings.TrimSpace(line), ",")
		if len(f) != 4 {
			continue
		}
		key := strings.TrimSpace(f[0])
		idx, err := strconv.Atoi(key)
		if err != nil {
			continue
		}
		ms[key] = DeviceMetrics{
			Index:          idx,
			UtilizationPct: metric(f[1]),
			MemoryUsedMiB:  metric(f[2]),
			PowerW:         metric(f[3]),
			SampledAt:      at,
		}
	}
	return ms
}

func metric(s string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return -1
	}
	return v
}
