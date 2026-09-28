package main

// Faucet rate limiting (added 2026-09-28): 1 claim per address per 24h, 1 per client IP per hour.
// Client IP = X-Real-IP (set by nginx) when the request comes from loopback, else RemoteAddr.

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	faucetAddrWindow = 24 * time.Hour
	faucetIPWindow   = time.Hour
)

type faucetLimits struct {
	mu   sync.Mutex
	path string
	Addr map[string]int64 `json:"addr"`
	IP   map[string]int64 `json:"ip"`
}

func loadFaucetLimits(path string) *faucetLimits {
	f := &faucetLimits{path: path, Addr: map[string]int64{}, IP: map[string]int64{}}
	if b, err := os.ReadFile(path); err == nil {
		json.Unmarshal(b, f)
		if f.Addr == nil {
			f.Addr = map[string]int64{}
		}
		if f.IP == nil {
			f.IP = map[string]int64{}
		}
	}
	return f
}

func (f *faucetLimits) check(addr, ip string) time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	var wait time.Duration
	if t, ok := f.Addr[addr]; ok {
		if d := time.Unix(t, 0).Add(faucetAddrWindow).Sub(now); d > wait {
			wait = d
		}
	}
	if t, ok := f.IP[ip]; ok {
		if d := time.Unix(t, 0).Add(faucetIPWindow).Sub(now); d > wait {
			wait = d
		}
	}
	return wait
}

func (f *faucetLimits) record(addr, ip string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	f.Addr[addr] = now.Unix()
	f.IP[ip] = now.Unix()
	for k, t := range f.Addr { // prune
		if now.Sub(time.Unix(t, 0)) > faucetAddrWindow {
			delete(f.Addr, k)
		}
	}
	for k, t := range f.IP {
		if now.Sub(time.Unix(t, 0)) > faucetIPWindow {
			delete(f.IP, k)
		}
	}
	if b, err := json.Marshal(f); err == nil {
		os.WriteFile(f.path+".tmp", b, 0644)
		os.Rename(f.path+".tmp", f.path)
	}
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		if x := strings.TrimSpace(r.Header.Get("X-Real-IP")); x != "" {
			return x
		}
		if x := r.Header.Get("X-Forwarded-For"); x != "" {
			return strings.TrimSpace(strings.Split(x, ",")[0])
		}
	}
	return host
}
