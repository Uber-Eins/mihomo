package dns

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"

	"github.com/metacubex/mihomo/component/fakeip"
	"github.com/metacubex/mihomo/component/trie"
	C "github.com/metacubex/mihomo/constant"
	icontext "github.com/metacubex/mihomo/context"

	D "github.com/miekg/dns"
	"github.com/stretchr/testify/require"
)

const fakeIPTestTTL = 120

func TestWithFakeIPHTTPSRewritesOwnerHint(t *testing.T) {
	const host = "example.com."
	answer := fakeIPTestRR(t, `example.com. 60 IN HTTPS 1 . alpn="h3,h2" no-default-alpn port=443 ipv4hint=203.0.113.10,203.0.113.11 ech="AAQAAQID" key65400="\000\255"`).(*D.HTTPS)
	upstream := fakeIPTestResponse(host, D.TypeHTTPS, answer)
	before, err := upstream.Pack()
	require.NoError(t, err)
	h := fakeIPTestHandler(&fakeip.Skipper{}, fakeIPTestPool(t, "198.18.0.0/24"), nil, upstream)

	got, responseType := fakeIPTestExchange(t, h, host, D.TypeHTTPS)
	a, _ := fakeIPTestExchange(t, h, host, D.TypeA)
	require.Equal(t, icontext.DNSTypeFakeIP, responseType)
	require.NotSame(t, upstream, got)
	require.Len(t, got.Answer, 1)
	rewritten := got.Answer[0].(*D.HTTPS)
	require.Equal(t, uint32(fakeIPTestTTL), rewritten.Hdr.Ttl)
	wantParams := append([]D.SVCBKeyValue(nil), answer.Value...)
	wantParams[3] = &D.SVCBIPv4Hint{Hint: []net.IP{a.Answer[0].(*D.A).A}}
	require.Equal(t, wantParams, rewritten.Value)
	after, err := upstream.Pack()
	require.NoError(t, err)
	require.Equal(t, before, after, "upstream response must not be mutated")
}

func TestWithFakeIPSVCBRewritesTargetHints(t *testing.T) {
	const host, target = "example.com.", "endpoint.example.net."
	answer := fakeIPTestRR(t, `example.com. 60 IN SVCB 1 endpoint.example.net. ipv4hint=203.0.113.10 ipv6hint=2001:db8::1`)
	pool := fakeIPTestPool(t, "198.18.0.0/24")
	pool6 := fakeIPTestPool(t, "fd00::/120")
	h := fakeIPTestHandler(&fakeip.Skipper{}, pool, pool6, fakeIPTestResponse(host, D.TypeSVCB, answer))

	got, _ := fakeIPTestExchange(t, h, host, D.TypeSVCB)
	a, _ := fakeIPTestExchange(t, h, target, D.TypeA)
	aaaa, _ := fakeIPTestExchange(t, h, target, D.TypeAAAA)
	params := got.Answer[0].(*D.SVCB).Value
	require.Equal(t, a.Answer[0].(*D.A).A, params[0].(*D.SVCBIPv4Hint).Hint[0])
	require.Equal(t, aaaa.Answer[0].(*D.AAAA).AAAA, params[1].(*D.SVCBIPv6Hint).Hint[0])
	require.NotEqual(t, net.IP(pool.Lookup("example.com").AsSlice()), params[0].(*D.SVCBIPv4Hint).Hint[0])
}

func TestWithFakeIPHTTPSOwnerHintFollowsQueriedName(t *testing.T) {
	pool := fakeIPTestPool(t, "198.18.0.0/24")
	for _, tc := range []struct {
		name, query, want string
		answers           []string
	}{
		{"cname", "www.example.com.", "www.example.com.", []string{
			`www.example.com. 60 IN CNAME edge.cdn.example.net.`,
			`edge.cdn.example.net. 60 IN HTTPS 1 . ipv4hint=203.0.113.10`,
		}},
		{"port prefix", "_8443._https.example.com.", "example.com.", []string{
			`_8443._https.example.com. 60 IN HTTPS 1 . port=8443 ipv4hint=203.0.113.10`,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var answers []D.RR
			for _, rr := range tc.answers {
				answers = append(answers, fakeIPTestRR(t, rr))
			}
			h := fakeIPTestHandler(&fakeip.Skipper{}, pool, nil, fakeIPTestResponse(tc.query, D.TypeHTTPS, answers...))
			got, _ := fakeIPTestExchange(t, h, tc.query, D.TypeHTTPS)
			a, _ := fakeIPTestExchange(t, h, tc.want, D.TypeA)
			https := got.Answer[len(got.Answer)-1].(*D.HTTPS)
			require.Equal(t, []net.IP{a.Answer[0].(*D.A).A}, https.Value[len(https.Value)-1].(*D.SVCBIPv4Hint).Hint)
		})
	}
}

func TestWithFakeIPHTTPSRemovesUnavailableHintsFromMandatory(t *testing.T) {
	const host = "example.com."
	upstream := fakeIPTestResponse(host, D.TypeHTTPS,
		fakeIPTestRR(t, `example.com. 60 IN HTTPS 1 . mandatory=ipv6hint,alpn alpn=h2 ipv6hint=2001:db8::1`),
		fakeIPTestRR(t, `example.com. 60 IN HTTPS 2 . mandatory=ipv6hint ipv6hint=2001:db8::2`),
	)
	h := fakeIPTestHandler(&fakeip.Skipper{}, fakeIPTestPool(t, "198.18.0.0/24"), nil, upstream)

	got, responseType := fakeIPTestExchange(t, h, host, D.TypeHTTPS)
	require.Equal(t, icontext.DNSTypeFakeIP, responseType)
	require.Equal(t, []D.SVCBKeyValue{
		&D.SVCBMandatory{Code: []D.SVCBKey{D.SVCB_ALPN}},
		&D.SVCBAlpn{Alpn: []string{"h2"}},
	}, got.Answer[0].(*D.HTTPS).Value)
	require.Empty(t, got.Answer[1].(*D.HTTPS).Value)
	require.Equal(t, []D.SVCBKey{D.SVCB_IPV6HINT, D.SVCB_ALPN}, upstream.Answer[0].(*D.HTTPS).Value[0].(*D.SVCBMandatory).Code)
}

func TestWithFakeIPHTTPSRemovesGlueAndInvalidatedSignatures(t *testing.T) {
	const host = "example.com."
	alias := fakeIPTestRR(t, `example.com. 60 IN HTTPS 0 svc.example.net.`).(*D.HTTPS)
	service := fakeIPTestRR(t, `svc.example.net. 60 IN HTTPS 1 . ipv4hint=203.0.113.10`)
	sig := func(name string, covered uint16) *D.RRSIG {
		return &D.RRSIG{Hdr: D.RR_Header{Name: name, Rrtype: D.TypeRRSIG, Class: D.ClassINET, Ttl: 60}, TypeCovered: covered}
	}
	upstream := fakeIPTestResponse(host, D.TypeHTTPS, alias, service, sig(host, D.TypeHTTPS), sig("SVC.example.net.", D.TypeHTTPS))
	upstream.AuthenticatedData = true
	upstream.Extra = []D.RR{
		sig("svc.example.net.", D.TypeHTTPS),
		fakeIPTestRR(t, `svc.example.net. 60 IN A 203.0.113.10`),
		fakeIPTestRR(t, `svc.example.net. 60 IN AAAA 2001:db8::1`),
		sig("svc.example.net.", D.TypeA),
		fakeIPTestRR(t, `example.com. 60 IN TXT "keep"`),
	}
	upstream.SetEdns0(1232, true)
	h := fakeIPTestHandler(&fakeip.Skipper{}, fakeIPTestPool(t, "198.18.0.0/24"), nil, upstream)

	got, _ := fakeIPTestExchange(t, h, host, D.TypeHTTPS)
	require.False(t, got.AuthenticatedData)
	require.True(t, upstream.AuthenticatedData)
	require.Len(t, got.Answer, 3)
	wantAlias := D.Copy(alias)
	wantAlias.Header().Ttl = fakeIPTestTTL
	require.Equal(t, wantAlias, got.Answer[0])
	a, _ := fakeIPTestExchange(t, h, "svc.example.net.", D.TypeA)
	require.Equal(t, []net.IP{a.Answer[0].(*D.A).A}, got.Answer[1].(*D.HTTPS).Value[0].(*D.SVCBIPv4Hint).Hint)
	require.Equal(t, host, got.Answer[2].(*D.RRSIG).Hdr.Name)
	require.Len(t, got.Extra, 2)
	require.Equal(t, []string{"keep"}, got.Extra[0].(*D.TXT).Txt)
	require.Equal(t, D.Copy(upstream.IsEdns0()), got.IsEdns0())
}

func TestWithFakeIPHTTPSPassesThroughUnchangedResponses(t *testing.T) {
	var builder trie.DomainSetBuilder
	require.NoError(t, builder.Insert("skipped.example.net"))
	skipper := &fakeip.Skipper{Host: []C.DomainMatcher{builder.Build()}}
	pool := fakeIPTestPool(t, "198.18.0.0/24")

	for _, tc := range []struct {
		name, host, rr string
	}{
		{"skipped query", "skipped.example.net.", `skipped.example.net. 60 IN HTTPS 1 . ipv4hint=203.0.113.10`},
		{"skipped target", "example.com.", `example.com. 60 IN HTTPS 1 skipped.example.net. ipv4hint=203.0.113.10`},
		{"alias mode", "example.com.", `example.com. 60 IN HTTPS 0 target.example.net. ipv4hint=203.0.113.10`},
		{"no hints", "example.com.", `example.com. 60 IN HTTPS 1 . alpn=h2 ech="AAQAAQID"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := fakeIPTestResponse(tc.host, D.TypeHTTPS, fakeIPTestRR(t, tc.rr))
			upstream.AuthenticatedData = true
			if tc.name == "skipped query" {
				upstream.Extra = []D.RR{fakeIPTestRR(t, `skipped.example.net. 60 IN A 203.0.113.10`)}
			}
			h := fakeIPTestHandler(skipper, pool, nil, upstream)
			got, responseType := fakeIPTestExchange(t, h, tc.host, D.TypeHTTPS)
			require.Same(t, upstream, got)
			require.True(t, got.AuthenticatedData)
			require.Equal(t, uint32(60), got.Answer[0].Header().Ttl)
			require.Equal(t, icontext.DNSTypeRaw, responseType)
		})
	}
}

func TestWithFakeIPHTTPSReturnsUpstreamError(t *testing.T) {
	wantErr := errors.New("upstream failed")
	upstream := fakeIPTestResponse("example.com.", D.TypeHTTPS)
	h := withFakeIP(&fakeip.Skipper{}, nil, nil, fakeIPTestTTL)(func(_ *icontext.DNSContext, _ *D.Msg) (*D.Msg, error) {
		return upstream, wantErr
	})
	got, err := h(icontext.NewDNSContext(context.Background()), new(D.Msg).SetQuestion("example.com.", D.TypeHTTPS))
	require.Same(t, wantErr, err)
	require.Same(t, upstream, got)
}

func fakeIPTestPool(t *testing.T, prefix string) *fakeip.Pool {
	t.Helper()
	pool, err := fakeip.New(fakeip.Options{IPNet: netip.MustParsePrefix(prefix), Size: 32})
	require.NoError(t, err)
	return pool
}

func fakeIPTestRR(t *testing.T, text string) D.RR {
	t.Helper()
	rr, err := D.NewRR(text)
	require.NoError(t, err)
	return rr
}

func fakeIPTestResponse(host string, qtype uint16, answers ...D.RR) *D.Msg {
	msg := new(D.Msg).SetReply(new(D.Msg).SetQuestion(host, qtype))
	msg.Answer = answers
	return msg
}

func fakeIPTestHandler(skipper *fakeip.Skipper, pool, pool6 *fakeip.Pool, upstream *D.Msg) handler {
	return withFakeIP(skipper, pool, pool6, fakeIPTestTTL)(func(ctx *icontext.DNSContext, _ *D.Msg) (*D.Msg, error) {
		ctx.SetType(icontext.DNSTypeRaw)
		return upstream, nil
	})
}

func fakeIPTestExchange(t *testing.T, h handler, host string, qtype uint16) (*D.Msg, string) {
	t.Helper()
	ctx := icontext.NewDNSContext(context.Background())
	msg, err := h(ctx, new(D.Msg).SetQuestion(host, qtype))
	require.NoError(t, err)
	return msg, ctx.Type()
}
