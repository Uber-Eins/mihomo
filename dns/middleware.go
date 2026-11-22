package dns

import (
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

func withFakeIP(skipper *fakeip.Skipper, fakePool *fakeip.Pool, fakePool6 *fakeip.Pool, fakeIPTTL int) middleware {
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
				msg, err := next(ctx, r)
				if err != nil {
					return msg, err
				}

				rewritten := msg.Copy()
				if rewriteFakeIPServiceBindings(rewritten, skipper, fakePool, fakePool6) {
					rewritten.AuthenticatedData = false
					setMsgTTL(rewritten, uint32(fakeIPTTL))
					ctx.SetType(icontext.DNSTypeFakeIP)
					return rewritten, nil
				}
				return msg, nil
			default:
				return next(ctx, r)
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

func rewriteFakeIPServiceBindings(msg *D.Msg, skipper *fakeip.Skipper, fakePool, fakePool6 *fakeip.Pool) bool {
	type rrsetKey struct {
		name    string
		rrtype  uint16
		rrclass uint16
	}
	keyOf := func(hdr *D.RR_Header, rrtype uint16) rrsetKey {
		return rrsetKey{D.CanonicalName(hdr.Name), rrtype, hdr.Class}
	}
	// "." targets refer to the name the client looked up, before any CNAME in this answer.
	cnameFrom := make(map[string]string)
	for _, rr := range msg.Answer {
		if cname, ok := rr.(*D.CNAME); ok {
			cnameFrom[D.CanonicalName(cname.Target)] = cname.Hdr.Name
		}
	}
	queriedName := func(name string) string {
		for range cnameFrom { // bounded, so CNAME loops terminate
			prev, ok := cnameFrom[D.CanonicalName(name)]
			if !ok {
				break
			}
			name = prev
		}
		return name
	}

	rewritten := make(map[rrsetKey]struct{})
	for _, rr := range msg.Answer {
		var binding *D.SVCB
		switch rr := rr.(type) {
		case *D.HTTPS:
			binding = &rr.SVCB
		case *D.SVCB:
			binding = rr
		default:
			continue
		}
		if rewriteFakeIPServiceBinding(binding, queriedName(binding.Hdr.Name), skipper, fakePool, fakePool6) {
			rewritten[keyOf(&binding.Hdr, binding.Hdr.Rrtype)] = struct{}{}
		}
	}

	msg.Extra = slices.DeleteFunc(msg.Extra, func(rr D.RR) bool {
		hdr := rr.Header()
		switch hdr.Rrtype {
		case D.TypeA, D.TypeAAAA:
			rewritten[keyOf(hdr, hdr.Rrtype)] = struct{}{}
			return true
		default:
			return false
		}
	})
	if len(rewritten) == 0 {
		return false
	}

	removeSignature := func(rr D.RR) bool {
		sig, ok := rr.(*D.RRSIG)
		if !ok {
			return false
		}
		_, ok = rewritten[keyOf(&sig.Hdr, sig.TypeCovered)]
		return ok
	}
	msg.Answer = slices.DeleteFunc(msg.Answer, removeSignature)
	msg.Extra = slices.DeleteFunc(msg.Extra, removeSignature)
	return true
}

// rewriteFakeIPServiceBinding points the IP hints of a ServiceMode record at
// the fake IPs of its endpoint, so they match the A/AAAA answers for that name.
// owner is the record owner with CNAMEs unwound; it is used for "." targets.
func rewriteFakeIPServiceBinding(rr *D.SVCB, owner string, skipper *fakeip.Skipper, fakePool, fakePool6 *fakeip.Pool) bool {
	if rr.Priority == 0 {
		return false
	}
	endpoint := owner
	if rr.Target != "." {
		endpoint = rr.Target
	}
	endpoint = trimAttrleaf(strings.TrimRight(endpoint, "."))
	if skipper.ShouldSkipped(endpoint) {
		return false
	}

	var changed, removedIPv4Hint, removedIPv6Hint bool
	rr.Value = slices.DeleteFunc(rr.Value, func(param D.SVCBKeyValue) bool {
		switch hint := param.(type) {
		case *D.SVCBIPv4Hint:
			changed = true
			if fakePool == nil {
				removedIPv4Hint = true
				return true
			}
			hint.Hint = []net.IP{fakePool.Lookup(endpoint).AsSlice()}
		case *D.SVCBIPv6Hint:
			changed = true
			if fakePool6 == nil {
				removedIPv6Hint = true
				return true
			}
			hint.Hint = []net.IP{fakePool6.Lookup(endpoint).AsSlice()}
		}
		return false
	})
	if removedIPv4Hint || removedIPv6Hint {
		rr.Value = slices.DeleteFunc(rr.Value, func(param D.SVCBKeyValue) bool {
			mandatory, ok := param.(*D.SVCBMandatory)
			if !ok {
				return false
			}
			mandatory.Code = slices.DeleteFunc(mandatory.Code, func(key D.SVCBKey) bool {
				return key == D.SVCB_IPV4HINT && removedIPv4Hint || key == D.SVCB_IPV6HINT && removedIPv6Hint
			})
			return len(mandatory.Code) == 0
		})
	}
	return changed
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
		middlewares = append(middlewares, withFakeIP(mapper.fakeIPSkipper, mapper.fakeIPPool, mapper.fakeIPPool6, mapper.fakeIPTTL))
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
