package testhelper

import (
	"fmt"
	"sync"
	"testing"

	"github.com/miekg/dns"
	"github.com/stretchr/testify/require"
)

// FakeDNSServer starts a fake DNS server serving real DNS queries via UDP. The answers are returned
// from an input handler method.
type FakeDNSServer struct {
	t         *testing.T
	mux       *dns.ServeMux
	dnsServer *dns.Server
	handlers  map[uint16]fakeDNSHandler
}

type fakeDNSHandler func(string) []string

// WithHandler adds a handler for an input DNS record type
func (s *FakeDNSServer) WithHandler(t uint16, handler fakeDNSHandler) *FakeDNSServer {
	s.handlers[t] = handler
	return s
}

// Start starts the DNS name server. The server stops in the test clean-up phase
func (s *FakeDNSServer) Start() *FakeDNSServer {
	s.mux.HandleFunc(".", func(writer dns.ResponseWriter, msg *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(msg)
		m.Compress = false

		switch msg.Opcode {
		case dns.OpcodeQuery:
			for _, q := range m.Question {
				handler := s.handlers[q.Qtype]
				if handler == nil {
					continue
				}

				for _, answer := range handler(q.Name) {
					rr, err := dns.NewRR(fmt.Sprintf("%s %s %s", q.Name, dns.Type(q.Qtype).String(), answer))
					require.NoError(s.t, err)
					m.Answer = append(m.Answer, rr)
				}
			}
		}

		require.NoError(s.t, writer.WriteMsg(m))
	})

	var wg sync.WaitGroup
	s.dnsServer.NotifyStartedFunc = func() { wg.Done() }

	wg.Add(1)
	go func() { require.NoError(s.t, s.dnsServer.ListenAndServe()) }()

	s.t.Cleanup(func() {
		require.NoError(s.t, s.dnsServer.Shutdown())
	})

	wg.Wait()
	return s
}

// Addr returns the UDP address used to access the DNS nameserver
func (s *FakeDNSServer) Addr() string {
	return s.dnsServer.PacketConn.LocalAddr().String()
}

// NewFakeDNSServer returns a new real fake DNS server object
func NewFakeDNSServer(t *testing.T) *FakeDNSServer {
	mux := dns.NewServeMux()
	dnsServer := &dns.Server{
		Addr:    ":0",
		Net:     "udp",
		Handler: mux,
	}
	return &FakeDNSServer{
		t:         t,
		mux:       mux,
		dnsServer: dnsServer,
		handlers:  make(map[uint16]fakeDNSHandler),
	}
}
