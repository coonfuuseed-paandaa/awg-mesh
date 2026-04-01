package dns

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"

	mdns "github.com/miekg/dns"
)

// Server is an embedded DNS server for the overlay network zone.
type Server struct {
	zone     string
	listen   string
	upstream string

	mu     sync.RWMutex
	aMap   map[string]net.IP // fqdn -> IP
	ptrMap map[string]string // reversed IP -> fqdn

	fwdClient *mdns.Client // reused for forwarding queries
	server    *mdns.Server
}

// NewServer creates a new DNS server with the given configuration.
// Returns nil if zone is empty — DNS server is optional.
func NewServer(zone, listen, upstream string, records []Record) *Server {
	trimmedZone := strings.TrimSpace(zone)
	if trimmedZone == "" {
		return nil
	}
	if !strings.HasSuffix(trimmedZone, ".") {
		trimmedZone += "."
	}

	trimmedListen := strings.TrimSpace(listen)
	if trimmedListen == "" {
		trimmedListen = "0.0.0.0:53"
	}

	trimmedUpstream := strings.TrimSpace(upstream)
	if trimmedUpstream == "" {
		trimmedUpstream = "1.1.1.1"
	}
	if !strings.Contains(trimmedUpstream, ":") {
		trimmedUpstream += ":53"
	}

	s := &Server{
		zone:      trimmedZone,
		listen:    trimmedListen,
		upstream:  trimmedUpstream,
		fwdClient: &mdns.Client{Net: "udp"},
		aMap:      make(map[string]net.IP),
		ptrMap:    make(map[string]string),
	}

	s.loadRecords(records)
	return s
}

// loadRecords populates the internal record maps.
func (s *Server) loadRecords(records []Record) {
	aMap := make(map[string]net.IP, len(records)/2)
	ptrMap := make(map[string]string, len(records)/2)

	for _, r := range records {
		switch r.Type {
		case "A":
			ip := net.ParseIP(r.Value)
			if ip != nil {
				aMap[strings.ToLower(r.Name)] = ip
			}
		case "PTR":
			ptrMap[strings.ToLower(r.Name)] = r.Value
		}
	}

	s.mu.Lock()
	s.aMap = aMap
	s.ptrMap = ptrMap
	s.mu.Unlock()
}

// UpdateRecords replaces all DNS records. Thread-safe.
func (s *Server) UpdateRecords(records []Record) {
	s.loadRecords(records)
}

// Start starts the DNS server and blocks until ctx is cancelled.
func (s *Server) Start(ctx context.Context) error {
	mux := mdns.NewServeMux()
	mux.HandleFunc(s.zone, s.handleZone)
	mux.HandleFunc("in-addr.arpa.", s.handlePTR)
	mux.HandleFunc(".", s.handleForward)

	s.server = &mdns.Server{
		Addr:    s.listen,
		Net:     "udp",
		Handler: mux,
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- s.server.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("dns server failed: %w", err)
	case <-ctx.Done():
		if s.server != nil {
			_ = s.server.Shutdown()
		}
		return nil
	}
}

// handleZone handles queries for the overlay zone (A records).
func (s *Server) handleZone(w mdns.ResponseWriter, r *mdns.Msg) {
	msg := new(mdns.Msg)
	msg.SetReply(r)
	msg.Authoritative = true

	for _, q := range r.Question {
		name := strings.ToLower(q.Name)

		switch q.Qtype {
		case mdns.TypeA:
			s.mu.RLock()
			ip, exists := s.aMap[name]
			s.mu.RUnlock()

			if exists {
				msg.Answer = append(msg.Answer, &mdns.A{
					Hdr: mdns.RR_Header{
						Name:   q.Name,
						Rrtype: mdns.TypeA,
						Class:  mdns.ClassINET,
						Ttl:    60,
					},
					A: ip,
				})
			}
		}
	}

	_ = w.WriteMsg(msg)
}

// handlePTR handles reverse DNS queries.
func (s *Server) handlePTR(w mdns.ResponseWriter, r *mdns.Msg) {
	msg := new(mdns.Msg)
	msg.SetReply(r)
	msg.Authoritative = true

	for _, q := range r.Question {
		if q.Qtype != mdns.TypePTR {
			continue
		}

		name := strings.ToLower(q.Name)
		s.mu.RLock()
		fqdn, exists := s.ptrMap[name]
		s.mu.RUnlock()

		if exists {
			msg.Answer = append(msg.Answer, &mdns.PTR{
				Hdr: mdns.RR_Header{
					Name:   q.Name,
					Rrtype: mdns.TypePTR,
					Class:  mdns.ClassINET,
					Ttl:    60,
				},
				Ptr: fqdn,
			})
		}
	}

	_ = w.WriteMsg(msg)
}

// handleForward forwards non-zone queries to the upstream DNS server.
func (s *Server) handleForward(w mdns.ResponseWriter, r *mdns.Msg) {
	resp, _, err := s.fwdClient.Exchange(r, s.upstream)
	if err != nil {
		msg := new(mdns.Msg)
		msg.SetReply(r)
		msg.Rcode = mdns.RcodeServerFailure
		_ = w.WriteMsg(msg)
		return
	}
	_ = w.WriteMsg(resp)
}
