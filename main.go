package main

import (
	"bufio"
	"embed"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed static
var staticFiles embed.FS

// ---------- Disk ----------

type FSInfo struct {
	MountPoint string  `json:"mountPoint"`
	Device     string  `json:"device"`
	FsType     string  `json:"fsType"`
	Total      uint64  `json:"total"`
	Free       uint64  `json:"free"`
	Used       uint64  `json:"used"`
	UsedPct    float64 `json:"usedPct"`
}

type mountEntry struct {
	device     string
	mountPoint string
	fsType     string
}

func parseMounts() ([]mountEntry, error) {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var entries []mountEntry
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		entries = append(entries, mountEntry{
			device:     fields[0],
			mountPoint: fields[1],
			fsType:     fields[2],
		})
	}
	return entries, scanner.Err()
}

func isRealDevice(device string) bool {
	return strings.HasPrefix(device, "/dev/") || strings.Contains(device, ":")
}

func getDiskInfo() ([]FSInfo, error) {
	entries, err := parseMounts()
	if err != nil {
		return nil, err
	}

	// Detect whether the host root filesystem is bind-mounted at /host.
	hasHostMount := false
	for _, e := range entries {
		if e.mountPoint == "/host" && isRealDevice(e.device) {
			hasHostMount = true
			break
		}
	}

	var result []FSInfo
	seen := make(map[string]bool)

	for _, e := range entries {
		if !isRealDevice(e.device) {
			continue
		}

		var displayPath, statPath string

		if hasHostMount {
			if e.mountPoint == "/host" {
				displayPath = "/"
				statPath = "/host"
			} else if strings.HasPrefix(e.mountPoint, "/host/") {
				displayPath = e.mountPoint[5:]
				statPath = e.mountPoint
			} else {
				continue
			}
		} else {
			displayPath = e.mountPoint
			statPath = e.mountPoint
		}

		if seen[displayPath] {
			continue
		}
		seen[displayPath] = true

		var stat syscall.Statfs_t
		if err := syscall.Statfs(statPath, &stat); err != nil {
			log.Printf("statfs %s: %v", statPath, err)
			continue
		}

		bsize := uint64(stat.Bsize)
		total := stat.Blocks * bsize
		free := stat.Bfree * bsize
		used := total - free
		var usedPct float64
		if total > 0 {
			usedPct = float64(used) / float64(total) * 100
		}

		result = append(result, FSInfo{
			MountPoint: displayPath,
			Device:     e.device,
			FsType:     e.fsType,
			Total:      total,
			Free:       free,
			Used:       used,
			UsedPct:    usedPct,
		})
	}
	return result, nil
}

// ---------- Proc helper ----------

// procPath returns the path to a /proc file, preferring the host's proc
// filesystem when the host root is bind-mounted at /host. On Linux,
// /proc/stat and /proc/meminfo are kernel-wide (not namespaced), so both
// paths return identical data — but using /host/proc makes the intent clear.
func procPath(file string) string {
	host := "/host/proc/" + file
	if _, err := os.Stat(host); err == nil {
		return host
	}
	return "/proc/" + file
}

// ---------- CPU ----------

type cpuStat struct {
	total uint64
	idle  uint64
}

var (
	prevCPU   cpuStat
	prevCores []cpuStat
	prevCPUMu sync.Mutex
)

// parseCPUStat extracts total and idle ticks from a /proc/stat cpu* field slice.
func parseCPUStat(fields []string) cpuStat {
	// fields[0] is the cpu label; fields[1..] are:
	// user nice system idle iowait irq softirq steal guest guest_nice
	var v [10]uint64
	for i := 1; i < len(fields) && i-1 < len(v); i++ {
		v[i-1], _ = strconv.ParseUint(fields[i], 10, 64)
	}
	total := v[0] + v[1] + v[2] + v[3] + v[4] + v[5] + v[6] + v[7]
	idle := v[3] + v[4] // idle + iowait
	return cpuStat{total: total, idle: idle}
}

// readCPUStats reads /proc/stat and returns the aggregate CPU stat plus one
// entry per logical core (cpu0, cpu1, …).
func readCPUStats() (agg cpuStat, cores []cpuStat, err error) {
	f, err := os.Open(procPath("stat"))
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "cpu") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		s := parseCPUStat(fields)
		if fields[0] == "cpu" {
			agg = s
		} else {
			cores = append(cores, s)
		}
	}
	err = scanner.Err()
	return
}

// diffPct computes the usage percentage between two consecutive cpu readings.
func diffPct(cur, prev cpuStat) float64 {
	dTotal := cur.total - prev.total
	dIdle := cur.idle - prev.idle
	if dTotal == 0 {
		return 0
	}
	pct := float64(dTotal-dIdle) / float64(dTotal) * 100
	if pct < 0 {
		return 0
	}
	return pct
}

// getCPUPcts returns the aggregate CPU percentage and a per-core slice.
func getCPUPcts() (float64, []float64) {
	curAgg, curCores, err := readCPUStats()
	if err != nil {
		log.Printf("cpu: %v", err)
		return 0, nil
	}

	prevCPUMu.Lock()
	prevAgg := prevCPU
	prevCoresCopy := make([]cpuStat, len(prevCores))
	copy(prevCoresCopy, prevCores)
	prevCPU = curAgg
	prevCores = curCores
	prevCPUMu.Unlock()

	corePcts := make([]float64, len(curCores))
	for i, cur := range curCores {
		if i < len(prevCoresCopy) {
			corePcts[i] = diffPct(cur, prevCoresCopy[i])
		}
	}
	return diffPct(curAgg, prevAgg), corePcts
}

// ---------- Memory ----------

type MemInfo struct {
	Total     uint64  `json:"total"`
	Used      uint64  `json:"used"`
	Free      uint64  `json:"free"`
	Available uint64  `json:"available"`
	UsedPct   float64 `json:"usedPct"`
}

type SwapInfo struct {
	Total   uint64  `json:"total"`
	Used    uint64  `json:"used"`
	Free    uint64  `json:"free"`
	UsedPct float64 `json:"usedPct"`
}

type SystemInfo struct {
	CPUPct float64   `json:"cpuPct"`
	Cores  []float64 `json:"cores"`
	Memory MemInfo   `json:"memory"`
	Swap   SwapInfo  `json:"swap"`
}

func getSystemInfo() (SystemInfo, error) {
	f, err := os.Open(procPath("meminfo"))
	if err != nil {
		return SystemInfo{}, err
	}
	defer f.Close()

	data := make(map[string]uint64)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		key := strings.TrimSuffix(fields[0], ":")
		val, _ := strconv.ParseUint(fields[1], 10, 64)
		data[key] = val * 1024 // kB → bytes
	}
	if err := scanner.Err(); err != nil {
		return SystemInfo{}, err
	}

	memTotal := data["MemTotal"]
	memAvail := data["MemAvailable"]
	memUsed := memTotal - memAvail
	var memPct float64
	if memTotal > 0 {
		memPct = float64(memUsed) / float64(memTotal) * 100
	}

	swapTotal := data["SwapTotal"]
	swapFree := data["SwapFree"]
	swapUsed := swapTotal - swapFree
	var swapPct float64
	if swapTotal > 0 {
		swapPct = float64(swapUsed) / float64(swapTotal) * 100
	}

	aggPct, corePcts := getCPUPcts()
	return SystemInfo{
		CPUPct: aggPct,
		Cores:  corePcts,
		Memory: MemInfo{
			Total:     memTotal,
			Used:      memUsed,
			Free:      data["MemFree"],
			Available: memAvail,
			UsedPct:   memPct,
		},
		Swap: SwapInfo{
			Total:   swapTotal,
			Used:    swapUsed,
			Free:    swapFree,
			UsedPct: swapPct,
		},
	}, nil
}

// ---------- Hardware ----------

// HardwareInfo holds static CPU and RAM details, read once at startup.
type HardwareInfo struct {
	CPUModel string `json:"cpuModel"` // e.g. "Intel(R) Core(TM) i9-13900K CPU @ 3.00GHz"
	RAMType  string `json:"ramType"`  // e.g. "DDR4"
	RAMSpeed string `json:"ramSpeed"` // e.g. "3200 MT/s"
}

var (
	hwOnce  sync.Once
	hwCache HardwareInfo
)

func getCPUModel() string {
	f, err := os.Open(procPath("cpuinfo"))
	if err != nil {
		return "Unknown"
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "model name") {
			if parts := strings.SplitN(line, ":", 2); len(parts) == 2 {
				return strings.TrimSpace(parts[1])
			}
		}
	}
	return "Unknown"
}

// smbiosMemType maps SMBIOS Type 17 memory-type byte to a name.
func smbiosMemType(t byte) string {
	switch t {
	case 0x12:
		return "DDR"
	case 0x13:
		return "DDR2"
	case 0x18:
		return "DDR3"
	case 0x1A:
		return "DDR4"
	case 0x1B:
		return "LPDDR"
	case 0x1C:
		return "LPDDR2"
	case 0x1D:
		return "LPDDR3"
	case 0x1E:
		return "LPDDR4"
	case 0x22:
		return "DDR5"
	case 0x23:
		return "LPDDR5"
	default:
		return ""
	}
}

// getRAMDetails parses SMBIOS Type 17 (Memory Device) records from the DMI
// table and returns the type and speed of the first populated slot found.
func getRAMDetails() (ramType, ramSpeed string) {
	var data []byte
	for _, p := range []string{"/sys/firmware/dmi/tables/DMITable", "/host/sys/firmware/dmi/tables/DMITable"} {
		if b, err := os.ReadFile(p); err == nil && len(b) > 0 {
			data = b
			break
		}
	}
	if data == nil {
		return "Unknown", ""
	}

	i := 0
	for i < len(data) {
		if i+4 > len(data) {
			break
		}
		sType := data[i]
		sLen := int(data[i+1])
		if sLen < 4 || i+sLen > len(data) {
			break
		}

		// Skip past the strings section to locate the next structure.
		next := i + sLen
		for next+1 < len(data) {
			if data[next] == 0 && data[next+1] == 0 {
				next += 2
				break
			}
			next++
		}

		if sType == 17 && sLen >= 0x17 {
			// Offset 0x0C: size in MB (0 = not installed).
			sz := binary.LittleEndian.Uint16(data[i+0x0C:])
			if sz == 0 {
				i = next
				continue
			}
			mt := smbiosMemType(data[i+0x12])
			if mt != "" {
				ramType = mt
				// Offset 0x15: speed in MT/s.
				speed := binary.LittleEndian.Uint16(data[i+0x15:])
				// Offset 0x20: configured speed (prefer if available).
				if sLen >= 0x23 && i+0x22 <= len(data) {
					if cs := binary.LittleEndian.Uint16(data[i+0x20:]); cs > 0 {
						speed = cs
					}
				}
				if speed > 0 {
					ramSpeed = fmt.Sprintf("%d MT/s", speed)
				}
				return ramType, ramSpeed
			}
		}
		i = next
	}
	if ramType == "" {
		ramType = "Unknown"
	}
	return ramType, ramSpeed
}

func getHardwareInfo() HardwareInfo {
	hwOnce.Do(func() {
		ramType, ramSpeed := getRAMDetails()
		hwCache = HardwareInfo{
			CPUModel: getCPUModel(),
			RAMType:  ramType,
			RAMSpeed: ramSpeed,
		}
	})
	return hwCache
}

// ---------- Processes ----------

const clockTicksPerSec = 100 // Linux USER_HZ

// ProcInfo holds per-process CPU and memory stats.
type ProcInfo struct {
	PID      int     `json:"pid"`
	Name     string  `json:"name"`
	CPUPct   float64 `json:"cpuPct"`
	MemBytes uint64  `json:"memBytes"`
}

// ProcessesResponse carries two pre-sorted lists for the two UI views.
type ProcessesResponse struct {
	ByCPU []ProcInfo `json:"byCPU"`
	ByMem []ProcInfo `json:"byMem"`
}

type procSnapshot struct {
	ticks uint64
	at    time.Time
}

var (
	prevProcSnaps map[int]procSnapshot
	prevProcMu    sync.Mutex
)

// procBaseDir returns the /proc directory, preferring the host bind-mount.
func procBaseDir() string {
	if _, err := os.Stat("/host/proc/1"); err == nil {
		return "/host/proc"
	}
	return "/proc"
}

// readProcStat returns the process name and total CPU ticks (utime+stime).
func readProcStat(base string, pid int) (name string, ticks uint64, err error) {
	data, err := os.ReadFile(fmt.Sprintf("%s/%d/stat", base, pid))
	if err != nil {
		return "", 0, err
	}
	s := string(data)
	start := strings.Index(s, "(")
	end := strings.LastIndex(s, ")")
	if start < 0 || end < 0 {
		return "", 0, fmt.Errorf("bad stat format")
	}
	name = s[start+1 : end]
	rest := strings.Fields(s[end+2:])
	// After state: index 11=utime, 12=stime (0-based from rest[0]=state)
	if len(rest) < 13 {
		return name, 0, nil
	}
	utime, _ := strconv.ParseUint(rest[11], 10, 64)
	stime, _ := strconv.ParseUint(rest[12], 10, 64)
	return name, utime + stime, nil
}

// readProcMem returns VmRSS in bytes from /proc/[pid]/status.
func readProcMem(base string, pid int) uint64 {
	f, err := os.Open(fmt.Sprintf("%s/%d/status", base, pid))
	if err != nil {
		return 0
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 && fields[0] == "VmRSS:" {
			v, _ := strconv.ParseUint(fields[1], 10, 64)
			return v * 1024
		}
	}
	return 0
}

func getProcesses() (ProcessesResponse, error) {
	base := procBaseDir()
	now := time.Now()

	entries, err := os.ReadDir(base)
	if err != nil {
		return ProcessesResponse{}, err
	}

	prevProcMu.Lock()
	oldSnaps := prevProcSnaps
	newSnaps := make(map[int]procSnapshot, len(entries))
	prevProcMu.Unlock()

	var all []ProcInfo
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		name, ticks, err := readProcStat(base, pid)
		if err != nil {
			continue
		}
		mem := readProcMem(base, pid)
		newSnaps[pid] = procSnapshot{ticks: ticks, at: now}

		var cpuPct float64
		if old, ok := oldSnaps[pid]; ok {
			if dt := now.Sub(old.at).Seconds(); dt > 0 && ticks >= old.ticks {
				cpuPct = float64(ticks-old.ticks) / (dt * clockTicksPerSec) * 100
			}
		}
		all = append(all, ProcInfo{PID: pid, Name: name, CPUPct: cpuPct, MemBytes: mem})
	}

	prevProcMu.Lock()
	prevProcSnaps = newSnaps
	prevProcMu.Unlock()

	const topN = 5
	byCPU := make([]ProcInfo, len(all))
	copy(byCPU, all)
	sort.Slice(byCPU, func(i, j int) bool { return byCPU[i].CPUPct > byCPU[j].CPUPct })
	if len(byCPU) > topN {
		byCPU = byCPU[:topN]
	}

	byMem := make([]ProcInfo, len(all))
	copy(byMem, all)
	sort.Slice(byMem, func(i, j int) bool { return byMem[i].MemBytes > byMem[j].MemBytes })
	if len(byMem) > topN {
		byMem = byMem[:topN]
	}

	return ProcessesResponse{ByCPU: byCPU, ByMem: byMem}, nil
}

// ---------- Main ----------

func main() {
	// Prime the CPU counter so the first API call returns a real delta.
	if agg, cores, err := readCPUStats(); err == nil {
		prevCPUMu.Lock()
		prevCPU = agg
		prevCores = cores
		prevCPUMu.Unlock()
	}

	// Prime the process snapshots.
	prevProcMu.Lock()
	prevProcSnaps = make(map[int]procSnapshot)
	prevProcMu.Unlock()
	getProcesses() //nolint — discard first sample, used only to populate snapshots

	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		log.Fatal(err)
	}
	http.Handle("/", http.FileServer(http.FS(sub)))

	http.HandleFunc("/api/disk", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-cache")
		info, err := getDiskInfo()
		if err != nil {
			http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusInternalServerError)
			return
		}
		if info == nil {
			info = []FSInfo{}
		}
		json.NewEncoder(w).Encode(info)
	})

	http.HandleFunc("/api/system", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-cache")
		info, err := getSystemInfo()
		if err != nil {
			http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(info)
	})

	http.HandleFunc("/api/processes", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-cache")
		info, err := getProcesses()
		if err != nil {
			http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(info)
	})

	log.Println("listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
