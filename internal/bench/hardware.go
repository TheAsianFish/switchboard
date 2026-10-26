package bench

import (
	"bufio"
	"os"
	"runtime"
	"strings"
)

// Hardware describes the machine a run executed on. CPUModel and Kernel are
// empty where /proc is not available.
type Hardware struct {
	NumCPU     int    `json:"num_cpu"`
	GOMAXPROCS int    `json:"gomaxprocs"`
	GOOS       string `json:"goos"`
	GOARCH     string `json:"goarch"`
	GoVersion  string `json:"go_version"`
	CPUModel   string `json:"cpu_model,omitempty"`
	Kernel     string `json:"kernel,omitempty"`
}

// DetectHardware reports the current machine.
func DetectHardware() Hardware {
	return Hardware{
		NumCPU:     runtime.NumCPU(),
		GOMAXPROCS: runtime.GOMAXPROCS(0),
		GOOS:       runtime.GOOS,
		GOARCH:     runtime.GOARCH,
		GoVersion:  runtime.Version(),
		CPUModel:   cpuModel("/proc/cpuinfo"),
		Kernel:     readTrimmed("/proc/sys/kernel/osrelease"),
	}
}

// cpuModel returns the first "model name" in a cpuinfo file, or "" if there
// is none (some ARM kernels do not report one).
func cpuModel(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		key, value, ok := strings.Cut(s.Text(), ":")
		if ok && strings.TrimSpace(key) == "model name" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func readTrimmed(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
