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

func TestWithFakeIPHTTPSRewritesHints(t *testing.T) {
	answer := fakeIPTestRR(t, `example.com. 60 IN HTTPS 1 . alpn="h3,h2" no-default-alpn port=443 ipv4hint=203.0.113.10,203.0.113.11 ech="AAQAAQID" ipv6hint=2001:db8::1 key65400="\000\255"`).(*D.HTTPS)
	upstream := fakeIPTestResponse("example.com.", D.TypeHTTPS, answer)
	upstream.AuthenticatedData = true
	before, err := upstream.Pack()
	require.NoError(t, err)
	h := fakeIPTestHandler(nil, fakeIPTestPool(t, "198.18.0.0/24"), fakeIPTestPool(t, "fd00::/120"), fakeIPTestTTL, upstream)

	got, responseType := fakeIPTestExchange(t, h, "example.com.", D.TypeHTTPS)
	a, _ := fakeIPTestExchange(t, h, "example.com.", D.TypeA)
	aaaa, _ := fakeIPTestExchange(t, h, "example.com.", D.TypeAAAA)
	require.Equal(t, icontext.DNSTypeFakeIP, responseType)
	require.False(t, got.AuthenticatedData)
	require.Len(t, got.Answer, 1)
	https := got.Answer[0].(*D.HTTPS)
	require.Equal(t, uint32(60), https.Hdr.Ttl, "upstream TTL is lower than fake-ip-ttl")
	want := append([]D.SVCBKeyValue(nil), answer.Value...)
	want[3] = &D.SVCBIPv4Hint{Hint: []net.IP{a.Answer[0].(*D.A).A}}
	want[5] = &D.SVCBIPv6Hint{Hint: []net.IP{aaaa.Answer[0].(*D.AAAA).AAAA}}
	require.Equal(t, want, https.Value)

	after, err := upstream.Pack()
	require.NoError(t, err)
	require.Equal(t, before, after, "upstream response must not be mutated")
}

func TestWithFakeIPHTTPSTTLCappedByFakeIPTTL(t *testing.T) {
	upstream := fakeIPTestResponse("example.com.", D.TypeHTTPS, fakeIPTestRR(t, `example.com. 3600 IN HTTPS 1 . alpn=h2`))
	h := fakeIPTestHandler(nil, fakeIPTestPool(t, "198.18.0.0/24"), nil, 5, upstream)
	got, _ := fakeIPTestExchange(t, h, "example.com.", D.TypeHTTPS)
	require.Equal(t, uint32(5), got.Answer[0].Header().Ttl)
}

func TestWithFakeIPHTTPSCollapsesCNAME(t *testing.T) {
	pool := fakeIPTestPool(t, "198.18.0.0/24")
	for _, target := range []string{".", "edge.cdn.example.net."} {
		t.Run(target, func(t *testing.T) {
			upstream := fakeIPTestResponse("www.example.com.", D.TypeHTTPS,
				fakeIPTestRR(t, `www.example.com. 30 IN CNAME edge.cdn.example.net.`),
				fakeIPTestRR(t, `edge.cdn.example.net. 300 IN HTTPS 1 `+target+` ipv4hint=203.0.113.10 ech="AAQAAQID"`),
			)
			h := fakeIPTestHandler(nil, pool, nil, fakeIPTestTTL, upstream)
			got, _ := fakeIPTestExchange(t, h, "www.example.com.", D.TypeHTTPS)
			a, _ := fakeIPTestExchange(t, h, "www.example.com.", D.TypeA)
			require.Len(t, got.Answer, 1)
			https := got.Answer[0].(*D.HTTPS)
			require.Equal(t, "www.example.com.", https.Hdr.Name)
			require.Equal(t, ".", https.Target)
			require.Equal(t, uint32(30), https.Hdr.Ttl, "TTL includes the CNAME chain")
			require.Equal(t, []net.IP{a.Answer[0].(*D.A).A}, https.Value[0].(*D.SVCBIPv4Hint).Hint)
			require.Equal(t, []byte{0, 4, 0, 1, 2, 3}, https.Value[1].(*D.SVCBECHConfig).ECH)
		})
	}
}

func TestWithFakeIPHTTPSDropsOtherTargets(t *testing.T) {
	upstream := fakeIPTestResponse("example.com.", D.TypeHTTPS,
		fakeIPTestRR(t, `example.com. 60 IN HTTPS 1 shared-cdn.example.net. ipv4hint=203.0.113.10`),
		fakeIPTestRR(t, `example.com. 60 IN HTTPS 2 . alpn=h2`),
	)
	h := fakeIPTestHandler(nil, fakeIPTestPool(t, "198.18.0.0/24"), nil, fakeIPTestTTL, upstream)
	got, _ := fakeIPTestExchange(t, h, "example.com.", D.TypeHTTPS)
	require.Len(t, got.Answer, 1)
	require.Equal(t, uint16(2), got.Answer[0].(*D.HTTPS).Priority)
}

func TestWithFakeIPHTTPSAnswersNODATA(t *testing.T) {
	for _, tc := range []struct {
		name    string
		answers []string
	}{
		{"alias mode drops whole rrset", []string{
			`example.com. 60 IN HTTPS 0 svc.example.net.`,
			`example.com. 60 IN HTTPS 1 . ipv4hint=203.0.113.10`,
		}},
		{"only other targets", []string{
			`example.com. 60 IN HTTPS 1 shared-cdn.example.net. ipv4hint=203.0.113.10`,
		}},
		{"cname without https", []string{
			`example.com. 60 IN CNAME edge.example.net.`,
		}},
		{"cname loop", []string{
			`example.com. 60 IN CNAME a.example.net.`,
			`a.example.net. 60 IN CNAME example.com.`,
			`example.com. 60 IN HTTPS 1 . alpn=h2`,
		}},
		{"no answer", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var answers []D.RR
			for _, rr := range tc.answers {
				answers = append(answers, fakeIPTestRR(t, rr))
			}
			upstream := fakeIPTestResponse("example.com.", D.TypeHTTPS, answers...)
			upstream.Ns = []D.RR{fakeIPTestRR(t, `example.net. 60 IN SOA ns.example.net. host.example.net. 1 7200 3600 1209600 60`)}
			h := fakeIPTestHandler(nil, fakeIPTestPool(t, "198.18.0.0/24"), nil, fakeIPTestTTL, upstream)
			got, responseType := fakeIPTestExchange(t, h, "example.com.", D.TypeHTTPS)
			fakeIPTestRequireNODATA(t, got)
			require.Equal(t, icontext.DNSTypeFakeIP, responseType)
		})
	}
}

func TestWithFakeIPHTTPSUpstreamFailures(t *testing.T) {
	for _, tc := range []struct {
		name      string
		rcode     int
		wantRcode int
	}{
		{"servfail", D.RcodeServerFailure, D.RcodeServerFailure},
		{"refused", D.RcodeRefused, D.RcodeServerFailure},
		{"nxdomain", D.RcodeNameError, D.RcodeSuccess},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := fakeIPTestResponse("example.com.", D.TypeHTTPS, fakeIPTestRR(t, `example.com. 60 IN HTTPS 1 . alpn=h2`))
			upstream.Rcode = tc.rcode
			upstream.AuthenticatedData = true
			h := fakeIPTestHandler(nil, fakeIPTestPool(t, "198.18.0.0/24"), nil, fakeIPTestTTL, upstream)
			got, _ := fakeIPTestExchange(t, h, "example.com.", D.TypeHTTPS)
			require.Equal(t, tc.wantRcode, got.Rcode)
			require.False(t, got.AuthenticatedData)
			require.Empty(t, got.Answer)
			require.Empty(t, got.Ns)
			require.Empty(t, got.Extra)
		})
	}

	t.Run("exchange error", func(t *testing.T) {
		wantErr := errors.New("upstream failed")
		h := withFakeIP(&fakeip.Skipper{}, fakeIPTestPool(t, "198.18.0.0/24"), nil, fakeIPTestTTL)(func(_ *icontext.DNSContext, _ *D.Msg) (*D.Msg, error) {
			return nil, wantErr
		})
		_, err := h(icontext.NewDNSContext(context.Background()), new(D.Msg).SetQuestion("example.com.", D.TypeHTTPS))
		require.Same(t, wantErr, err)
	})
}

func TestWithFakeIPHTTPSMandatoryHintWithoutPool(t *testing.T) {
	mandatory := fakeIPTestRR(t, `example.com. 60 IN HTTPS 1 . mandatory=ipv6hint,alpn alpn=h2 ipv6hint=2001:db8::1 ech="AAQAAQID"`)
	optional := fakeIPTestRR(t, `example.com. 60 IN HTTPS 2 . mandatory=alpn alpn=h2 ipv4hint=203.0.113.10 ipv6hint=2001:db8::2`)
	pool := fakeIPTestPool(t, "198.18.0.0/24")

	h := fakeIPTestHandler(nil, pool, nil, fakeIPTestTTL, fakeIPTestResponse("example.com.", D.TypeHTTPS, mandatory, optional))
	got, _ := fakeIPTestExchange(t, h, "example.com.", D.TypeHTTPS)
	a, _ := fakeIPTestExchange(t, h, "example.com.", D.TypeA)
	require.Len(t, got.Answer, 1, "record with a mandatory ipv6hint is excluded")
	require.Equal(t, []D.SVCBKeyValue{
		&D.SVCBMandatory{Code: []D.SVCBKey{D.SVCB_ALPN}},
		&D.SVCBAlpn{Alpn: []string{"h2"}},
		&D.SVCBIPv4Hint{Hint: []net.IP{a.Answer[0].(*D.A).A}},
	}, got.Answer[0].(*D.HTTPS).Value, "optional ipv6hint is removed, mandatory untouched")

	h = fakeIPTestHandler(nil, pool, nil, fakeIPTestTTL, fakeIPTestResponse("example.com.", D.TypeHTTPS, mandatory))
	got, _ = fakeIPTestExchange(t, h, "example.com.", D.TypeHTTPS)
	fakeIPTestRequireNODATA(t, got)
}

func TestWithFakeIPHTTPSSynthesizesCleanResponse(t *testing.T) {
	upstream := fakeIPTestResponse("example.com.", D.TypeHTTPS,
		fakeIPTestRR(t, `example.com. 60 IN HTTPS 1 . ipv4hint=203.0.113.10`),
		&D.RRSIG{Hdr: D.RR_Header{Name: "example.com.", Rrtype: D.TypeRRSIG, Class: D.ClassINET, Ttl: 60}, TypeCovered: D.TypeHTTPS},
	)
	upstream.AuthenticatedData = true
	upstream.Ns = []D.RR{fakeIPTestRR(t, `example.com. 60 IN NSEC next.example.com. A HTTPS RRSIG NSEC`)}
	upstream.Extra = []D.RR{fakeIPTestRR(t, `example.com. 60 IN A 203.0.113.10`)}
	upstream.SetEdns0(1232, true)
	h := fakeIPTestHandler(nil, fakeIPTestPool(t, "198.18.0.0/24"), nil, fakeIPTestTTL, upstream)

	got, _ := fakeIPTestExchange(t, h, "example.com.", D.TypeHTTPS)
	require.False(t, got.AuthenticatedData)
	require.Len(t, got.Answer, 1)
	require.IsType(t, &D.HTTPS{}, got.Answer[0])
	require.Empty(t, got.Ns)
	require.Empty(t, got.Extra)
}

func TestWithFakeIPHTTPSPortPrefix(t *testing.T) {
	upstream := fakeIPTestResponse("_8443._https.example.com.", D.TypeHTTPS,
		fakeIPTestRR(t, `_8443._https.example.com. 60 IN HTTPS 1 . port=8443 ipv4hint=203.0.113.10`),
	)
	h := fakeIPTestHandler(nil, fakeIPTestPool(t, "198.18.0.0/24"), nil, fakeIPTestTTL, upstream)
	got, _ := fakeIPTestExchange(t, h, "_8443._https.example.com.", D.TypeHTTPS)
	a, _ := fakeIPTestExchange(t, h, "example.com.", D.TypeA)
	https := got.Answer[0].(*D.HTTPS)
	require.Equal(t, "_8443._https.example.com.", https.Hdr.Name)
	require.Equal(t, []net.IP{a.Answer[0].(*D.A).A}, https.Value[1].(*D.SVCBIPv4Hint).Hint)
}

func TestWithFakeIPHTTPSSkippedPassesThrough(t *testing.T) {
	var builder trie.DomainSetBuilder
	require.NoError(t, builder.Insert("skipped.example.net"))
	skipper := &fakeip.Skipper{Host: []C.DomainMatcher{builder.Build()}}

	for _, query := range []string{"skipped.example.net.", "_8443._https.skipped.example.net."} {
		t.Run(query, func(t *testing.T) {
			upstream := fakeIPTestResponse(query, D.TypeHTTPS, fakeIPTestRR(t, query+` 60 IN HTTPS 1 . ipv4hint=203.0.113.10`))
			upstream.AuthenticatedData = true
			h := fakeIPTestHandler(skipper, fakeIPTestPool(t, "198.18.0.0/24"), nil, fakeIPTestTTL, upstream)
			got, responseType := fakeIPTestExchange(t, h, query, D.TypeHTTPS)
			require.Same(t, upstream, got)
			require.Equal(t, icontext.DNSTypeRaw, responseType)
		})
	}
}

func fakeIPTestRequireNODATA(t *testing.T, msg *D.Msg) {
	t.Helper()
	require.Equal(t, D.RcodeSuccess, msg.Rcode)
	require.False(t, msg.AuthenticatedData)
	require.Empty(t, msg.Answer)
	require.Empty(t, msg.Ns)
	require.Empty(t, msg.Extra)
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

func fakeIPTestHandler(skipper *fakeip.Skipper, pool, pool6 *fakeip.Pool, ttl int, upstream *D.Msg) handler {
	if skipper == nil {
		skipper = &fakeip.Skipper{}
	}
	return withFakeIP(skipper, pool, pool6, ttl)(func(ctx *icontext.DNSContext, _ *D.Msg) (*D.Msg, error) {
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
