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
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	dbus "github.com/godbus/dbus/v5"
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
	f, err := os.Open(procPath("mounts"))
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

func isVirtioFS(entry mountEntry) bool {
	return entry.fsType == "virtiofs"
}

func getDiskInfo() ([]FSInfo, error) {
	entries, err := parseMounts()
	if err != nil {
		return nil, err
	}

	// When running in a container, procPath returns /host/proc/mounts, so
	// mount points are host-native paths; prepend /host to reach them via statfs.
	inContainer := strings.HasPrefix(procPath("mounts"), "/host/")

	var result []FSInfo
	seen := make(map[string]bool)

	for _, e := range entries {
		if !isRealDevice(e.device) && !isVirtioFS(e) {
			continue
		}

		displayPath := e.mountPoint
		statPath := e.mountPoint
		if inContainer {
			statPath = "/host" + e.mountPoint
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

type LoadAvg struct {
	One    float64 `json:"one"`
	Five   float64 `json:"five"`
	Fifteen float64 `json:"fifteen"`
}

type SystemInfo struct {
	CPUPct     float64   `json:"cpuPct"`
	Cores      []float64 `json:"cores"`
	Memory     MemInfo   `json:"memory"`
	Swap       SwapInfo  `json:"swap"`
	Uptime     string    `json:"uptime"`     // e.g. "3d 14h 22m"
	LoadAvg    LoadAvg   `json:"loadAvg"`
	TotalProcs int       `json:"totalProcs"`
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
	loadAvg, totalProcs := getLoadAvg()
	return SystemInfo{
		CPUPct:     aggPct,
		Cores:      corePcts,
		Uptime:     getUptime(),
		LoadAvg:    loadAvg,
		TotalProcs: totalProcs,
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
	CPUModel  string `json:"cpuModel"`  // e.g. "Intel(R) Core(TM) i9-13900K CPU @ 3.00GHz"
	RAMType   string `json:"ramType"`   // e.g. "DDR4"
	RAMSpeed  string `json:"ramSpeed"`  // e.g. "3200 MT/s"
	Hostname  string `json:"hostname"`  // host system hostname
	OS        string `json:"os"`        // e.g. "Ubuntu 22.04.3 LTS"
	OSId      string `json:"osId"`      // e.g. "ubuntu", "debian", "arch"
	Kernel     string  `json:"kernel"`     // e.g. "6.6.114.1-microsoft-standard-WSL2"
	CoreCount  int     `json:"coreCount"`  // number of logical CPU cores
	Arch       string  `json:"arch"`       // e.g. "x86_64", "aarch64"
	CPUMaxMHz  float64 `json:"cpuMaxMHz"`  // e.g. 5200.0
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

func getUptime() string {
	data, err := os.ReadFile(procPath("uptime"))
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return ""
	}
	secs, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return ""
	}
	total := int(secs)
	days := total / 86400
	hours := (total % 86400) / 3600
	mins := (total % 3600) / 60
	if days > 0 {
		return fmt.Sprintf("%dd %dh %dm", days, hours, mins)
	}
	if hours > 0 {
		return fmt.Sprintf("%dh %dm", hours, mins)
	}
	return fmt.Sprintf("%dm", mins)
}

func getOSInfo() (name, id string) {
	for _, p := range []string{"/host/etc/os-release", "/etc/os-release"} {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		defer f.Close()
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "PRETTY_NAME=") && name == "" {
				name = strings.Trim(strings.TrimPrefix(line, "PRETTY_NAME="), `"`)
			}
			if strings.HasPrefix(line, "ID=") && id == "" {
				id = strings.Trim(strings.TrimPrefix(line, "ID="), `"`)
			}
		}
		if name != "" {
			return name, id
		}
	}
	return "Linux", ""
}

func getKernelVersion() string {
	for _, p := range []string{"/host/proc/sys/kernel/osrelease", "/proc/sys/kernel/osrelease"} {
		if data, err := os.ReadFile(p); err == nil {
			return strings.TrimSpace(string(data))
		}
	}
	return "unknown"
}

func getCoreCount() int {
	f, err := os.Open(procPath("cpuinfo"))
	if err != nil {
		return 0
	}
	defer f.Close()
	count := 0
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "processor") {
			count++
		}
	}
	return count
}

func getLoadAvg() (LoadAvg, int) {
	data, err := os.ReadFile(procPath("loadavg"))
	if err != nil {
		return LoadAvg{}, 0
	}
	// Format: "0.52 0.34 0.28 2/456 12345"
	fields := strings.Fields(string(data))
	if len(fields) < 4 {
		return LoadAvg{}, 0
	}
	one, _     := strconv.ParseFloat(fields[0], 64)
	five, _    := strconv.ParseFloat(fields[1], 64)
	fifteen, _ := strconv.ParseFloat(fields[2], 64)
	totalProcs := 0
	if parts := strings.SplitN(fields[3], "/", 2); len(parts) == 2 {
		totalProcs, _ = strconv.Atoi(parts[1])
	}
	return LoadAvg{One: one, Five: five, Fifteen: fifteen}, totalProcs
}

// goarchToDisplay maps Go's GOARCH values to conventional display names.
func goarchToDisplay() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	case "386":
		return "i386"
	case "arm":
		return "armv7l"
	default:
		return runtime.GOARCH
	}
}

func getCPUMaxMHz() float64 {
	// Try sysfs cpufreq first (most accurate).
	for _, p := range []string{
		"/host/sys/devices/system/cpu/cpu0/cpufreq/cpuinfo_max_freq",
		"/sys/devices/system/cpu/cpu0/cpufreq/cpuinfo_max_freq",
	} {
		if data, err := os.ReadFile(p); err == nil {
			if khz, err := strconv.ParseFloat(strings.TrimSpace(string(data)), 64); err == nil {
				return khz / 1000.0
			}
		}
	}
	// Fallback: read "cpu MHz" from /proc/cpuinfo.
	f, err := os.Open(procPath("cpuinfo"))
	if err != nil {
		return 0
	}
	defer f.Close()
	var maxMHz float64
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "cpu MHz") {
			if parts := strings.SplitN(line, ":", 2); len(parts) == 2 {
				if mhz, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64); err == nil && mhz > maxMHz {
					maxMHz = mhz
				}
			}
		}
	}
	return maxMHz
}

// getHostname returns the host system's hostname. When running in a container
// with the host root bind-mounted at /host, it reads /host/etc/hostname
// directly to avoid returning the container's hostname.
func getHostname() string {
	if data, err := os.ReadFile("/host/etc/hostname"); err == nil {
		return strings.TrimSpace(string(data))
	}
	if h, err := os.Hostname(); err == nil {
		return h
	}
	return "unknown"
}

func getHardwareInfo() HardwareInfo {
	hwOnce.Do(func() {
		ramType, ramSpeed := getRAMDetails()
		osName, osId := getOSInfo()
		hwCache = HardwareInfo{
			CPUModel:  getCPUModel(),
			RAMType:   ramType,
			RAMSpeed:  ramSpeed,
			Hostname:  getHostname(),
			OS:        osName,
			OSId:      osId,
			Kernel:    getKernelVersion(),
			CoreCount: getCoreCount(),
			Arch:      goarchToDisplay(),
			CPUMaxMHz: getCPUMaxMHz(),
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

// ---------- Network ----------

// NetIface holds per-interface I/O rates computed from consecutive /proc/net/dev samples.
type NetIface struct {
	Name    string  `json:"name"`
	RxRate  float64 `json:"rxRate"`  // bytes/sec
	TxRate  float64 `json:"txRate"`  // bytes/sec
	RxTotal uint64  `json:"rxTotal"` // cumulative bytes
	TxTotal uint64  `json:"txTotal"` // cumulative bytes
	MAC     string  `json:"mac"`
	Speed   int     `json:"speed"`  // Mbps, 0 = unknown
	Driver  string  `json:"driver"`
}

// nicSysPath returns the path to a file under /sys/class/net/<iface>/,
// preferring the host bind-mount when available.
func nicSysPath(iface, file string) string {
	host := fmt.Sprintf("/host/sys/class/net/%s/%s", iface, file)
	if _, err := os.Stat(host); err == nil {
		return host
	}
	return fmt.Sprintf("/sys/class/net/%s/%s", iface, file)
}

func readNICDetails(iface string) (mac, driver string, speed int) {
	if b, err := os.ReadFile(nicSysPath(iface, "address")); err == nil {
		mac = strings.TrimSpace(string(b))
	}
	if link, err := os.Readlink(nicSysPath(iface, "device/driver")); err == nil {
		driver = filepath.Base(link)
	}
	if b, err := os.ReadFile(nicSysPath(iface, "speed")); err == nil {
		if v, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && v > 0 {
			speed = v
		}
	}
	return mac, driver, speed
}

type netSnapshot struct {
	rx uint64
	tx uint64
	at time.Time
}

var (
	prevNetSnaps map[string]netSnapshot
	prevNetMu    sync.Mutex
)

func getNetworkIO() ([]NetIface, error) {
	f, err := os.Open(procPath("net/dev"))
	if err != nil {
		return nil, err
	}
	defer f.Close()

	now := time.Now()
	prevNetMu.Lock()
	old := prevNetSnaps
	newSnaps := make(map[string]netSnapshot)
	prevNetMu.Unlock()

	var result []NetIface
	scanner := bufio.NewScanner(f)
	// Skip two header lines.
	scanner.Scan()
	scanner.Scan()
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		colon := strings.Index(line, ":")
		if colon < 0 {
			continue
		}
		name := strings.TrimSpace(line[:colon])
		if name == "lo" {
			continue
		}
		fields := strings.Fields(line[colon+1:])
		if len(fields) < 9 {
			continue
		}
		rxBytes, _ := strconv.ParseUint(fields[0], 10, 64)
		txBytes, _ := strconv.ParseUint(fields[8], 10, 64)
		newSnaps[name] = netSnapshot{rx: rxBytes, tx: txBytes, at: now}

		var rxRate, txRate float64
		if prev, ok := old[name]; ok {
			if dt := now.Sub(prev.at).Seconds(); dt > 0 {
				if rxBytes >= prev.rx {
					rxRate = float64(rxBytes-prev.rx) / dt
				}
				if txBytes >= prev.tx {
					txRate = float64(txBytes-prev.tx) / dt
				}
			}
		}
		mac, driver, speed := readNICDetails(name)
		result = append(result, NetIface{
			Name:    name,
			RxRate:  rxRate,
			TxRate:  txRate,
			RxTotal: rxBytes,
			TxTotal: txBytes,
			MAC:     mac,
			Speed:   speed,
			Driver:  driver,
		})
	}

	prevNetMu.Lock()
	prevNetSnaps = newSnaps
	prevNetMu.Unlock()

	return result, scanner.Err()
}

// ---------- Services ----------

// ServiceInfo holds the state of a single systemd service unit.
type ServiceInfo struct {
	Name        string `json:"name"`
	LoadState   string `json:"loadState"`
	ActiveState string `json:"activeState"`
	SubState    string `json:"subState"`
	Description string `json:"description"`
}

// ServicesResponse wraps the services list with an optional error message.
type ServicesResponse struct {
	Services []ServiceInfo `json:"services"`
	Error    string        `json:"error,omitempty"`
}

var (
	svcCacheMu   sync.Mutex
	svcCacheResp ServicesResponse
	svcCacheAt   time.Time
)

const svcCacheTTL = 5 * time.Second

// dbusSockPath returns the first reachable D-Bus system socket path.
func dbusSockPath() string {
	for _, p := range []string{"/host/run/dbus/system_bus_socket", "/run/dbus/system_bus_socket"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// unitRecord matches the D-Bus ListUnits return signature:
// (ssssssouso) — name, description, load, active, sub, following,
// object-path, job-id, job-type, job-object-path
type unitRecord struct {
	Name        string
	Description string
	LoadState   string
	ActiveState string
	SubState    string
	Following   string
	Path        dbus.ObjectPath
	JobID       uint32
	JobType     string
	JobPath     dbus.ObjectPath
}

func fetchServices() ServicesResponse {
	sock := dbusSockPath()
	if sock == "" {
		return ServicesResponse{Services: []ServiceInfo{},
			Error: "D-Bus system socket not found (checked /host/run/dbus and /run/dbus)"}
	}

	conn, err := dbus.Dial("unix:path=" + sock)
	if err != nil {
		return ServicesResponse{Services: []ServiceInfo{},
			Error: fmt.Sprintf("D-Bus connect (%s): %v", sock, err)}
	}
	defer conn.Close()

	if err := conn.Auth(nil); err != nil {
		return ServicesResponse{Services: []ServiceInfo{},
			Error: fmt.Sprintf("D-Bus auth: %v", err)}
	}
	if err := conn.Hello(); err != nil {
		return ServicesResponse{Services: []ServiceInfo{},
			Error: fmt.Sprintf("D-Bus hello: %v", err)}
	}

	obj := conn.Object("org.freedesktop.systemd1", "/org/freedesktop/systemd1")
	var units []unitRecord
	if err := obj.Call("org.freedesktop.systemd1.Manager.ListUnits", 0).Store(&units); err != nil {
		return ServicesResponse{Services: []ServiceInfo{},
			Error: fmt.Sprintf("ListUnits: %v", err)}
	}

	var services []ServiceInfo
	for _, u := range units {
		if !strings.HasSuffix(u.Name, ".service") {
			continue
		}
		services = append(services, ServiceInfo{
			Name:        strings.TrimSuffix(u.Name, ".service"),
			LoadState:   u.LoadState,
			ActiveState: u.ActiveState,
			SubState:    u.SubState,
			Description: u.Description,
		})
	}
	return ServicesResponse{Services: services}
}

func getServices() ServicesResponse {
	svcCacheMu.Lock()
	defer svcCacheMu.Unlock()
	if time.Since(svcCacheAt) < svcCacheTTL && svcCacheResp.Services != nil {
		return svcCacheResp
	}
	svcCacheResp = fetchServices()
	svcCacheAt = time.Now()
	return svcCacheResp
}

// ---------- Logs ----------

// LogEntry holds a single parsed journal log line.
type LogEntry struct {
	Timestamp string `json:"timestamp"`
	Message   string `json:"message"`
	Priority  int    `json:"priority"` // 0=emerg … 7=debug
}

var unitNameRe = regexp.MustCompile(`^[a-zA-Z0-9@._-]+$`)

func validUnitName(name string) bool {
	return len(name) > 0 && len(name) <= 256 && unitNameRe.MatchString(name)
}

func fetchLogs(unit string) ([]LogEntry, error) {
	cmd := exec.Command("journalctl",
		"--root=/host",
		"-u", unit+".service",
		"-n", "20",
		"--no-pager",
		"--output=json",
		"--utc",
	)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("journalctl: %v", err)
	}

	var entries []LogEntry
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			continue
		}

		// MESSAGE can be a string or an array of byte values.
		var msg string
		if m, ok := raw["MESSAGE"]; ok {
			if err := json.Unmarshal(m, &msg); err != nil {
				// binary message — decode as byte array
				var bytes []int
				if err2 := json.Unmarshal(m, &bytes); err2 == nil {
					b := make([]byte, len(bytes))
					for i, v := range bytes {
						b[i] = byte(v)
					}
					msg = string(b)
				}
			}
		}

		var ts string
		if t, ok := raw["__REALTIME_TIMESTAMP"]; ok {
			var usStr string
			if err := json.Unmarshal(t, &usStr); err == nil {
				if us, err := strconv.ParseInt(usStr, 10, 64); err == nil {
					ts = time.UnixMicro(us).UTC().Format("Jan 02 15:04:05")
				}
			}
		}

		pri := 7
		if p, ok := raw["PRIORITY"]; ok {
			var pStr string
			if err := json.Unmarshal(p, &pStr); err == nil {
				if v, err := strconv.Atoi(pStr); err == nil {
					pri = v
				}
			}
		}

		entries = append(entries, LogEntry{
			Timestamp: ts,
			Message:   strings.TrimSpace(msg),
			Priority:  pri,
		})
	}
	return entries, nil
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

	// Prime the network snapshots.
	prevNetMu.Lock()
	prevNetSnaps = make(map[string]netSnapshot)
	prevNetMu.Unlock()
	getNetworkIO() //nolint — discard first sample, used only to populate snapshots

	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		log.Fatal(err)
	}
	http.Handle("/", http.FileServer(http.FS(sub)))

	http.HandleFunc("/api/stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-cache")

		sys, sysErr := getSystemInfo()
		disk, diskErr := getDiskInfo()
		procs, procsErr := getProcesses()
		net, netErr := getNetworkIO()

		if sysErr != nil || diskErr != nil || procsErr != nil || netErr != nil {
			var msgs []string
			for _, e := range []error{sysErr, diskErr, procsErr, netErr} {
				if e != nil {
					msgs = append(msgs, e.Error())
				}
			}
			http.Error(w, `{"error":"`+strings.Join(msgs, "; ")+`"}`, http.StatusInternalServerError)
			return
		}
		if disk == nil {
			disk = []FSInfo{}
		}
		if net == nil {
			net = []NetIface{}
		}
		json.NewEncoder(w).Encode(struct {
			System    SystemInfo        `json:"system"`
			Disk      []FSInfo          `json:"disk"`
			Processes ProcessesResponse `json:"processes"`
			Network   []NetIface        `json:"network"`
		}{sys, disk, procs, net})
	})

	http.HandleFunc("/api/services", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-cache")
		json.NewEncoder(w).Encode(getServices())
	})

	http.HandleFunc("/api/hardware", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		json.NewEncoder(w).Encode(getHardwareInfo())
	})

	http.HandleFunc("/api/logs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-cache")

		unit := r.URL.Query().Get("unit")
		if !validUnitName(unit) {
			http.Error(w, `{"error":"invalid unit name"}`, http.StatusBadRequest)
			return
		}

		entries, err := fetchLogs(unit)
		if err != nil {
			http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(entries)
	})

	log.Println("listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
