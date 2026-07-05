package realm

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEvaluate_DecisionMatrix(t *testing.T) {
	r := NewRegistrar()

	// 用一个肯定不可达的地址（保留文档地址段 + 不存在端口）模拟 L3 不可达。
	// 192.0.2.0/24 是 TEST-NET-1，路由黑洞。
	unreachable := "http://192.0.2.1:9/realm"

	d := r.Evaluate(unreachable, true, false)
	if d.Reachable {
		t.Skip("环境意外可达 TEST-NET-1，跳过")
	}
	// L3 不可达 → 绝不 reload（reload 也连不上）。
	if d.NeedReload {
		t.Errorf("L3 不可达时不应 reload, got reason=%s", d.Reason)
	}
	if d.Reason != "l3_unreachable_wait" {
		t.Errorf("unexpected reason: %s", d.Reason)
	}
}

// ★obs 探测器 bug 修复回归：自签 TLS 会合面(=de-01 的 otun-s-test https://ip:9443)。
//   insecure=false(验证证书)→ 假报不可达；insecure=true(rendezvous_insecure)→ 正确报可达。
//   这是 de-01 类出口 l3_reachable 恒假报 false 的根因，删 insecure 分支→本测试红。
func TestProbeL3_SelfSignedRendezvous(t *testing.T) {
	// httptest.NewTLSServer 用自签证书(同 otun-s-test 的 CN=otun-s-test 自签)。
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound) // 会合面对 / 返 404，任何 HTTP 响应=端点在线。
	}))
	defer srv.Close()

	r := NewRegistrar()

	// insecure=false：验证自签证书失败 → 假报不可达（这正是修复前 de-01 的症状）。
	if got := r.ProbeL3(srv.URL, false); got.Reachable {
		t.Errorf("verify-cert probe of self-signed server should be unreachable (cert fail), got reachable")
	}
	// insecure=true：跳过验证 → 正确报可达（修复后 de-01 应如此）。
	if got := r.ProbeL3(srv.URL, true); !got.Reachable {
		t.Errorf("insecure probe of self-signed server should be reachable, got unreachable")
	}
}

// 确认 insecure 客户端确实配了 skip-verify（防将来误删 Transport）。
func TestInsecureClientSkipsVerify(t *testing.T) {
	r := NewRegistrar()
	tr, ok := r.httpClientInsecure.Transport.(*http.Transport)
	if !ok || tr.TLSClientConfig == nil || !tr.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("httpClientInsecure must have InsecureSkipVerify=true")
	}
	// 正式客户端不得 skip（自签会合面之外仍验证）。
	if r.httpClient.Transport != nil {
		if tr2, ok := r.httpClient.Transport.(*http.Transport); ok && tr2.TLSClientConfig != nil && tr2.TLSClientConfig.InsecureSkipVerify {
			t.Fatal("httpClient (verify) must NOT skip verify")
		}
	}
	_ = tls.Config{}
}

func TestHostPortFromURL(t *testing.T) {
	cases := map[string]string{
		"https://situstechnologies.com/realm": "situstechnologies.com:443",
		"http://1.2.3.4:8080/x":               "1.2.3.4:8080",
		"situstechnologies.com":               "situstechnologies.com:443",
	}
	for in, want := range cases {
		if got := hostPortFromURL(in); got != want {
			t.Errorf("hostPortFromURL(%q) = %q, want %q", in, got, want)
		}
	}
}
