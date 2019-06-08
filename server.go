package zdns

import (
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/mpolden/zdns/dns"
	"github.com/mpolden/zdns/hosts"
)

const (
	// HijackZero returns the zero IP address to matching requests.
	HijackZero = iota
	// HijackEmpty returns an empty answer to matching requests.
	HijackEmpty
	// HijackHosts returns the value of the  hoss entry to matching request.
	HijackHosts
)

// A Server defines parameters for running a DNS server.
type Server struct {
	Config  Config
	hosts   hosts.Hosts
	logger  *log.Logger
	proxy   *dns.Proxy
	ticker  *time.Ticker
	done    chan bool
	signal  chan os.Signal
	mu      sync.RWMutex
	started bool
}

// NewServer returns a new server configured according to config.
func NewServer(logger *log.Logger, config Config) (*Server, error) {
	server := &Server{
		Config: config,
		signal: make(chan os.Signal, 1),
		done:   make(chan bool, 1),
		logger: logger,
	}

	// Start goroutines
	if config.Filter.refreshInterval > 0 {
		server.ticker = time.NewTicker(config.Filter.refreshInterval)
		go server.reloadHosts()
	}
	signal.Notify(server.signal)
	go server.readSignal()

	// Configure proxy
	server.proxy = dns.NewProxy(server.hijack, config.Resolvers, config.Resolver.timeout)

	// Load initial hosts
	server.loadHosts()

	server.started = true
	return server, nil
}

func readHosts(name string) (hosts.Hosts, error) {
	url, err := url.Parse(name)
	if err != nil {
		return nil, err
	}
	var rc io.ReadCloser
	switch url.Scheme {
	case "file":
		f, err := os.Open(url.Path)
		if err != nil {
			return nil, err
		}
		rc = f
	case "http", "https":
		client := http.Client{Timeout: 10 * time.Second}
		res, err := client.Get(url.String())
		if err != nil {
			return nil, err
		}
		rc = res.Body
	default:
		return nil, fmt.Errorf("%s: invalid scheme: %s", url, url.Scheme)
	}
	hosts, err := hosts.Parse(rc)
	if err1 := rc.Close(); err == nil {
		err = err1
	}
	return hosts, err
}

func nonFqdn(s string) string {
	sz := len(s)
	if sz > 0 && s[sz-1:] == "." {
		return s[:sz-1]
	}
	return s
}

func (s *Server) logf(format string, v ...interface{}) {
	if s.logger != nil {
		s.logger.Printf(format, v...)
	}
}

func (s *Server) readSignal() {
	for {
		select {
		case <-s.done:
			signal.Stop(s.signal)
			return
		case sig := <-s.signal:
			switch sig {
			case syscall.SIGHUP:
				s.logf("received signal %s: reloading filters", sig)
				s.loadHosts()
			case syscall.SIGTERM, syscall.SIGINT:
				s.logf("received signal %s: shutting down", sig)
				if err := s.Close(); err != nil {
					s.logf("close failed: %s", err)
				}
			default:
				s.logf("received signal %s: ignoring", sig)
			}
		}
	}
}

func (s *Server) reloadHosts() {
	for {
		select {
		case <-s.done:
			s.ticker.Stop()
			return
		case <-s.ticker.C:
			s.loadHosts()
		}
	}
}

func (s *Server) loadHosts() {
	hs := make(hosts.Hosts)
	for _, f := range s.Config.Filters {
		src := "inline hosts"
		hs1 := f.hosts
		if f.URL != "" {
			src = f.URL
			var err error
			hs1, err = readHosts(f.URL)
			if err != nil {
				s.logf("failed to read hosts from %s: %s", f.URL, err)
				continue
			}
		}
		if f.Reject {
			for name, ipAddrs := range hs1 {
				hs[name] = ipAddrs
			}
			s.logf("loaded %d hosts from %s", len(hs1), src)
		} else {
			removed := 0
			for hostToRemove := range hs1 {
				if _, ok := hs.Get(hostToRemove); ok {
					removed++
					hs.Del(hostToRemove)
				}
			}
			if removed > 0 {
				s.logf("removed %d hosts from %s", removed, src)
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hosts = hs
	s.logf("loaded %d hosts in total", len(hs))
}

// Close terminates all active operations and shuts down the DNS server.
func (s *Server) Close() error {
	if !s.started {
		return nil
	}
	s.done <- true
	s.done <- true
	return s.proxy.Close()
}

func (s *Server) hijack(r *dns.Request) *dns.Reply {
	if r.Type != dns.TypeA && r.Type != dns.TypeAAAA {
		return nil // Type not applicable
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	ipAddrs, ok := s.hosts.Get(nonFqdn(r.Name))
	if !ok {
		return nil // No match
	}
	switch s.Config.Filter.hijackMode {
	case HijackZero:
		switch r.Type {
		case dns.TypeA:
			return dns.ReplyA(r.Name, net.IPv4zero)
		case dns.TypeAAAA:
			return dns.ReplyAAAA(r.Name, net.IPv6zero)
		}
	case HijackEmpty:
		return &dns.Reply{}
	case HijackHosts:
		var ipv4Addr []net.IP
		var ipv6Addr []net.IP
		for _, ipAddr := range ipAddrs {
			if ipAddr.IP.To4() == nil {
				ipv6Addr = append(ipv6Addr, ipAddr.IP)
			} else {
				ipv4Addr = append(ipv4Addr, ipAddr.IP)
			}
		}
		switch r.Type {
		case dns.TypeA:
			return dns.ReplyA(r.Name, ipv4Addr...)
		case dns.TypeAAAA:
			return dns.ReplyAAAA(r.Name, ipv6Addr...)
		}
	}
	return nil
}

// ListenAndServe starts a server on configured address and protocol.
func (s *Server) ListenAndServe() error {
	return s.proxy.ListenAndServe(s.Config.Listen, s.Config.Protocol)
}
