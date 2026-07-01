package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// networkStatusTTL bounds how long a cached IP/location/weather snapshot is
// considered fresh. Location and weather change slowly, so this is generous
// on purpose — it's the interval between background refreshes, not a
// staleness the user ever waits on (the statusline never blocks for it).
const networkStatusTTL = 20 * time.Minute

// networkStatusRefreshDebounce prevents every render during an in-flight
// (or just-failed) fetch from spawning its own redundant background process.
const networkStatusRefreshDebounce = 60 * time.Second

type networkStatus struct {
	IP           string  `json:"ip"`
	City         string  `json:"city"`
	Region       string  `json:"region"`
	Country      string  `json:"country"`
	WeatherTempC float64 `json:"weather_temp_c"`
	WeatherLabel string  `json:"weather_label"`
	FetchedAt    string  `json:"fetched_at"`
	OK           bool    `json:"ok"`
}

// networkInfoEnabled reports whether the IP/location/weather portion of the
// statusline's second line is shown. Default OFF: enabling it makes
// outbound HTTPS calls (ipapi.co, open-meteo.com) and puts your public IP
// and approximate location on screen, which most flow installs should not
// opt into silently. Set FLOW_STATUSLINE_NETWORK_INFO=1/true/on to enable.
func networkInfoEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("FLOW_STATUSLINE_NETWORK_INFO"))) {
	case "1", "true", "on", "yes":
		return true
	default:
		return false
	}
}

func networkStatusCachePath() (string, error) {
	root, err := flowRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "provider_usage", "network_status.json"), nil
}

func networkStatusLockPath() (string, error) {
	root, err := flowRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "provider_usage", "network_status.refreshing"), nil
}

// cachedNetworkStatus best-effort reads the last fetched IP/location/weather
// snapshot. Only ever touches local disk — never blocks on network.
func cachedNetworkStatus() (networkStatus, bool) {
	path, err := networkStatusCachePath()
	if err != nil {
		return networkStatus{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return networkStatus{}, false
	}
	var ns networkStatus
	if err := json.Unmarshal(data, &ns); err != nil {
		return networkStatus{}, false
	}
	return ns, ns.OK
}

// maybeTriggerNetworkStatusRefresh spawns a detached background process to
// refresh the IP/location/weather cache when it's missing or stale. It never
// waits for that process: the current statusline render always uses
// whatever is already cached (or nothing, on a cold start), so a slow or
// failing network call never stalls the prompt.
func maybeTriggerNetworkStatusRefresh() {
	cachePath, err := networkStatusCachePath()
	if err != nil {
		return
	}
	if info, statErr := os.Stat(cachePath); statErr == nil {
		if time.Since(info.ModTime()) < networkStatusTTL {
			return
		}
	}
	lockPath, err := networkStatusLockPath()
	if err != nil {
		return
	}
	if info, statErr := os.Stat(lockPath); statErr == nil && time.Since(info.ModTime()) < networkStatusRefreshDebounce {
		return // a refresh is already in flight (or just finished/failed)
	}
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return
	}
	if err := os.WriteFile(lockPath, []byte(time.Now().UTC().Format(time.RFC3339)), 0o644); err != nil {
		return
	}
	spawnNetworkStatusRefresh()
}

// spawnNetworkStatusRefresh launches the detached background refresh.
// A package-level var (like agentHookPost elsewhere in this file) so tests
// can stub it out instead of actually forking a process and hitting the
// network every time a statusline render checks a cold/stale cache.
var spawnNetworkStatusRefresh = func() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	cmd := exec.Command(exe, "hook", "__refresh-network-status")
	cmd.Env = os.Environ()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	_ = cmd.Start()
	// Deliberately not Wait()'d — fire-and-forget by design.
}

// cmdHookRefreshNetworkStatus performs the actual network calls and writes
// the cache. Only ever invoked out-of-band by
// maybeTriggerNetworkStatusRefresh, never inline in a statusline render.
func cmdHookRefreshNetworkStatus(args []string) int {
	ns := networkStatus{FetchedAt: time.Now().UTC().Format(time.RFC3339)}
	if loc, ok := fetchIPLocation(); ok {
		ns.IP, ns.City, ns.Region, ns.Country = loc.IP, loc.City, loc.Region, loc.Country
		ns.OK = true
		if temp, label, ok := fetchWeather(loc.Latitude, loc.Longitude); ok {
			ns.WeatherTempC, ns.WeatherLabel = temp, label
		}
	}
	path, err := networkStatusCachePath()
	if err != nil {
		return 0
	}
	data, err := json.MarshalIndent(ns, "", "  ")
	if err != nil {
		return 0
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return 0
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return 0
	}
	_ = os.Rename(tmp, path)
	return 0
}

type ipLocation struct {
	IP        string
	City      string
	Region    string
	Country   string
	Latitude  float64
	Longitude float64
}

// fetchIPLocation resolves the caller's public IP and approximate location
// via ipinfo.io (HTTPS, works unauthenticated at this call volume — a
// 20-minute-TTL background refresh per machine is far under any free-tier
// limit). ipapi.co was tried first but free-tier rate limiting made it
// unreliable from shared egress IPs (sandboxes, offices, CI).
func fetchIPLocation() (ipLocation, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://ipinfo.io/json", nil)
	if err != nil {
		return ipLocation{}, false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ipLocation{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ipLocation{}, false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return ipLocation{}, false
	}
	var raw struct {
		IP      string `json:"ip"`
		City    string `json:"city"`
		Region  string `json:"region"`
		Country string `json:"country"`
		Loc     string `json:"loc"` // "lat,lon"
	}
	if err := json.Unmarshal(body, &raw); err != nil || raw.IP == "" {
		return ipLocation{}, false
	}
	lat, lon := parseLatLon(raw.Loc)
	return ipLocation{
		IP: raw.IP, City: raw.City, Region: raw.Region, Country: raw.Country,
		Latitude: lat, Longitude: lon,
	}, true
}

// parseLatLon splits ipinfo.io's "loc" field, e.g. "13.0878,80.2785".
// Best-effort: a malformed value yields (0, 0) rather than an error, since
// the IP/city/region are still useful even without coordinates (weather
// just won't resolve).
func parseLatLon(loc string) (lat, lon float64) {
	parts := strings.SplitN(loc, ",", 2)
	if len(parts) != 2 {
		return 0, 0
	}
	fmt.Sscanf(strings.TrimSpace(parts[0]), "%f", &lat)
	fmt.Sscanf(strings.TrimSpace(parts[1]), "%f", &lon)
	return lat, lon
}

// fetchWeather resolves current conditions at the given coordinates via
// Open-Meteo (HTTPS, no API key required).
func fetchWeather(lat, lon float64) (tempC float64, label string, ok bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	url := fmt.Sprintf("https://api.open-meteo.com/v1/forecast?latitude=%.4f&longitude=%.4f&current_weather=true", lat, lon)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, "", false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, "", false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return 0, "", false
	}
	var raw struct {
		CurrentWeather struct {
			Temperature float64 `json:"temperature"`
			WeatherCode int     `json:"weathercode"`
		} `json:"current_weather"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return 0, "", false
	}
	return raw.CurrentWeather.Temperature, weatherCodeLabel(raw.CurrentWeather.WeatherCode), true
}

// weatherCodeLabel maps WMO weather codes (the scheme Open-Meteo reports) to
// a short label. See https://open-meteo.com/en/docs for the full table.
func weatherCodeLabel(code int) string {
	switch {
	case code == 0:
		return "Clear"
	case code == 1:
		return "Mainly clear"
	case code == 2:
		return "Partly cloudy"
	case code == 3:
		return "Overcast"
	case code == 45 || code == 48:
		return "Fog"
	case code >= 51 && code <= 57:
		return "Drizzle"
	case code >= 61 && code <= 67:
		return "Rain"
	case code >= 71 && code <= 77:
		return "Snow"
	case code >= 80 && code <= 82:
		return "Rain showers"
	case code == 85 || code == 86:
		return "Snow showers"
	case code >= 95:
		return "Thunderstorm"
	default:
		return ""
	}
}

// renderNetworkStatusLine builds the statusline's second line: the bound
// flow task name (if any) plus, only when explicitly enabled, public
// IP/location/weather.
func renderNetworkStatusLine(taskLabel string) string {
	var parts []string
	if taskLabel != "" {
		parts = append(parts, fmt.Sprintf("%stask%s %s", ansiDim, ansiReset, taskLabel))
	}
	if networkInfoEnabled() {
		maybeTriggerNetworkStatusRefresh()
		if ns, ok := cachedNetworkStatus(); ok {
			if ns.IP != "" {
				parts = append(parts, ns.IP)
			}
			if loc := joinNonEmpty(", ", ns.City, ns.Region); loc != "" {
				parts = append(parts, loc)
			} else if ns.Country != "" {
				parts = append(parts, ns.Country)
			}
			if ns.WeatherLabel != "" {
				parts = append(parts, fmt.Sprintf("%.0f°C %s", ns.WeatherTempC, ns.WeatherLabel))
			}
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, ansiDim+" · "+ansiReset)
}

func joinNonEmpty(sep string, values ...string) string {
	var nonEmpty []string
	for _, v := range values {
		if v != "" {
			nonEmpty = append(nonEmpty, v)
		}
	}
	return strings.Join(nonEmpty, sep)
}
