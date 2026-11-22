package dns

import (
	"context"
	"math"
	"net"
	"net/netip"
	"strings"
	"time"

	"github.com/metacubex/mihomo/common/lru"
	"github.com/metacubex/mihomo/component/fakeip"
	"github.com/metacubex/mihomo/component/resolver"
	C "github.com/metacubex/mihomo/constant"
	icontext "github.com/metacubex/mihomo/context"
	"github.com/metacubex/mihomo/log"

	D "github.com/miekg/dns"
	"golang.org/x/exp/slices"
)

type (
	handler    func(ctx *icontext.DNSContext, r *D.Msg) (*D.Msg, error)
	middleware func(next handler) handler
)

func withHosts(mapping *lru.LruCache[netip.Addr, string]) middleware {
	return func(next handler) handler {
		return func(ctx *icontext.DNSContext, r *D.Msg) (*D.Msg, error) {
			q := r.Question[0]

			if !isIPRequest(q) {
				return next(ctx, r)
			}

			host := strings.TrimRight(q.Name, ".")
			handleCName := func(resp *D.Msg, domain string) {
				rr := &D.CNAME{}
				rr.Hdr = D.RR_Header{Name: q.Name, Rrtype: D.TypeCNAME, Class: D.ClassINET, Ttl: 10}
				rr.Target = domain + "."
				resp.Answer = append([]D.RR{rr}, resp.Answer...)
			}
			record, ok := resolver.DefaultHosts.Search(host, q.Qtype != D.TypeA && q.Qtype != D.TypeAAAA)
			if !ok {
				if record != nil && record.IsDomain {
					// replace request domain
					newR := r.Copy()
					newR.Question[0].Name = record.Domain + "."
					resp, err := next(ctx, newR)
					if err == nil {
						resp.Id = r.Id
						resp.Question = r.Question
						handleCName(resp, record.Domain)
					}
					return resp, err
				}
				return next(ctx, r)
			}

			msg := r.Copy()
			handleIPs := func() {
				for _, ipAddr := range record.IPs {
					if ipAddr.Is4() && q.Qtype == D.TypeA {
						rr := &D.A{}
						rr.Hdr = D.RR_Header{Name: q.Name, Rrtype: D.TypeA, Class: D.ClassINET, Ttl: 10}
						rr.A = ipAddr.AsSlice()
						msg.Answer = append(msg.Answer, rr)
						if mapping != nil {
							mapping.SetWithExpire(ipAddr, host, time.Now().Add(time.Second*10))
						}
					} else if ipAddr.Is6() && q.Qtype == D.TypeAAAA {
						rr := &D.AAAA{}
						rr.Hdr = D.RR_Header{Name: q.Name, Rrtype: D.TypeAAAA, Class: D.ClassINET, Ttl: 10}
						rr.AAAA = ipAddr.AsSlice()
						msg.Answer = append(msg.Answer, rr)
						if mapping != nil {
							mapping.SetWithExpire(ipAddr, host, time.Now().Add(time.Second*10))
						}
					}
				}
			}

			switch q.Qtype {
			case D.TypeA:
				handleIPs()
			case D.TypeAAAA:
				handleIPs()
			case D.TypeCNAME:
				handleCName(msg, record.Domain)
			default:
				return next(ctx, r)
			}

			ctx.SetType(icontext.DNSTypeHost)
			msg.SetRcode(r, D.RcodeSuccess)
			msg.Authoritative = true
			msg.RecursionAvailable = true
			return msg, nil
		}
	}
}

func withMapping(mapping *lru.LruCache[netip.Addr, string]) middleware {
	return func(next handler) handler {
		return func(ctx *icontext.DNSContext, r *D.Msg) (*D.Msg, error) {
			q := r.Question[0]

			if !isIPRequest(q) {
				return next(ctx, r)
			}

			msg, err := next(ctx, r)
			if err != nil {
				return nil, err
			}

			host := strings.TrimRight(q.Name, ".")

			for _, ans := range msg.Answer {
				var ip netip.Addr
				var ttl uint32

				switch a := ans.(type) {
				case *D.A:
					ip, _ = netip.AddrFromSlice(a.A)
					ttl = a.Hdr.Ttl
				case *D.AAAA:
					ip, _ = netip.AddrFromSlice(a.AAAA)
					ttl = a.Hdr.Ttl
				default:
					continue
				}
				if !ip.IsValid() {
					continue
				}
				if !ip.IsGlobalUnicast() {
					continue
				}
				ip = ip.Unmap()

				if ttl < 1 {
					ttl = 1
				}

				mapping.SetWithExpire(ip, host, time.Now().Add(time.Second*time.Duration(ttl)))
			}

			return msg, nil
		}
	}
}

func withFakeIP(skipper *fakeip.Skipper, fakePool *fakeip.Pool, fakePool6 *fakeip.Pool, fakeIPTTL int, httpsWait time.Duration) middleware {
	return func(next handler) handler {
		return func(ctx *icontext.DNSContext, r *D.Msg) (*D.Msg, error) {
			q := r.Question[0]

			host := strings.TrimRight(q.Name, ".")
			if skipper.ShouldSkipped(host) {
				return next(ctx, r)
			}

			var rr D.RR
			switch q.Qtype {
			case D.TypeA:
				if fakePool == nil {
					return handleMsgWithEmptyAnswer(r), nil
				}
				ip := fakePool.Lookup(host)
				rr = &D.A{
					Hdr: D.RR_Header{Name: q.Name, Rrtype: D.TypeA, Class: D.ClassINET, Ttl: dnsDefaultTTL},
					A:   ip.AsSlice(),
				}
			case D.TypeAAAA:
				if fakePool6 == nil {
					return handleMsgWithEmptyAnswer(r), nil
				}
				ip := fakePool6.Lookup(host)
				rr = &D.AAAA{
					Hdr:  D.RR_Header{Name: q.Name, Rrtype: D.TypeAAAA, Class: D.ClassINET, Ttl: dnsDefaultTTL},
					AAAA: ip.AsSlice(),
				}
			case D.TypeSVCB, D.TypeHTTPS:
				svcHost := trimAttrleaf(host)
				if skipper.ShouldSkipped(svcHost) {
					return next(ctx, r)
				}

				upstream, err := next(ctx, r)
				if err != nil {
					return upstream, err
				}
				ctx.SetType(icontext.DNSTypeFakeIP)
				msg := handleMsgWithEmptyAnswer(r)
				switch upstream.Rcode {
				case D.RcodeSuccess:
				case D.RcodeNameError:
					// the fake A/AAAA answers already claim the name exists
					return msg, nil
				default:
					// keep resolution failures visible so clients can refuse
					// to downgrade from ECH (RFC 9460 section 3.1)
					msg.Rcode = D.RcodeServerFailure
					return msg, nil
				}

				answer, ttl := fakeIPServiceBindings(upstream, q, svcHost, fakePool, fakePool6)
				if ttl > uint32(fakeIPTTL) {
					ttl = uint32(fakeIPTTL)
				}
				if answer != nil {
					msg.Answer = answer
					setMsgTTL(msg, ttl)
				}
				return msg, nil
			default:
				return next(ctx, r)
			}

			if httpsWait > 0 {
				waitFakeIPHTTPS(ctx, next, q.Name, httpsWait)
			}

			msg := r.Copy()
			msg.Answer = []D.RR{rr}

			ctx.SetType(icontext.DNSTypeFakeIP)
			setMsgTTL(msg, uint32(fakeIPTTL))
			msg.SetRcode(r, D.RcodeSuccess)
			msg.Authoritative = true
			msg.RecursionAvailable = true

			return msg, nil
		}
	}
}

// waitFakeIPHTTPS starts the upstream HTTPS lookup for name and waits for it,
// at most wait, before a fake A/AAAA answer is returned. Clients like Chromium
// wait only briefly for HTTPS once A/AAAA arrive, which leaves no room after
// an instant fake answer. The lookup keeps running past the wait so its result
// still reaches the cache, and the client's own HTTPS query joins it in the
// resolver instead of querying upstream again.
func waitFakeIPHTTPS(ctx context.Context, next handler, name string, wait time.Duration) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		lookupCtx, cancel := context.WithTimeout(context.Background(), resolver.DefaultDNSTimeout)
		defer cancel()
		_, _ = next(icontext.NewDNSContext(lookupCtx), new(D.Msg).SetQuestion(name, D.TypeHTTPS))
	}()

	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	case <-ctx.Done():
	}
}

// fakeIPServiceBindings rebuilds the HTTPS/SVCB answer of a fake-ip query so it
// agrees with the fake A/AAAA answers: the CNAME chain is collapsed onto the
// query name, TargetName becomes "." and IP hints point at the fake IPs of host.
// Records that would make the client resolve another name are not kept, and an
// RRset containing AliasMode is dropped entirely (RFC 9460 section 2.4.1).
// It returns nil when nothing is usable, and the lowest TTL of the records used.
func fakeIPServiceBindings(upstream *D.Msg, q D.Question, host string, fakePool, fakePool6 *fakeip.Pool) ([]D.RR, uint32) {
	ttl := uint32(math.MaxUint32)
	useTTL := func(hdr *D.RR_Header) {
		if hdr.Ttl < ttl {
			ttl = hdr.Ttl
		}
	}

	cnames := make(map[string]*D.CNAME)
	for _, rr := range upstream.Answer {
		if cname, ok := rr.(*D.CNAME); ok {
			cnames[D.CanonicalName(cname.Hdr.Name)] = cname
		}
	}
	name := D.CanonicalName(q.Name)
	for range cnames { // bounded, so CNAME loops terminate
		cname, ok := cnames[name]
		if !ok {
			break
		}
		useTTL(&cname.Hdr)
		name = D.CanonicalName(cname.Target)
	}
	if _, ok := cnames[name]; ok {
		return nil, 0
	}

	var answer []D.RR
	for _, rr := range upstream.Answer {
		hdr := rr.Header()
		if hdr.Rrtype != q.Qtype || D.CanonicalName(hdr.Name) != name {
			continue
		}
		rr = D.Copy(rr)
		var binding *D.SVCB
		switch rr := rr.(type) {
		case *D.HTTPS:
			binding = &rr.SVCB
		case *D.SVCB:
			binding = rr
		default:
			continue
		}
		if binding.Priority == 0 {
			return nil, 0
		}
		if binding.Target != "." && D.CanonicalName(binding.Target) != name {
			continue
		}
		if !rewriteFakeIPHints(binding, host, fakePool, fakePool6) {
			continue
		}
		useTTL(&binding.Hdr)
		binding.Hdr.Name = q.Name
		binding.Target = "."
		answer = append(answer, rr)
	}
	if len(answer) == 0 {
		return nil, 0
	}
	return answer, ttl
}

// rewriteFakeIPHints points ipv4hint/ipv6hint at the fake IPs of host. Hints of
// a family without a fake-ip pool are removed; if such a hint is mandatory the
// record cannot be kept as published and false is returned.
func rewriteFakeIPHints(rr *D.SVCB, host string, fakePool, fakePool6 *fakeip.Pool) bool {
	for _, param := range rr.Value {
		if mandatory, ok := param.(*D.SVCBMandatory); ok {
			for _, key := range mandatory.Code {
				if key == D.SVCB_IPV4HINT && fakePool == nil || key == D.SVCB_IPV6HINT && fakePool6 == nil {
					return false
				}
			}
		}
	}
	rr.Value = slices.DeleteFunc(rr.Value, func(param D.SVCBKeyValue) bool {
		switch hint := param.(type) {
		case *D.SVCBIPv4Hint:
			if fakePool == nil {
				return true
			}
			hint.Hint = []net.IP{fakePool.Lookup(host).AsSlice()}
		case *D.SVCBIPv6Hint:
			if fakePool6 == nil {
				return true
			}
			hint.Hint = []net.IP{fakePool6.Lookup(host).AsSlice()}
		}
		return false
	})
	return true
}

func withResolver(resolver resolver.Resolver, ipv6 bool) handler {
	return func(ctx *icontext.DNSContext, r *D.Msg) (*D.Msg, error) {
		ctx.SetType(icontext.DNSTypeRaw)

		q := r.Question[0]

		// return a empty AAAA msg when ipv6 disabled
		if !ipv6 && q.Qtype == D.TypeAAAA {
			return handleMsgWithEmptyAnswer(r), nil
		}

		msg, err := resolver.ExchangeContext(ctx, r)
		if err != nil {
			log.Debugln("[DNS Server] Exchange %s failed: %v", q.String(), err)
			return msg, err
		}
		msg.SetRcode(r, msg.Rcode)
		msg.Authoritative = true

		return msg, nil
	}
}

func compose(middlewares []middleware, endpoint handler) handler {
	length := len(middlewares)
	h := endpoint
	for i := length - 1; i >= 0; i-- {
		middleware := middlewares[i]
		h = middleware(h)
	}

	return h
}

func newHandler(resolver resolver.Resolver, mapper *ResolverEnhancer) handler {
	var middlewares []middleware

	if mapper.useHosts {
		middlewares = append(middlewares, withHosts(mapper.mapping))
	}

	if mapper.mode == C.DNSFakeIP {
		middlewares = append(middlewares, withFakeIP(mapper.fakeIPSkipper, mapper.fakeIPPool, mapper.fakeIPPool6, mapper.fakeIPTTL, mapper.fakeIPHTTPSWait))
	}

	if mapper.mode != C.DNSNormal {
		middlewares = append(middlewares, withMapping(mapper.mapping))
	}

	return compose(middlewares, withResolver(resolver, mapper.ipv6))
}

// trimAttrleaf strips underscore labels such as "_8443._https." (RFC 9460 port prefix naming).
func trimAttrleaf(name string) string {
	for strings.HasPrefix(name, "_") {
		i := strings.IndexByte(name, '.')
		if i < 0 {
			break
		}
		name = name[i+1:]
	}
	return name
}
