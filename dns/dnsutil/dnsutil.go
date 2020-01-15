package dnsutil

import (
	"fmt"
	"sync"
	"time"

	"github.com/miekg/dns"
	"github.com/mpolden/zdns/dns/http"
)

var (
	// TypeToString contains a mapping of DNS request type to string.
	TypeToString = dns.TypeToString

	// RcodeToString contains a mapping of Mapping DNS response code to string.
	RcodeToString = dns.RcodeToString
)

// Exchanger is the interface that wraps the Exchange method of a DNS client.
type Exchanger interface {
	Exchange(*dns.Msg, string) (*dns.Msg, time.Duration, error)
}

// Client wraps a DNS client and a list of server addresses.
type Client struct {
	Exchanger Exchanger
	Addresses []string
}

// NewClient creates a new Client using the named network and addresses.
func NewClient(network string, timeout time.Duration, addresses ...string) *Client {
	var client Exchanger
	if network == "https" {
		client = http.NewClient(timeout)
	} else {
		client = &dns.Client{Net: network, Timeout: timeout}
	}
	return &Client{Exchanger: client, Addresses: addresses}
}

func multiExchange(exchanger Exchanger, msg *dns.Msg, address ...string) (*dns.Msg, error) {
	if len(address) == 0 {
		return nil, fmt.Errorf("no addresses to query")
	}
	responses := make(chan *dns.Msg, len(address))
	errs := make(chan error, len(address))
	var wg sync.WaitGroup
	for _, a := range address {
		wg.Add(1)
		go func(addr string) {
			defer wg.Done()
			r, _, err := exchanger.Exchange(msg, addr)
			if err != nil {
				errs <- fmt.Errorf("resolver %s failed: %w", addr, err)
				return
			}
			responses <- r
		}(a)
	}
	go func() {
		wg.Wait()
		close(errs)
		close(responses)
	}()
	for rr := range responses {
		return rr, nil
	}
	return nil, <-errs
}

// Exchange performs a synchronous DNS query. All addresses in Client c are queried in parallel and the first successful
// response is returned.
func (c *Client) Exchange(msg *dns.Msg) (*dns.Msg, error) {
	return multiExchange(c.Exchanger, msg, c.Addresses...)
}

// Answers returns all values in the answer section of DNS message msg.
func Answers(msg *dns.Msg) []string {
	var answers []string
	for _, answer := range msg.Answer {
		switch v := answer.(type) {
		case *dns.A:
			answers = append(answers, v.A.String())
		case *dns.AAAA:
			answers = append(answers, v.AAAA.String())
		case *dns.MX:
			answers = append(answers, v.Mx)
		case *dns.PTR:
			answers = append(answers, v.Ptr)
		case *dns.NS:
			answers = append(answers, v.Ns)
		case *dns.CNAME:
			answers = append(answers, v.Target)
		}
	}
	return answers
}

// MinTTL returns the lowest TTL of of answer, authority and additional sections.
func MinTTL(msg *dns.Msg) time.Duration {
	var ttl uint32 = (1 << 31) - 1 // Maximum TTL from RFC 2181
	for _, answer := range msg.Answer {
		ttl = min(answer.Header().Ttl, ttl)
	}
	for _, ns := range msg.Ns {
		ttl = min(ns.Header().Ttl, ttl)
	}
	for _, extra := range msg.Extra {
		// OPT (EDNS) is a pseudo record which uses TTL field for extended RCODE and flags
		if extra.Header().Rrtype == dns.TypeOPT {
			continue
		}
		ttl = min(extra.Header().Ttl, ttl)
	}
	return time.Duration(ttl) * time.Second
}

func min(x, y uint32) uint32 {
	if x < y {
		return x
	}
	return y
}
